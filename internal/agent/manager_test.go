package agent

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stubExec records invocations and returns canned responses. Replaces sys in tests
// to avoid requiring root, real OS users, or system binaries.
type stubExec struct {
	calls   [][]string
	failOn  string // if non-empty, return error when command name matches
	failOut []byte
}

func (s *stubExec) Run(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	s.calls = append(s.calls, call)
	if s.failOn != "" && name == s.failOn {
		return s.failOut, errors.New("stub: forced failure")
	}
	return nil, nil
}

// withStub replaces the package-level sys executor with stub for the duration
// of the test, restoring the original on cleanup.
func withStub(t *testing.T) *stubExec {
	t.Helper()
	stub := &stubExec{}
	orig := sys
	sys = stub
	t.Cleanup(func() { sys = orig })
	return stub
}

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
		t.Error("pre-push hook should embed UnicodeTrapRegex (Pass 3, red-team A3)")
	}
	if !strings.Contains(prePushHookContent, "base64 -d") {
		t.Error("pre-push hook should include base64 decode pass (Pass 2, red-team A9)")
	}
}

// TestUnicodeTrapRegex_MatchesHiddenCharacters covers red-team A3:
// zero-width, bidi-override, and BOM chars used to smuggle hidden
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

// TestPrePushHook_BlocksBase64EncodedSecret: red-team A9. Attacker
// base64-encodes a ghp_ token; literal scanner misses the prefix. Pass 2
// decodes and re-scans.
func TestPrePushHook_BlocksBase64EncodedSecret(t *testing.T) {
	for _, bin := range []string{"bash", "grep", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	// Construct the base64 of a fake GitHub PAT.
	token := "ghp_1234567890abcdefghijklmnopqrstuvwxyz"
	// Go's encoding/base64 is imported at package level in other tests - use
	// the /usr/bin/base64 binary here to keep this test self-contained.
	encodedCmd := exec.Command("bash", "-c", "echo -n "+token+" | base64")
	encodedOut, err := encodedCmd.Output()
	if err != nil {
		t.Fatalf("base64 encode fixture: %v", err)
	}
	encoded := strings.TrimSpace(string(encodedOut))

	hookPath := writeTestableHook(t, "echo '+debugBlob=\""+encoded+"\"'")
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook should have blocked base64-encoded secret, got exit 0. output:\n%s", out)
	}
	if !strings.Contains(string(out), "base64-encoded secret") {
		t.Errorf("expected 'base64-encoded secret' message, got:\n%s", out)
	}
}

// TestPrePushHook_AllowsBenignBase64: negative case for A9. Legitimate
// base64 (embedded PNG, JWT header, test fixture) must NOT false-positive.
func TestPrePushHook_AllowsBenignBase64(t *testing.T) {
	for _, bin := range []string{"bash", "grep", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	encodedCmd := exec.Command("bash", "-c", "echo -n 'hello world benign fixture string' | base64")
	encodedOut, err := encodedCmd.Output()
	if err != nil {
		t.Fatalf("base64 encode fixture: %v", err)
	}
	encoded := strings.TrimSpace(string(encodedOut))

	hookPath := writeTestableHook(t, "echo '+fixture=\""+encoded+"\"'")
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook should have allowed benign base64, got exit err %v. output:\n%s", err, out)
	}
}

// TestPrePushHook_BlocksUnicodeTraps: red-team A3 end-to-end. Diff contains
// a zero-width space; Pass 3 blocks.
func TestPrePushHook_BlocksUnicodeTraps(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	// printf emits the literal U+200B bytes (UTF-8: e2 80 8b).
	hookPath := writeTestableHook(t,
		`printf '+comment: approve\xe2\x80\x8b (actually run rm -rf)\n'`)
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook should have blocked unicode-trap diff, got exit 0. output:\n%s", out)
	}
	if !strings.Contains(string(out), "unicode control/override") {
		t.Errorf("expected 'unicode control/override' message, got:\n%s", out)
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

// --- Executor seam tests ---

func TestProjectFromUsername(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"myproject-ada", "myproject"},
		{"clem-dev-ada", "clem-dev"},
		{"nohyphen", "nohyphen"},
		{"", ""},
	}
	for _, tc := range cases {
		got := projectFromUsername(tc.in)
		if got != tc.want {
			t.Errorf("projectFromUsername(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTokenExpiry_MissingFile(t *testing.T) {
	dir := t.TempDir()
	expiry := TokenExpiry(dir)
	if !expiry.IsZero() {
		t.Errorf("expected zero time for missing credentials, got %v", expiry)
	}
}

func TestTokenExpiry_ValidCredentials(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}

	// expiresAt is milliseconds since epoch
	wantExpiry := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ms := wantExpiry.UnixMilli()
	creds := map[string]any{
		"claudeAiOauth": map[string]any{"expiresAt": ms},
	}
	data, _ := json.Marshal(creds)
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	got := TokenExpiry(dir)
	if !got.Equal(wantExpiry) {
		t.Errorf("TokenExpiry = %v, want %v", got, wantExpiry)
	}
}

func TestNeedsLogin_NoToken(t *testing.T) {
	dir := t.TempDir()
	if !NeedsLogin(dir) {
		t.Error("NeedsLogin should return true when no credentials file exists")
	}
}

func TestNeedsLogin_ValidToken(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(30 * 24 * time.Hour)
	creds := map[string]any{
		"claudeAiOauth": map[string]any{"expiresAt": future.UnixMilli()},
	}
	data, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), data, 0600) //nolint:errcheck
	if NeedsLogin(dir) {
		t.Error("NeedsLogin should return false when token is valid for 30 days")
	}
}

func TestWriteSettings_WritesExpectedFiles(t *testing.T) {
	stub := withStub(t)
	dir := t.TempDir()
	username := "testuser"

	if err := WriteSettings(username, dir, ""); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}

	// settings.json must exist and contain the trust flags
	settingsData, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	if !strings.Contains(string(settingsData), "hasTrustDialogAccepted") {
		t.Errorf("settings.json missing hasTrustDialogAccepted: %s", settingsData)
	}
	if !strings.Contains(string(settingsData), `"includeCoAuthoredBy": false`) {
		t.Errorf("settings.json missing includeCoAuthoredBy=false: %s", settingsData)
	}
	// Empty effort => no effortLevel field rendered.
	if strings.Contains(string(settingsData), "effortLevel") {
		t.Errorf("settings.json should omit effortLevel when effort empty: %s", settingsData)
	}

	// .claude.json must exist and contain the project trust entry
	appStateData, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatalf(".claude.json not written: %v", err)
	}
	if !strings.Contains(string(appStateData), "hasCompletedOnboarding") {
		t.Errorf(".claude.json missing hasCompletedOnboarding: %s", appStateData)
	}

	// ChownPath was called (best-effort; stub records it without failing)
	if len(stub.calls) == 0 {
		t.Error("expected ChownPath to invoke sys.Run at least once")
	}
	_ = stub
}

func TestWriteSettings_RendersEffortLevel(t *testing.T) {
	withStub(t)
	dir := t.TempDir()

	if err := WriteSettings("testuser", dir, "low"); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	settingsData, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	if !strings.Contains(string(settingsData), `"effortLevel": "low"`) {
		t.Errorf("settings.json missing effortLevel=low: %s", settingsData)
	}
}

func TestEnsureUser_AlreadyExists(t *testing.T) {
	stub := withStub(t) // "id" returns nil error → user exists
	if err := EnsureUser("existinguser"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	// Only "id" should have been called — no "useradd"
	if len(stub.calls) != 1 || stub.calls[0][0] != "id" {
		t.Errorf("expected only 'id' call, got %v", stub.calls)
	}
}

func TestEnsureUser_CreateNew(t *testing.T) {
	stub := withStub(t)
	stub.failOn = "id" // "id" fails → user does not exist → useradd is called
	if err := EnsureUser("newuser"); err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if len(stub.calls) < 2 {
		t.Fatalf("expected id + useradd calls, got %v", stub.calls)
	}
	if stub.calls[1][0] != "useradd" {
		t.Errorf("second call should be useradd, got %s", stub.calls[1][0])
	}
}

func TestWriteEnvFile_WritesSecretsAndGitignore(t *testing.T) {
	withStub(t) // stub chown so no root required
	dir := t.TempDir()
	secrets := map[string]string{"GH_TOKEN": "gh-test-token", "FOO": "bar"}

	if err := WriteEnvFile("testuser", dir, secrets); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}

	envData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf(".env not written: %v", err)
	}
	envStr := string(envData)
	if !strings.Contains(envStr, "export GH_TOKEN=") {
		t.Errorf(".env missing GH_TOKEN export: %s", envStr)
	}

	ignoreData, err := os.ReadFile(filepath.Join(dir, ".gitignore_global"))
	if err != nil {
		t.Fatalf(".gitignore_global not written: %v", err)
	}
	if !strings.Contains(string(ignoreData), ".env") {
		t.Errorf(".gitignore_global missing .env entry: %s", ignoreData)
	}
}

func TestConfigureGit_WritesSigningConfig(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	username := "testuser"
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPubKeyData testuser@clem"

	if err := ConfigureGit(username, dir, pubKey, "", ""); err != nil {
		t.Fatalf("ConfigureGit: %v", err)
	}

	asData, err := os.ReadFile(filepath.Join(dir, ".ssh", "allowed_signers"))
	if err != nil {
		t.Fatalf("allowed_signers not written: %v", err)
	}
	asStr := string(asData)
	if !strings.Contains(asStr, username+"@clem") {
		t.Errorf("allowed_signers missing commit email: %s", asStr)
	}
	if !strings.Contains(asStr, pubKey) {
		t.Errorf("allowed_signers missing pubkey: %s", asStr)
	}

	gcData, err := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	if err != nil {
		t.Fatalf(".gitconfig not written: %v", err)
	}
	gcStr := string(gcData)
	for _, want := range []string{"gpgsign = true", "format = ssh", "allowedSignersFile", "signingkey"} {
		if !strings.Contains(gcStr, want) {
			t.Errorf(".gitconfig missing %q: %s", want, gcStr)
		}
	}
}

func TestConfigureGit_WritesUserIdentity(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPubKeyData testuser@clem"

	if err := ConfigureGit("testuser", dir, pubKey, "clauderesearch", "212849679+clauderesearch@users.noreply.github.com"); err != nil {
		t.Fatalf("ConfigureGit: %v", err)
	}

	gcData, err := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	if err != nil {
		t.Fatalf(".gitconfig not written: %v", err)
	}
	gcStr := string(gcData)
	if !strings.Contains(gcStr, "\tname = clauderesearch") {
		t.Errorf(".gitconfig missing name: %s", gcStr)
	}
	if !strings.Contains(gcStr, "\temail = 212849679+clauderesearch@users.noreply.github.com") {
		t.Errorf(".gitconfig missing email: %s", gcStr)
	}
}

func TestConfigureGit_PreservesExistingIdentity(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPubKeyData testuser@clem"

	// pre-existing operator-set identity
	existing := "[user]\n\tname = operator\n\temail = operator@example.com\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitconfig"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ConfigureGit("testuser", dir, pubKey, "clauderesearch", "bot@example.com"); err != nil {
		t.Fatalf("ConfigureGit: %v", err)
	}

	gcData, _ := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	gcStr := string(gcData)
	if strings.Contains(gcStr, "clauderesearch") {
		t.Errorf("ConfigureGit overwrote operator name: %s", gcStr)
	}
	if strings.Contains(gcStr, "bot@example.com") {
		t.Errorf("ConfigureGit overwrote operator email: %s", gcStr)
	}
}

func TestConfigureGit_Idempotent(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPubKeyData testuser@clem"

	if err := ConfigureGit("testuser", dir, pubKey, "ada", "ada@clem.local"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ConfigureGit("testuser", dir, pubKey, "ada", "ada@clem.local"); err != nil {
		t.Fatalf("second call: %v", err)
	}

	gcData, _ := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	gcStr := string(gcData)
	if count := strings.Count(gcStr, "gpgsign"); count != 1 {
		t.Errorf("expected gpgsign once in .gitconfig, got %d: %s", count, gcStr)
	}
	if count := strings.Count(gcStr, "\tname = ada"); count != 1 {
		t.Errorf("expected name once in .gitconfig, got %d: %s", count, gcStr)
	}
	if count := strings.Count(gcStr, "\temail = ada@clem.local"); count != 1 {
		t.Errorf("expected email once in .gitconfig, got %d: %s", count, gcStr)
	}
}

func TestInstallGitHooks_WritesHookAndConfig(t *testing.T) {
	withStub(t)
	dir := t.TempDir()

	if err := InstallGitHooks("testuser", dir); err != nil {
		t.Fatalf("InstallGitHooks: %v", err)
	}

	hookPath := filepath.Join(dir, ".config", "git", "hooks", "pre-push")
	hookData, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("pre-push hook not written: %v", err)
	}
	if !strings.HasPrefix(string(hookData), "#!/bin/bash") {
		t.Errorf("pre-push hook missing shebang: %s", hookData[:20])
	}

	gitConfigData, err := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	if err != nil {
		t.Fatalf(".gitconfig not written: %v", err)
	}
	if !strings.Contains(string(gitConfigData), "hooksPath") {
		t.Errorf(".gitconfig missing hooksPath: %s", gitConfigData)
	}
}

func withGHHTTPClient(t *testing.T, handler http.Handler) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	orig := ghHTTPClient
	ghHTTPClient = srv.Client()
	// Redirect all requests to the test server by rewriting the URL host.
	// The test handler receives the full path so it can assert on it.
	ghHTTPClient.Transport = &rewriteTransport{base: srv.Client().Transport, host: srv.URL}
	t.Cleanup(func() { ghHTTPClient = orig })
}

type rewriteTransport struct {
	base http.RoundTripper
	host string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(rt.host, "http://")
	return rt.base.RoundTrip(req)
}

func TestRegisterSSHSigningKey_NoToken(t *testing.T) {
	err := RegisterSSHSigningKey("ssh-ed25519 AAAA testuser@clem", "", "clem-test")
	if err == nil {
		t.Fatal("expected error when ghToken is empty")
	}
	if !strings.Contains(err.Error(), "GH_TOKEN required") {
		t.Errorf("expected 'GH_TOKEN required' in error, got: %v", err)
	}
}

func TestRegisterSSHSigningKey_Success(t *testing.T) {
	withGHHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "ssh_signing_keys") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			t.Errorf("unexpected Authorization: %s", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"title":"clem-cdev-lead"`) {
			t.Errorf("expected title 'clem-cdev-lead' in payload, got: %s", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))

	err := RegisterSSHSigningKey("ssh-ed25519 AAAA testuser@clem", "fake-token", "clem-cdev-lead")
	if err != nil {
		t.Fatalf("expected no error on 201, got: %v", err)
	}
}

func TestRegisterSSHSigningKey_DefaultTitleWhenEmpty(t *testing.T) {
	withGHHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"title":"clem-signing"`) {
			t.Errorf("expected default title 'clem-signing' when caller passes empty, got: %s", body)
		}
		w.WriteHeader(http.StatusCreated)
	}))

	err := RegisterSSHSigningKey("ssh-ed25519 AAAA testuser@clem", "fake-token", "")
	if err != nil {
		t.Fatalf("expected no error on 201, got: %v", err)
	}
}

func TestRegisterSSHSigningKey_AlreadyRegistered(t *testing.T) {
	withGHHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"key is already in use"}`)) //nolint:errcheck
	}))

	err := RegisterSSHSigningKey("ssh-ed25519 AAAA testuser@clem", "fake-token", "clem-test")
	if err != nil {
		t.Fatalf("expected nil error when key already registered, got: %v", err)
	}
}

func TestRegisterSSHSigningKey_APIError(t *testing.T) {
	withGHHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Must have admin rights to Repository."}`)) //nolint:errcheck
	}))

	err := RegisterSSHSigningKey("ssh-ed25519 AAAA testuser@clem", "fake-token", "clem-test")
	if err == nil {
		t.Fatal("expected error on non-201/non-422 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected status code 403 in error, got: %v", err)
	}
}

func TestTmuxAlive_UsesSudoForAgentUser(t *testing.T) {
	stub := withStub(t)

	if !TmuxAlive("cdev-worker", "worker") {
		t.Fatal("expected alive when stub returns no error")
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected exactly one Run call, got %d: %v", len(stub.calls), stub.calls)
	}
	got := stub.calls[0]
	want := []string{"sudo", "-n", "-u", "cdev-worker", "tmux", "has-session", "-t", "worker"}
	if !equalSlice(got, want) {
		t.Errorf("invocation mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestTmuxAlive_ReportsDownWhenSudoTmuxFails(t *testing.T) {
	stub := withStub(t)
	stub.failOn = "sudo"

	if TmuxAlive("cdev-worker", "worker") {
		t.Fatal("expected dead when sudo tmux returns error")
	}
}

func TestTmuxAlive_EmptyOSUserFallsBackToCallerOwnServer(t *testing.T) {
	// Backwards-compat path: an empty user means "check our own tmux server"
	// which is what older callers (and tests) relied on. Keep that working
	// so the new signature is additive, not breaking.
	stub := withStub(t)

	TmuxAlive("", "worker")
	if len(stub.calls) != 1 {
		t.Fatalf("expected one Run call, got %d", len(stub.calls))
	}
	if stub.calls[0][0] != "tmux" {
		t.Errorf("expected fallback to call tmux directly, got: %v", stub.calls[0])
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
