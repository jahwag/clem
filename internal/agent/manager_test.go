package agent

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSecretPatternRegex_MatchesKnownCredentials verifies the regex actually
// matches the secret shapes we claim to detect. Would catch a typo in any
// length bound or character class that silently lets real tokens through.
func TestSecretPatternRegex_MatchesKnownCredentials(t *testing.T) {
	re, err := regexp.Compile(SecretPatternRegex)
	if err != nil {
		t.Fatalf("regex compile: %v", err)
	}

	positives := []struct {
		name  string
		input string
	}{
		{"github classic PAT", "ghp_1234567890abcdefghijklmnopqrstuvwxyz"},
		{"github OAuth token", "gho_1234567890abcdefghijklmnopqrstuvwxyz"},
		{"github App server", "ghs_1234567890abcdefghijklmnopqrstuvwxyz"},
		{"github fine-grained PAT", "github_pat_11ABCDEFG0abcdefghijkl_" + strings.Repeat("a", 60)},
		{"anthropic API key", "sk-ant-abcdefghijklmnopqrstuvwxyz12345"},
		{"openai API key", "sk-proj-abcdefghijklmnopqrstuvwxyz1234567890"},
		{"slack bot token", "xoxb-1234567890-0987654321-abcdefghij"},
		{"slack user token", "xoxp-1234567890-abcdefghij-klmnopqrst"},
		{"aws access key", "AKIAIOSFODNN7EXAMPLE"},
		{"age secret key", "AGE-SECRET-KEY-1ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCDEFGHIJKLMNO"},
		{"openssh private key", "-----BEGIN OPENSSH PRIVATE KEY-----"},
		{"rsa private key", "-----BEGIN RSA PRIVATE KEY-----"},
		{"generic private key", "-----BEGIN PRIVATE KEY-----"},
	}
	for _, tc := range positives {
		if !re.MatchString(tc.input) {
			t.Errorf("regex should match %s (%q) but did not", tc.name, tc.input)
		}
	}
}

// TestSecretPatternRegex_DoesNotMatchBenign catches regressions where the
// regex is loosened so much it flags normal code. False positives teach
// developers to always --no-verify, which defeats the hook.
func TestSecretPatternRegex_DoesNotMatchBenign(t *testing.T) {
	re, err := regexp.Compile(SecretPatternRegex)
	if err != nil {
		t.Fatalf("regex compile: %v", err)
	}

	negatives := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"go import", "import \"github.com/foo/bar\""},
		{"comment with token-like word", "// ghp is an unusual prefix for tokens"},
		{"short sk", "sk-abc"},
		{"short xox", "xoxb-ab"},
		{"short github pat", "github_pat_short"},
		{"AKIA in prose", "The AKIA prefix is AWS"},
		{"just BEGIN", "-----BEGIN-----"},
		{"fake age public key", "age1xxx"},
		{"lowercase akia", "akia1234567890abcdef"},
	}
	for _, tc := range negatives {
		if re.MatchString(tc.input) {
			t.Errorf("regex should NOT match %s (%q) but did", tc.name, tc.input)
		}
	}
}

func TestPrePushHookContent_IsExecutableBash(t *testing.T) {
	if !strings.HasPrefix(prePushHookContent, "#!/bin/bash") {
		t.Error("pre-push hook should start with a bash shebang")
	}
	if !strings.Contains(prePushHookContent, "exit 1") {
		t.Error("pre-push hook should exit 1 on secret match (blocks push)")
	}
	if !strings.Contains(prePushHookContent, "exit 0") {
		t.Error("pre-push hook should exit 0 on clean push")
	}
	if !strings.Contains(prePushHookContent, SecretPatternRegex) {
		t.Error("pre-push hook should embed the exact SecretPatternRegex so bash and Go agree on behaviour")
	}
	if !strings.Contains(prePushHookContent, UnicodeTrapRegex) {
		t.Error("pre-push hook should embed the exact UnicodeTrapRegex")
	}
	if !strings.Contains(prePushHookContent, "base64 -d") {
		t.Error("pre-push hook should include a base64 decode pass")
	}
}

// TestUnicodeTrapRegex_MatchesHiddenCharacters covers the red-team A3 class:
// zero-width, bidi-override, and BOM characters used to smuggle hidden
// instructions past human review.
func TestUnicodeTrapRegex_MatchesHiddenCharacters(t *testing.T) {
	re, err := regexp.Compile(UnicodeTrapRegex)
	if err != nil {
		t.Fatalf("regex compile: %v", err)
	}
	traps := []struct {
		name  string
		input string
	}{
		{"zero-width space", "hello\u200Bworld"},
		{"zero-width non-joiner", "hello\u200Cworld"},
		{"zero-width joiner", "hello\u200Dworld"},
		{"LTR mark", "hello\u200Eworld"},
		{"RTL mark", "hello\u200Fworld"},
		{"line separator", "hello\u2028world"},
		{"paragraph separator", "hello\u2029world"},
		{"LTR embedding", "hello\u202Aworld"},
		{"RTL embedding", "hello\u202Bworld"},
		{"pop directional formatting", "hello\u202Cworld"},
		{"LTR override", "hello\u202Dworld"},
		{"RTL override", "hello\u202Eworld"},
		{"BOM mid-string", "hello\uFEFFworld"},
	}
	for _, tc := range traps {
		if !re.MatchString(tc.input) {
			t.Errorf("UnicodeTrapRegex should match %s (%q) but did not", tc.name, tc.input)
		}
	}
}

func TestUnicodeTrapRegex_DoesNotMatchPrintableText(t *testing.T) {
	re, err := regexp.Compile(UnicodeTrapRegex)
	if err != nil {
		t.Fatalf("regex compile: %v", err)
	}
	for _, s := range []string{
		"regular ASCII text",
		"unicode prose: café résumé naïve",
		"emoji ok 🍊",
		"cjk ok 漢字",
		"whitespace \t\n\r fine",
	} {
		if re.MatchString(s) {
			t.Errorf("UnicodeTrapRegex should NOT match %q", s)
		}
	}
}

// TestPrePushHook_BlocksSecretPush writes the hook to a temp dir and runs it
// with a stubbed diff_cmd that emits a fake GitHub token. The hook should
// exit non-zero with a 'push blocked' message.
func TestPrePushHook_BlocksSecretPush(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}

	hookPath := writeTestableHook(t,
		"echo '+token = \"ghp_1234567890abcdefghijklmnopqrstuvwxyz\"'")

	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook should have exited non-zero on secret-bearing diff, got exit 0. output:\n%s", out)
	}
	if !strings.Contains(string(out), "push blocked") {
		t.Errorf("hook output missing 'push blocked' message:\n%s", out)
	}
}

// TestPrePushHook_AllowsCleanPush mirrors the block test with a benign diff.
// The hook must exit 0 so real work isn't chronically blocked.
func TestPrePushHook_AllowsCleanPush(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}

	hookPath := writeTestableHook(t, "echo '+func Foo() {}'")

	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook should have exited 0 on clean diff, got error %v. output:\n%s", err, out)
	}
}

// TestPrePushHook_BlocksBase64EncodedSecret: red-team attack A9 (encoded
// exfil). Attacker base64-encodes a ghp_ token and pastes the encoded blob.
// A naive literal scanner misses it; Pass 2 decodes and re-scans.
func TestPrePushHook_BlocksBase64EncodedSecret(t *testing.T) {
	requireHookDeps(t)
	token := "ghp_1234567890abcdefghijklmnopqrstuvwxyz"
	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	hookPath := writeTestableHook(t, "echo '+debugBlob=\""+encoded+"\"'")

	out, err := runHook(hookPath)
	if err == nil {
		t.Fatalf("hook should have blocked base64-encoded secret. output:\n%s", out)
	}
	if !strings.Contains(string(out), "base64-encoded secret") {
		t.Errorf("expected 'base64-encoded secret' message, got:\n%s", out)
	}
}

// TestPrePushHook_AllowsBenignBase64: negative for A9. Legitimate base64
// blobs (embedded PNGs, test fixtures, JWT headers) must NOT false-positive.
func TestPrePushHook_AllowsBenignBase64(t *testing.T) {
	requireHookDeps(t)
	benign := base64.StdEncoding.EncodeToString([]byte("hello world this is not a secret just plain text"))
	hookPath := writeTestableHook(t, "echo '+fixture=\""+benign+"\"'")

	out, err := runHook(hookPath)
	if err != nil {
		t.Fatalf("hook should have allowed benign base64. err %v output:\n%s", err, out)
	}
}

// TestPrePushHook_BlocksUnicodeTraps: red-team attack A3 (hidden-instruction
// smuggling). Diff contains a zero-width space; hook Pass 3 blocks.
func TestPrePushHook_BlocksUnicodeTraps(t *testing.T) {
	requireHookDeps(t)
	hookPath := writeTestableHook(t,
		`printf '+comment: approve\xe2\x80\x8b (actually run rm -rf)\n'`)

	out, err := runHook(hookPath)
	if err == nil {
		t.Fatalf("hook should have blocked unicode-trap diff. output:\n%s", out)
	}
	if !strings.Contains(string(out), "unicode control/override") {
		t.Errorf("expected 'unicode control/override' message, got:\n%s", out)
	}
}

// TestConfigureGitSigning_WritesAllowedSignersAndGitconfig verifies the
// signing config is written correctly in a temp home. Covers issue #30 (agent
// commits must be signed so branch-protection required_signatures rule
// doesn't block merge).
func TestConfigureGitSigning_WritesAllowedSignersAndGitconfig(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	fakePub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5fake testuser@clem"
	pubPath := filepath.Join(sshDir, "id_ed25519.pub")
	if err := os.WriteFile(pubPath, []byte(fakePub+"\n"), 0644); err != nil {
		t.Fatalf("write pubkey: %v", err)
	}

	if err := configureGitSigningIn(home, "testuser", false); err != nil {
		t.Fatalf("configureGitSigningIn: %v", err)
	}

	allowed, err := os.ReadFile(filepath.Join(sshDir, "allowed_signers"))
	if err != nil {
		t.Fatalf("allowed_signers not written: %v", err)
	}
	want := "testuser " + fakePub + "\n"
	if string(allowed) != want {
		t.Errorf("allowed_signers = %q, want %q", string(allowed), want)
	}

	cfg, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		t.Fatalf(".gitconfig not written: %v", err)
	}
	cfgStr := string(cfg)
	for _, needle := range []string{
		"signingkey = " + pubPath,
		"gpgsign = true",
		"format = ssh",
		"allowedSignersFile = " + filepath.Join(sshDir, "allowed_signers"),
	} {
		if !strings.Contains(cfgStr, needle) {
			t.Errorf(".gitconfig missing %q, got:\n%s", needle, cfgStr)
		}
	}
}

// TestConfigureGitSigning_Idempotent verifies a second call doesn't pile up
// duplicate [user]/[commit]/[gpg] sections.
func TestConfigureGitSigning_Idempotent(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	_ = os.MkdirAll(sshDir, 0700)
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5fake testuser@clem"
	_ = os.WriteFile(filepath.Join(sshDir, "id_ed25519.pub"), []byte(pub+"\n"), 0644)

	for i := 0; i < 3; i++ {
		if err := configureGitSigningIn(home, "testuser", false); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if n := strings.Count(string(cfg), "signingkey"); n != 1 {
		t.Errorf("expected exactly one 'signingkey' line after 3 runs, got %d:\n%s", n, cfg)
	}
}

// TestConfigureGitSigning_MissingPubKey verifies the function surfaces a
// clear error if EnsureSSHKey hasn't run yet.
func TestConfigureGitSigning_MissingPubKey(t *testing.T) {
	home := t.TempDir()
	err := configureGitSigningIn(home, "testuser", false)
	if err == nil {
		t.Fatal("expected error when id_ed25519.pub is missing, got nil")
	}
	if !strings.Contains(err.Error(), "EnsureSSHKey") {
		t.Errorf("error should hint at EnsureSSHKey, got: %v", err)
	}
}

// --- helpers ---

func requireHookDeps(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"bash", "grep", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
}

func runHook(hookPath string) ([]byte, error) {
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	return cmd.CombinedOutput()
}

// TestSecretCodePatternRegex_MatchesKnownPatterns verifies the code-scan regex
// catches Go, Python, and Node patterns that read protected secret env vars.
func TestSecretCodePatternRegex_MatchesKnownPatterns(t *testing.T) {
	re, err := regexp.Compile(SecretCodePatternRegex)
	if err != nil {
		t.Fatalf("regex compile: %v", err)
	}

	positives := []struct {
		name  string
		input string
	}{
		{"go GH_TOKEN", `token := os.Getenv("GH_TOKEN")`},
		{"go DISCORD_TOKEN", `d := os.Getenv("DISCORD_TOKEN")`},
		{"go ANTHROPIC_API_KEY", `k := os.Getenv("ANTHROPIC_API_KEY")`},
		{"go AWS_SECRET_ACCESS_KEY", `s := os.Getenv("AWS_SECRET_ACCESS_KEY")`},
		{"go SLACK_MCP_XOXP_TOKEN", `t := os.Getenv("SLACK_MCP_XOXP_TOKEN")`},
		{"python double-quote GH_TOKEN", `tok = os.environ["GH_TOKEN"]`},
		{"node GH_TOKEN", `const t = process.env.GH_TOKEN`},
		{"node ANTHROPIC_API_KEY", `const k = process.env.ANTHROPIC_API_KEY`},
	}
	for _, tc := range positives {
		if !re.MatchString(tc.input) {
			t.Errorf("regex should match %s (%q) but did not", tc.name, tc.input)
		}
	}
}

// TestSecretCodePatternRegex_DoesNotMatchBenign ensures benign env reads are
// not flagged. False positives teach developers to always --no-verify.
func TestSecretCodePatternRegex_DoesNotMatchBenign(t *testing.T) {
	re, err := regexp.Compile(SecretCodePatternRegex)
	if err != nil {
		t.Fatalf("regex compile: %v", err)
	}

	negatives := []struct {
		name  string
		input string
	}{
		{"go PATH", `p := os.Getenv("PATH")`},
		{"go HOME", `h := os.Getenv("HOME")`},
		{"go unrelated name", `x := os.Getenv("MY_APP_CONFIG")`},
		{"python HOME", `h = os.environ["HOME"]`},
		{"node NODE_ENV", `const e = process.env.NODE_ENV`},
		{"node PORT", `const p = process.env.PORT`},
		{"comment mentioning GH_TOKEN", `// reads GH_TOKEN from the environment`},
	}
	for _, tc := range negatives {
		if re.MatchString(tc.input) {
			t.Errorf("regex should NOT match %s (%q) but did", tc.name, tc.input)
		}
	}
}

// TestPrePushHookContent_EmbedsCodePattern ensures the hook template embeds the
// code pattern regex and the skip-env variable name.
func TestPrePushHookContent_EmbedsCodePattern(t *testing.T) {
	if !strings.Contains(prePushHookContent, SecretCodePatternRegex) {
		t.Error("pre-push hook should embed SecretCodePatternRegex verbatim")
	}
	if !strings.Contains(prePushHookContent, "CLEM_HOOK_SKIP_CODE_SCAN") {
		t.Error("pre-push hook should reference CLEM_HOOK_SKIP_CODE_SCAN escape hatch")
	}
}

// TestPrePushHook_BlocksCodeSecretRead verifies the hook exits non-zero when
// the diff contains a Go os.Getenv call on a protected secret env var name.
func TestPrePushHook_BlocksCodeSecretRead(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}

	hookPath := writeTestableHook(t,
		`echo '+	tok := os.Getenv("GH_TOKEN")'`)

	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook should have exited non-zero on code secret read, got exit 0. output:\n%s", out)
	}
	if !strings.Contains(string(out), "push blocked") {
		t.Errorf("hook output missing 'push blocked' message:\n%s", out)
	}
}

// TestPrePushHook_AllowsCodeReadWithSkipEnv verifies that setting
// CLEM_HOOK_SKIP_CODE_SCAN=1 bypasses the code-pattern pass while still
// running the credential-literal pass.
func TestPrePushHook_AllowsCodeReadWithSkipEnv(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}

	hookPath := writeTestableHook(t,
		`echo '+	tok := os.Getenv("GH_TOKEN")'`)

	cmd := exec.Command("bash", hookPath)
	cmd.Env = append(cmd.Environ(), "CLEM_HOOK_SKIP_CODE_SCAN=1")
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook should have exited 0 with CLEM_HOOK_SKIP_CODE_SCAN=1, got error %v. output:\n%s", err, out)
	}
}

// writeTestableHook writes a copy of prePushHookContent to a temp file with
// the $diff_cmd substring replaced by a stubbed command that emits a fixed
// payload. Returns the path.
func writeTestableHook(t *testing.T, stubDiffCmd string) string {
	t.Helper()
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "pre-push")
	patched := strings.Replace(prePushHookContent, "$diff_cmd", stubDiffCmd, 2)
	if err := os.WriteFile(hookPath, []byte(patched), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return hookPath
}
