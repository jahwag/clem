package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jahwag/clem/internal/config"
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
		{"github classic PAT", "ghp_1234567890abcdefghijklmnopqrstuvwxyz"},                          // clem:allow-secret
		{"github OAuth token", "gho_1234567890abcdefghijklmnopqrstuvwxyz"},                          // clem:allow-secret
		{"github App server", "ghs_1234567890abcdefghijklmnopqrstuvwxyz"},                           // clem:allow-secret
		{"github fine-grained PAT", "github_pat_11ABCDEFG0abcdefghijkl_" + strings.Repeat("a", 60)}, // clem:allow-secret
		{"anthropic API key", "sk-ant-abcdefghijklmnopqrstuvwxyz12345"},                             // clem:allow-secret
		{"openai API key", "sk-proj-abcdefghijklmnopqrstuvwxyz1234567890"},                          // clem:allow-secret
		{"slack bot token", "xoxb-1234567890-0987654321-abcdefghij"},                                // clem:allow-secret
		{"slack user token", "xoxp-1234567890-abcdefghij-klmnopqrst"},                               // clem:allow-secret
		{"aws access key", "AKIAIOSFODNN7EXAMPLE"},                                                  // clem:allow-secret
		{"age secret key", "AGE-SECRET-KEY-1ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCDEFGHIJKLMNO"},   // clem:allow-secret
		{"openssh private key", "-----BEGIN OPENSSH PRIVATE KEY-----"},                              // clem:allow-secret
		{"rsa private key", "-----BEGIN RSA PRIVATE KEY-----"},                                      // clem:allow-secret
		{"generic private key", "-----BEGIN PRIVATE KEY-----"},                                      // clem:allow-secret
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
	if !strings.Contains(prePushHookContent, UnicodeTrapBytesPattern) {
		t.Error("pre-push hook should embed UnicodeTrapBytesPattern (Pass 3, red-team A3)")
	}
	if !strings.Contains(prePushHookContent, "export LC_ALL=C") {
		t.Error("pre-push hook should force LC_ALL=C so matching is locale-independent (systemd sessions run under the C locale)")
	}
	if strings.Contains(prePushHookContent, "grep -P") {
		t.Error("pre-push hook must not use grep -P: \\x{} code points above 0xFF silently fail under LANG=C")
	}
	if !strings.Contains(prePushHookContent, "base64 -d") {
		t.Error("pre-push hook should include base64 decode pass (Pass 2, red-team A9)")
	}
	if !strings.Contains(prePushHookContent, PrePushAllowSecretMarker) {
		t.Errorf("pre-push hook should embed allow-marker %q so bash and Go agree on the bypass token", PrePushAllowSecretMarker)
	}
	if !strings.Contains(prePushHookContent, `grep -E '^\+([^+]|$)'`) {
		t.Error("pre-push hook should restrict secret-pattern scanning to ADDED diff lines (^+ excluding ^+++)")
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

// trapBytesRe converts UnicodeTrapBytesPattern (bash printf octal escapes over
// raw UTF-8 bytes) into a Go regexp operating on a latin-1-widened copy of the
// subject — a faithful simulation of how LC_ALL=C grep -E sees bytes. This
// keeps the byte pattern derived from the single shipped constant, not from a
// hand-maintained copy.
func trapBytesRe(t *testing.T) *regexp.Regexp {
	t.Helper()
	var sb strings.Builder
	s := UnicodeTrapBytesPattern
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
			if err != nil {
				t.Fatalf("bad octal escape at %d in UnicodeTrapBytesPattern: %v", i, err)
			}
			fmt.Fprintf(&sb, `\x{%02X}`, n)
			i += 3
			continue
		}
		sb.WriteByte(s[i])
	}
	re, err := regexp.Compile(sb.String())
	if err != nil {
		t.Fatalf("UnicodeTrapBytesPattern does not translate to a valid regexp (%q): %v", sb.String(), err)
	}
	return re
}

// latin1Widen maps each UTF-8 byte of s to its own rune, so a byte-level
// pattern can be evaluated with Go's rune-based regexp engine.
func latin1Widen(s string) string {
	b := []byte(s)
	rs := make([]rune, len(b))
	for i, bb := range b {
		rs[i] = rune(bb)
	}
	return string(rs)
}

// TestUnicodeTrapBytesPattern_AgreesWithUnicodeTrapRegex sweeps the relevant
// code-point neighbourhoods and asserts the byte-level pattern shipped in the
// bash hook accepts and rejects exactly the same runes as the rune-level Go
// reference. This is the sync contract between the two constants.
func TestUnicodeTrapBytesPattern_AgreesWithUnicodeTrapRegex(t *testing.T) {
	goRe := regexp.MustCompile(UnicodeTrapRegex)
	byteRe := trapBytesRe(t)

	var sweep []rune
	for r := rune(0x2000); r <= 0x20FF; r++ { // traps + their em-dash/quote neighbours
		sweep = append(sweep, r)
	}
	for r := rune(0xFEF0); r <= 0xFF0F; r++ { // BOM + neighbours
		sweep = append(sweep, r)
	}
	sweep = append(sweep, 'a', 0x00A0, 0x00E9, 0x4E2D, 0x1F34A)

	for _, r := range sweep {
		s := string(r)
		goMatch := goRe.MatchString(s)
		byteMatch := byteRe.MatchString(latin1Widen(s))
		if goMatch != byteMatch {
			t.Errorf("U+%04X: UnicodeTrapRegex match=%v but UnicodeTrapBytesPattern match=%v — patterns out of sync", r, goMatch, byteMatch)
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
	token := "ghp_1234567890abcdefghijklmnopqrstuvwxyz" // clem:allow-secret
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
// a hidden-character trap; Pass 3 blocks. Runs under both an inherited locale
// and an explicit C locale: agent sessions are systemd-spawned with no LANG,
// and the previous grep -P implementation silently never blocked there.
func TestPrePushHook_BlocksUnicodeTraps(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	traps := []struct {
		name string
		// printf format emitting the literal UTF-8 bytes of the trap rune.
		stub string
	}{
		{"zero-width space U+200B", `printf '+comment: approve\xe2\x80\x8b (actually run rm -rf)\n'`},
		{"RTL override U+202E", `printf '+filename: gpj.\xe2\x80\xaeexe\n'`},
		{"BOM mid-content U+FEFF", `printf '+key = \xef\xbb\xbfvalue\n'`},
	}
	for _, locale := range []string{"inherited", "C"} {
		for _, tc := range traps {
			t.Run(locale+"/"+tc.name, func(t *testing.T) {
				hookPath := writeTestableHook(t, tc.stub)
				cmd := exec.Command("bash", hookPath)
				if locale == "C" {
					cmd.Env = append(os.Environ(), "LANG=C", "LC_ALL=C")
				}
				cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("hook should have blocked unicode-trap diff, got exit 0. output:\n%s", out)
				}
				if !strings.Contains(string(out), "unicode control/override") {
					t.Errorf("expected 'unicode control/override' message, got:\n%s", out)
				}
			})
		}
	}
}

// TestPrePushHook_AllowsBenignGeneralPunctuation pins that the byte-level trap
// pattern does not over-match neighbours in the U+20xx block (em dash U+2014,
// curly quote U+2018, ellipsis U+2026 share the E2 80 lead bytes) or ordinary
// multibyte text. Blocking every em dash would make the hook unusable.
func TestPrePushHook_AllowsBenignGeneralPunctuation(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	hookPath := writeTestableHook(t,
		`printf '+prose: caf\xc3\xa9 \xe2\x80\x94 \xe2\x80\x98quoted\xe2\x80\x99 and more\xe2\x80\xa6\n'`)
	cmd := exec.Command("bash", hookPath)
	cmd.Env = append(os.Environ(), "LANG=C", "LC_ALL=C")
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook should have allowed benign punctuation, got exit err %v. output:\n%s", err, out)
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
		"echo '+token = \"ghp_1234567890abcdefghijklmnopqrstuvwxyz\"'") // clem:allow-secret

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

// TestPrePushHook_AllowsRemovedSecretLine pins the added-only scope rule:
// a removed line that textually matches the secret pattern cannot leak the
// token to the remote (it was already there and is now being deleted), so
// the hook must NOT block. Without this rule, every fork-sync that touches
// a file whose previous version contained a secret-shaped fixture is blocked
// — see the regression Ada reported on cdev 2026-05-09.
func TestPrePushHook_AllowsRemovedSecretLine(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	hookPath := writeTestableHook(t,
		"echo '-token = \"ghp_1234567890abcdefghijklmnopqrstuvwxyz\"'") // clem:allow-secret
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook should have exited 0 on removed-line diff, got error %v. output:\n%s", err, out)
	}
}

// TestPrePushHook_AllowsLineWithAllowMarker pins the marker escape hatch:
// an ADDED line containing a secret-shaped string AND the
// PrePushAllowSecretMarker on the same line must NOT block. This lets the
// hook's own regex test fixtures live in the repo without making fork sync
// a wedge. The marker must be on the SAME line as the secret — line scope is
// intentional so a stray marker elsewhere cannot blanket-bypass scanning.
func TestPrePushHook_AllowsLineWithAllowMarker(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	// Build the stub diff line at runtime so this source line itself does
	// not embed a secret-shaped literal that would trip the hook when the
	// commit lands.
	addedLine := "+token := \"ghp_" + strings.Repeat("X", 36) + "\" // " + PrePushAllowSecretMarker
	hookPath := writeTestableHook(t, "echo '"+addedLine+"'")
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook should have exited 0 on marker-bearing line, got error %v. output:\n%s", err, out)
	}
}

// TestPrePushHook_StillBlocksAddedSecretWithoutMarker is the negative side
// of the marker rule: removing the marker brings the block back. Together
// with TestPrePushHook_AllowsLineWithAllowMarker this guarantees the marker
// is the only difference and not a regex-loosening bug.
func TestPrePushHook_StillBlocksAddedSecretWithoutMarker(t *testing.T) {
	for _, bin := range []string{"bash", "grep"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH - skipping integration test", bin)
		}
	}
	addedLine := "+token := \"ghp_" + strings.Repeat("X", 36) + "\""
	hookPath := writeTestableHook(t, "echo '"+addedLine+"'")
	cmd := exec.Command("bash", hookPath)
	cmd.Stdin = strings.NewReader("refs/heads/feature aaa refs/heads/feature bbb\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook should have blocked added secret without marker, got exit 0. output:\n%s", out)
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
		{"go GH_TOKEN", `token := os.Getenv("GH_TOKEN")`},                       // clem:allow-secret
		{"go DISCORD_TOKEN", `d := os.Getenv("DISCORD_TOKEN")`},                 // clem:allow-secret
		{"go ANTHROPIC_API_KEY", `k := os.Getenv("ANTHROPIC_API_KEY")`},         // clem:allow-secret
		{"go AWS_SECRET_ACCESS_KEY", `s := os.Getenv("AWS_SECRET_ACCESS_KEY")`}, // clem:allow-secret
		{"go SLACK_MCP_XOXP_TOKEN", `t := os.Getenv("SLACK_MCP_XOXP_TOKEN")`},   // clem:allow-secret
		{"python double-quote GH_TOKEN", `tok = os.environ["GH_TOKEN"]`},        // clem:allow-secret
		{"node GH_TOKEN", `const t = process.env.GH_TOKEN`},                     // clem:allow-secret
		{"node ANTHROPIC_API_KEY", `const k = process.env.ANTHROPIC_API_KEY`},   // clem:allow-secret
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

func TestWriteSettings_WorkdirKeyUsesProjectDirectly(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	// Agent key "my-agent" contains a hyphen; project is "myproject".
	// The workdir key in .claude.json must be homeDir/myproject, not homeDir/myproject-my.
	if err := WriteSettings("myproject-my-agent", dir, "myproject", ""); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatalf(".claude.json not written: %v", err)
	}
	wantKey := filepath.Join(dir, "myproject")
	if !strings.Contains(string(data), wantKey) {
		t.Errorf(".claude.json workdir key: want %q in\n%s", wantKey, data)
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

// writeCreds is a small helper for credentials-file fixtures.
func writeCreds(t *testing.T, dir string, oauth map[string]any) {
	t.Helper()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"claudeAiOauth": oauth})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Refresh-token presence — not access-token expiry — gates NeedsLogin.
// Claude Max access tokens live ~8h and refresh transparently; gating on
// expiry would prompt unnecessary daily logins.
func TestNeedsLogin_RefreshTokenPresent(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, map[string]any{
		"expiresAt":    time.Now().Add(-1 * time.Hour).UnixMilli(), // already expired
		"refreshToken": "rt_abc",
	})
	if NeedsLogin(dir) {
		t.Error("NeedsLogin should return false when a refresh token is present, even if access token expired")
	}
}

func TestNeedsLogin_RefreshTokenMissing(t *testing.T) {
	dir := t.TempDir()
	writeCreds(t, dir, map[string]any{
		"expiresAt": time.Now().Add(24 * time.Hour).UnixMilli(),
		// no refreshToken
	})
	if !NeedsLogin(dir) {
		t.Error("NeedsLogin should return true when refresh token is missing — access token alone cannot self-renew")
	}
}

func TestHasRefreshToken(t *testing.T) {
	dir := t.TempDir()
	if HasRefreshToken(dir) {
		t.Error("HasRefreshToken should be false when credentials file is missing")
	}
	writeCreds(t, dir, map[string]any{"refreshToken": ""})
	if HasRefreshToken(dir) {
		t.Error("HasRefreshToken should be false when refreshToken is empty")
	}
	writeCreds(t, dir, map[string]any{"refreshToken": "rt_xyz"})
	if !HasRefreshToken(dir) {
		t.Error("HasRefreshToken should be true when refreshToken is non-empty")
	}
}

func writeCodexAuth(t *testing.T, dir string, auth map[string]any) {
	t.Helper()
	codexDir := filepath.Join(dir, ".codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexNeedsLogin(t *testing.T) {
	// No auth.json at all → needs login.
	if !CodexNeedsLogin(t.TempDir()) {
		t.Error("missing auth.json should require login")
	}

	// auth.json with neither refresh token nor API key → needs login.
	dir := t.TempDir()
	writeCodexAuth(t, dir, map[string]any{"tokens": map[string]any{"access_token": "short"}})
	if !CodexNeedsLogin(dir) {
		t.Error("auth without refresh token or API key should require login")
	}

	// OAuth refresh token present → authenticated.
	dir = t.TempDir()
	writeCodexAuth(t, dir, map[string]any{"tokens": map[string]any{"refresh_token": "rt_abc"}})
	if CodexNeedsLogin(dir) {
		t.Error("auth with refresh token should not require login")
	}

	// API-key login (no OAuth tokens) → authenticated.
	dir = t.TempDir()
	writeCodexAuth(t, dir, map[string]any{"OPENAI_API_KEY": "sk-test"})
	if CodexNeedsLogin(dir) {
		t.Error("auth with API key should not require login")
	}

	// Malformed JSON → needs login (fail safe).
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "auth.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if !CodexNeedsLogin(dir) {
		t.Error("malformed auth.json should require login")
	}
}

func TestWriteSettings_WritesExpectedFiles(t *testing.T) {
	stub := withStub(t)
	dir := t.TempDir()
	username := "testuser"

	if err := WriteSettings(username, dir, "myproject", ""); err != nil {
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

	if err := WriteSettings("testuser", dir, "myproject", "low"); err != nil {
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

func TestWriteEnvFile_SingleQuotesSpecialChars(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	// Values containing $, backtick, and ' must be written literally (no bash expansion).
	secrets := map[string]string{
		"DOLLAR":   "foo$bar",
		"BACKTICK": "foo`bar`baz",
		"QUOTE":    "it's here",
	}

	if err := WriteEnvFile("testuser", dir, secrets); err != nil {
		t.Fatalf("WriteEnvFile: %v", err)
	}

	envData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatalf(".env not written: %v", err)
	}
	envStr := string(envData)

	// All values must use single-quote syntax so bash treats them as literals.
	if !strings.Contains(envStr, "export DOLLAR='foo$bar'") {
		t.Errorf("DOLLAR not single-quoted literally: %s", envStr)
	}
	if !strings.Contains(envStr, "export BACKTICK='foo`bar`baz'") {
		t.Errorf("BACKTICK not single-quoted literally: %s", envStr)
	}
	// Single quote in value must be escaped as '\''
	if !strings.Contains(envStr, `export QUOTE='it'\''s here'`) {
		t.Errorf("QUOTE single-quote not escaped correctly: %s", envStr)
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
	gitEmail := "212849679+clauderesearch@users.noreply.github.com"

	if err := ConfigureGit("testuser", dir, pubKey, "clauderesearch", gitEmail); err != nil {
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
	if !strings.Contains(gcStr, "\temail = "+gitEmail) {
		t.Errorf(".gitconfig missing email: %s", gcStr)
	}

	asData, err := os.ReadFile(filepath.Join(dir, ".ssh", "allowed_signers"))
	if err != nil {
		t.Fatalf("allowed_signers not written: %v", err)
	}
	asStr := string(asData)
	if !strings.HasPrefix(asStr, gitEmail+" ") {
		t.Errorf("allowed_signers principal should be gitEmail %q, got: %s", gitEmail, asStr)
	}
}

func TestConfigureGit_OverwritesExistingIdentity(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPubKeyData testuser@clem"

	// pre-existing stale identity (e.g. from a prior provision)
	existing := "[user]\n\tname = old-name\n\temail = old@example.com\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitconfig"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ConfigureGit("testuser", dir, pubKey, "clauderesearch", "bot@example.com"); err != nil {
		t.Fatalf("ConfigureGit: %v", err)
	}

	gcData, _ := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	gcStr := string(gcData)
	if !strings.Contains(gcStr, "\tname = clauderesearch") {
		t.Errorf("ConfigureGit did not write new name: %s", gcStr)
	}
	if !strings.Contains(gcStr, "\temail = bot@example.com") {
		t.Errorf("ConfigureGit did not write new email: %s", gcStr)
	}
	if strings.Contains(gcStr, "old-name") {
		t.Errorf("stale name was not removed: %s", gcStr)
	}
	if strings.Contains(gcStr, "old@example.com") {
		t.Errorf("stale email was not removed: %s", gcStr)
	}
}

func TestConfigureGit_LeavesIdentityWhenInputsEmpty(t *testing.T) {
	withStub(t)
	dir := t.TempDir()
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestPubKeyData testuser@clem"

	// operator-set identity present; clem.yaml supplies no git_name / git_email
	existing := "[user]\n\tname = operator\n\temail = operator@example.com\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitconfig"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ConfigureGit("testuser", dir, pubKey, "", ""); err != nil {
		t.Fatalf("ConfigureGit: %v", err)
	}

	gcData, _ := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	gcStr := string(gcData)
	if !strings.Contains(gcStr, "\tname = operator") {
		t.Errorf("operator name was removed despite empty input: %s", gcStr)
	}
	if !strings.Contains(gcStr, "\temail = operator@example.com") {
		t.Errorf("operator email was removed despite empty input: %s", gcStr)
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

func TestWriteHostManagedSettings_WritesDenyList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "claude-code", "managed-settings.json")

	cfg := &config.Config{
		Project: "test",
		Agents: map[string]config.AgentConfig{
			"lead":   {Permissions: config.PermissionsConfig{Deny: []string{"Bash(curl:*)", "Bash(wget:*)"}}},
			"worker": {Permissions: config.PermissionsConfig{Deny: []string{"Bash(rm:*)", "Bash(curl:*)"}}},
		},
	}

	if err := WriteHostManagedSettings(cfg, path); err != nil {
		t.Fatalf("WriteHostManagedSettings: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("managed-settings.json not written: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("expected permissions object, got: %v", doc)
	}
	deny, ok := perms["deny"].([]any)
	if !ok {
		t.Fatalf("expected deny array, got: %v", perms)
	}

	// curl appears in both agents but must deduplicate; result sorted.
	wantDenies := []string{"Bash(curl:*)", "Bash(rm:*)", "Bash(wget:*)"}
	if len(deny) != len(wantDenies) {
		t.Fatalf("expected %d deny entries, got %d: %v", len(wantDenies), len(deny), deny)
	}
	for i, want := range wantDenies {
		if deny[i] != want {
			t.Errorf("deny[%d] = %q, want %q", i, deny[i], want)
		}
	}
}

func TestWriteHostManagedSettings_EmptyDenyWhenNoPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed-settings.json")

	cfg := &config.Config{
		Project: "test",
		Agents: map[string]config.AgentConfig{
			"lead": {Name: "Lead"},
		},
	}

	if err := WriteHostManagedSettings(cfg, path); err != nil {
		t.Fatalf("WriteHostManagedSettings: %v", err)
	}

	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	perms := doc["permissions"].(map[string]any)
	deny := perms["deny"].([]any)
	if len(deny) != 0 {
		t.Errorf("expected empty deny list, got: %v", deny)
	}
}

// skillsStub interprets ln -sfn / rm -f / mkdir -p so SyncSkillsRepo's
// filesystem-mutating side effects land on the real test temp dir. Calls are
// still recorded for assertions on what was invoked.
type skillsStub struct {
	stubExec
	cloneSeed func(cache string) // called when `git clone` is invoked
}

func (s *skillsStub) Run(name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, append([]string{name}, args...))
	// Two forms supported:
	//   sudo -iu <user> <cmd> <cmdArgs...>    (provision path)
	//   <cmd> <cmdArgs...>                    (runner/self path)
	var cmd string
	var cargs []string
	switch {
	case name == "sudo" && len(args) >= 4 && args[0] == "-iu":
		cmd = args[2]
		cargs = args[3:]
	default:
		cmd = name
		cargs = args
	}
	switch cmd {
	case "ln":
		if len(cargs) >= 3 && cargs[0] == "-sfn" {
			_ = os.Remove(cargs[2])
			if err := os.Symlink(cargs[1], cargs[2]); err != nil {
				return nil, err
			}
		}
	case "rm":
		if len(cargs) >= 2 && cargs[0] == "-f" {
			_ = os.Remove(cargs[1])
		}
	case "git":
		if len(cargs) >= 1 && cargs[0] == "clone" && s.cloneSeed != nil {
			if len(cargs) >= 3 {
				s.cloneSeed(cargs[2])
			}
		}
		// git -C <dir> pull --ff-only — no-op
	}
	return nil, nil
}

func withSkillsStub(t *testing.T) *skillsStub {
	t.Helper()
	stub := &skillsStub{}
	orig := sys
	sys = stub
	t.Cleanup(func() { sys = orig })
	return stub
}

func TestSkillsCacheName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/foo/bar":            "bar",
		"https://github.com/foo/bar.git":        "bar",
		"https://github.com/foo/bar/":           "bar",
		"git@github.com:foo/bar.git":            "bar",
		"ssh://git@self-hosted/foo/bar.git":     "bar",
		"https://gitlab.example.com/g/sub/repo": "repo",
	}
	for in, want := range cases {
		if got := skillsCacheName(in); got != want {
			t.Errorf("skillsCacheName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSyncSkillsRepo_SymlinksSharedAndAgent verifies the happy path: a
// pre-existing cache with shared/ and worker/ subdirs produces symlinks in
// the agent's skills dir for each, and ignores top-level dirs outside that set.
func TestSyncSkillsRepo_SymlinksSharedAndAgent(t *testing.T) {
	stub := withSkillsStub(t)
	home := t.TempDir()
	repoURL := "https://example.com/owner/team-skills.git"
	cache := filepath.Join(home, ".cache", "team-skills")

	// Seed cache as if `git clone` had already run.
	for _, p := range []string{
		filepath.Join(cache, "shared", "skill-a", "SKILL.md"),
		filepath.Join(cache, "worker", "skill-b", "SKILL.md"),
		filepath.Join(cache, "lead", "skill-c", "SKILL.md"),       // not for worker
		filepath.Join(cache, "random-dir", "skill-d", "SKILL.md"), // ignored
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatalf("seed mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("---\nname: x\n---\n"), 0644); err != nil {
			t.Fatalf("seed write: %v", err)
		}
	}

	if err := SyncSkillsRepo("testuser", home, "worker", repoURL); err != nil {
		t.Fatalf("SyncSkillsRepo: %v", err)
	}

	skillsDir := filepath.Join(home, ".claude", "skills")
	for _, name := range []string{"skill-a", "skill-b"} {
		link := filepath.Join(skillsDir, name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Errorf("%s: expected symlink, got: %v", name, err)
			continue
		}
		if !strings.HasPrefix(target, cache+string(filepath.Separator)) {
			t.Errorf("%s: symlink target %q not under cache %q", name, target, cache)
		}
	}
	// skill-c (lead) and skill-d (random-dir) must NOT appear for worker
	for _, name := range []string{"skill-c", "skill-d"} {
		if _, err := os.Lstat(filepath.Join(skillsDir, name)); err == nil {
			t.Errorf("worker should not see %s", name)
		}
	}

	// First call should have invoked `git clone` (cache didn't exist before
	// the test seeded it... wait, we seeded it. So `git -C cache pull --ff-only`
	// is what should have fired).
	sawPull := false
	for _, c := range stub.calls {
		if len(c) >= 5 && c[0] == "sudo" && c[3] == "git" && c[4] == "-C" {
			sawPull = true
		}
	}
	if !sawPull {
		t.Errorf("expected git pull to fire on existing cache; calls: %v", stub.calls)
	}
}

// TestSyncSkillsRepo_PrunesStaleSymlinks verifies that a symlink in the
// skills dir pointing into the cache for a skill that no longer exists in
// the repo is removed on next sync. Real dirs and external-target symlinks
// are left alone.
func TestSyncSkillsRepo_PrunesStaleSymlinks(t *testing.T) {
	withSkillsStub(t)
	home := t.TempDir()
	repoURL := "https://example.com/owner/team-skills.git"
	cache := filepath.Join(home, ".cache", "team-skills")
	skillsDir := filepath.Join(home, ".claude", "skills")

	// Cache has only one skill in shared.
	if err := os.MkdirAll(filepath.Join(cache, "shared", "kept-skill"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing stale symlink pointing into cache for a now-removed skill.
	staleTarget := filepath.Join(cache, "shared", "removed-skill")
	if err := os.Symlink(staleTarget, filepath.Join(skillsDir, "removed-skill")); err != nil {
		t.Fatal(err)
	}
	// Operator-installed real skill dir (e.g. from InstallSkill). Must survive.
	if err := os.MkdirAll(filepath.Join(skillsDir, "operator-skill"), 0755); err != nil {
		t.Fatal(err)
	}
	// Symlink pointing OUTSIDE the cache. Must survive.
	external := filepath.Join(home, "external-skill")
	if err := os.MkdirAll(external, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(skillsDir, "external-skill")); err != nil {
		t.Fatal(err)
	}

	if err := SyncSkillsRepo("testuser", home, "worker", repoURL); err != nil {
		t.Fatalf("SyncSkillsRepo: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(skillsDir, "removed-skill")); err == nil {
		t.Error("stale symlink to cache should have been pruned")
	}
	if _, err := os.Lstat(filepath.Join(skillsDir, "operator-skill")); err != nil {
		t.Errorf("operator-installed real dir should survive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(skillsDir, "external-skill")); err != nil {
		t.Errorf("symlink outside cache should survive: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(skillsDir, "kept-skill")); err != nil {
		t.Errorf("kept-skill symlink should be created: %v", err)
	}
}

// TestSyncSkillsRepo_RejectsInvalidNames verifies that skill subdirs whose
// names fail the extension-name regex (e.g. shell-special characters) are
// skipped rather than symlinked, even if the operator-controlled repo
// somehow contained them.
func TestSyncSkillsRepo_RejectsInvalidNames(t *testing.T) {
	withSkillsStub(t)
	home := t.TempDir()
	repoURL := "https://example.com/owner/team-skills.git"
	cache := filepath.Join(home, ".cache", "team-skills")

	if err := os.MkdirAll(filepath.Join(cache, "shared", "good-skill"), 0755); err != nil {
		t.Fatal(err)
	}
	// Names that should be rejected by the regex
	for _, bad := range []string{"-leading-dash", "has space", "has;semi"} {
		if err := os.MkdirAll(filepath.Join(cache, "shared", bad), 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := SyncSkillsRepo("testuser", home, "worker", repoURL); err != nil {
		t.Fatalf("SyncSkillsRepo: %v", err)
	}

	skillsDir := filepath.Join(home, ".claude", "skills")
	if _, err := os.Readlink(filepath.Join(skillsDir, "good-skill")); err != nil {
		t.Errorf("good-skill should be symlinked: %v", err)
	}
	for _, bad := range []string{"-leading-dash", "has space", "has;semi"} {
		if _, err := os.Lstat(filepath.Join(skillsDir, bad)); err == nil {
			t.Errorf("invalid name %q should have been skipped", bad)
		}
	}
}

// TestSyncSkillsRepo_ClonesWhenCacheMissing verifies that a missing cache dir
// triggers `git clone` rather than `git pull`. The stub's cloneSeed creates
// the dir so the rest of the function can read it.
// TestSyncSkillsRepoAsSelf_NoSudoWrap verifies the runtime variant invokes
// git/ln/rm directly (no `sudo -iu` wrapping), since the runner already
// executes as the agent user.
func TestSyncSkillsRepoAsSelf_NoSudoWrap(t *testing.T) {
	stub := withSkillsStub(t)
	home := t.TempDir()
	cache := filepath.Join(home, ".cache", "team-skills")
	if err := os.MkdirAll(filepath.Join(cache, "shared", "live-skill"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := SyncSkillsRepoAsSelf(home, "worker", "https://example.com/owner/team-skills.git"); err != nil {
		t.Fatalf("SyncSkillsRepoAsSelf: %v", err)
	}

	for _, c := range stub.calls {
		if len(c) >= 2 && c[0] == "sudo" {
			t.Errorf("AsSelf path must not call sudo; got %v", c)
		}
	}
	if _, err := os.Readlink(filepath.Join(home, ".claude", "skills", "live-skill")); err != nil {
		t.Errorf("AsSelf should symlink live-skill: %v", err)
	}
}

func TestSyncSkillsRepo_ClonesWhenCacheMissing(t *testing.T) {
	stub := withSkillsStub(t)
	home := t.TempDir()
	repoURL := "git@gitlab.example.com:owner/team-skills.git"
	cache := filepath.Join(home, ".cache", "team-skills")

	stub.cloneSeed = func(c string) {
		if c != cache {
			t.Errorf("clone target %q, want %q", c, cache)
		}
		if err := os.MkdirAll(filepath.Join(c, "shared", "first-skill"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := SyncSkillsRepo("testuser", home, "worker", repoURL); err != nil {
		t.Fatalf("SyncSkillsRepo: %v", err)
	}

	sawClone := false
	for _, c := range stub.calls {
		if len(c) >= 5 && c[0] == "sudo" && c[3] == "git" && c[4] == "clone" {
			sawClone = true
			if c[5] != repoURL {
				t.Errorf("clone URL = %q, want %q", c[5], repoURL)
			}
		}
	}
	if !sawClone {
		t.Errorf("expected git clone on missing cache; calls: %v", stub.calls)
	}

	if _, err := os.Readlink(filepath.Join(home, ".claude", "skills", "first-skill")); err != nil {
		t.Errorf("first-skill should be symlinked after clone: %v", err)
	}
}

func TestWriteHostManagedSettings_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed-settings.json")
	cfg := &config.Config{
		Project: "test",
		Agents: map[string]config.AgentConfig{
			"lead": {Permissions: config.PermissionsConfig{Deny: []string{"Bash(curl:*)"}}},
		},
	}

	if err := WriteHostManagedSettings(cfg, path); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, _ := os.ReadFile(path)

	if err := WriteHostManagedSettings(cfg, path); err != nil {
		t.Fatalf("second write: %v", err)
	}
	second, _ := os.ReadFile(path)

	if string(first) != string(second) {
		t.Errorf("WriteHostManagedSettings not idempotent:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestEnsureSystemUser_CreatesWithSystemFlags(t *testing.T) {
	stub := withStub(t)
	stub.failOn = "id" // user does not exist yet
	if err := EnsureSystemUser("clem-proxy"); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	var useradd []string
	for _, c := range stub.calls {
		if c[0] == "useradd" {
			useradd = c
		}
	}
	if useradd == nil {
		t.Fatal("expected useradd to be called")
	}
	joined := strings.Join(useradd, " ")
	for _, want := range []string{"--system", "--no-create-home", "/usr/sbin/nologin", "clem-proxy"} {
		if !strings.Contains(joined, want) {
			t.Errorf("useradd missing %q: %v", want, useradd)
		}
	}
}

func TestEnsureSystemUser_SkipsWhenExists(t *testing.T) {
	stub := withStub(t) // id succeeds (no failOn) => user exists
	if err := EnsureSystemUser("clem-proxy"); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	for _, c := range stub.calls {
		if c[0] == "useradd" {
			t.Fatalf("should not call useradd when user exists: %v", stub.calls)
		}
	}
}

func TestInstallPipelock_PinnedDownloadWhenAbsent(t *testing.T) {
	// Default stub returns empty output for the version-check bash call, so the
	// pinned version is not detected and the download proceeds.
	stub := withStub(t)
	if err := InstallPipelock(); err != nil {
		t.Fatalf("InstallPipelock: %v", err)
	}
	var script string
	for _, c := range stub.calls {
		if c[0] == "bash" && len(c) >= 3 && strings.Contains(c[2], "releases/download") {
			script = c[2]
		}
	}
	if script == "" {
		t.Fatal("expected bash install (download) script to run")
	}
	for _, want := range []string{
		PipelockVersion,
		"releases/download",
		"sha256sum -c -",
		"no checksum line for", // empty-grep guard
		"install -m 0755 pipelock /usr/local/bin/pipelock",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script missing %q:\n%s", want, script)
		}
	}
}

func TestInstallPipelock_SkipsWhenPinnedPresent(t *testing.T) {
	orig := sys
	t.Cleanup(func() { sys = orig })
	// The version-check bash call reports the pinned version → no download.
	rec := &versionStub{version: PipelockVersion}
	sys = rec
	if err := InstallPipelock(); err != nil {
		t.Fatalf("InstallPipelock: %v", err)
	}
	if rec.downloaded {
		t.Fatal("should not download when pinned version already present")
	}
}

// versionStub reports the pinned version for the version-check bash invocation
// and records whether a download (bash script referencing the release) ran.
type versionStub struct {
	version    string
	downloaded bool
}

func (v *versionStub) Run(name string, args ...string) ([]byte, error) {
	if name == "bash" && len(args) >= 2 {
		script := args[1]
		if strings.Contains(script, "releases/download") {
			v.downloaded = true
			return nil, nil
		}
		if strings.Contains(script, "--version") {
			return []byte("pipelock " + strings.TrimPrefix(v.version, "v")), nil
		}
	}
	return nil, nil
}

func TestBrokeredEnv_PlaceholdersAndConnection(t *testing.T) {
	av := config.VaultBackend{Backend: "agent-vault"}
	ac := config.AgentConfig{
		VaultBroker:     true,
		BrokeredSecrets: []string{"ANTHROPIC_API_KEY", "SLACK_MCP_XOXP_TOKEN"},
	}
	flat := map[string]string{
		"ANTHROPIC_API_KEY":    "sk-ant-REAL",
		"SLACK_MCP_XOXP_TOKEN": "xoxp-REAL",
		"DISCORD_TOKEN":        "discord-REAL",
	}
	env := BrokeredEnv(av, ac, "av_agt_tok", "anthropic", flat)

	// Brokered keys → placeholders; real secret must NOT appear.
	if env["ANTHROPIC_API_KEY"] != "__anthropic_api_key__" {
		t.Errorf("ANTHROPIC_API_KEY=%q, want placeholder", env["ANTHROPIC_API_KEY"])
	}
	if env["SLACK_MCP_XOXP_TOKEN"] != "__slack_mcp_xoxp_token__" {
		t.Errorf("SLACK placeholder wrong: %q", env["SLACK_MCP_XOXP_TOKEN"])
	}
	// Non-brokered secret keeps real value (Discord gateway is unbrokerable).
	if env["DISCORD_TOKEN"] != "discord-REAL" {
		t.Errorf("DISCORD_TOKEN should stay real, got %q", env["DISCORD_TOKEN"])
	}
	for k, v := range env {
		if v == "sk-ant-REAL" || v == "xoxp-REAL" {
			t.Errorf("real brokered secret leaked into env key %s", k)
		}
	}
	// Connection + CA trust.
	if env["HTTPS_PROXY"] != "https://av_agt_tok:anthropic@127.0.0.1:14322" {
		t.Errorf("HTTPS_PROXY=%q", env["HTTPS_PROXY"])
	}
	if env["AGENT_VAULT_TOKEN"] != "av_agt_tok" || env["AGENT_VAULT_VAULT"] != "anthropic" {
		t.Errorf("AGENT_VAULT_* wrong: %v", env)
	}
	if env["NODE_EXTRA_CA_CERTS"] != "/etc/clem/agent-vault-ca.pem" {
		t.Errorf("NODE_EXTRA_CA_CERTS=%q", env["NODE_EXTRA_CA_CERTS"])
	}
	// git tunnels through the TLS proxy, so it must trust the agent-vault CA for
	// the proxy connection too (http.proxySSLCAInfo) — else every git op fails.
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "http.proxySSLCAInfo" ||
		env["GIT_CONFIG_VALUE_0"] != "/etc/clem/agent-vault-ca.pem" {
		t.Errorf("git proxy CA config missing/wrong: COUNT=%q KEY_0=%q VALUE_0=%q",
			env["GIT_CONFIG_COUNT"], env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"])
	}
}

func TestInstallAgentVault_PinnedDownload(t *testing.T) {
	stub := withStub(t)
	if err := InstallAgentVault(); err != nil {
		t.Fatalf("InstallAgentVault: %v", err)
	}
	var script string
	for _, c := range stub.calls {
		if c[0] == "bash" && len(c) >= 3 && strings.Contains(c[2], "releases/download") {
			script = c[2]
		}
	}
	for _, want := range []string{AgentVaultVersion, "Infisical/agent-vault", "install -m 0755 agent-vault /usr/local/bin/agent-vault"} {
		if !strings.Contains(script, want) {
			t.Errorf("agent-vault install script missing %q:\n%s", want, script)
		}
	}
}

func TestWriteSystemdEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.env")
	if err := WriteSystemdEnvFile(path, map[string]string{
		"ES_PASSWORD": "p@ss:w/rd",
		"ES_USER":     "elastic",
	}); err != nil {
		t.Fatalf("WriteSystemdEnvFile: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted, literal KEY=value (no export, no quoting), one per line.
	want := "ES_PASSWORD=p@ss:w/rd\nES_USER=elastic\n"
	if string(b) != want {
		t.Errorf("content = %q, want %q", string(b), want)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteSystemdEnvFile_RejectsNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.env")
	if err := WriteSystemdEnvFile(path, map[string]string{"K": "a\nb"}); err == nil {
		t.Fatal("expected error for newline in secret value")
	}
}

func TestWriteWranglerConfig_LiteralStringsPreserveSpecialChars(t *testing.T) {
	homeDir := t.TempDir()
	// These chars corrupted the file when it used TOML basic strings:
	// " broke the string delimiter and \ was treated as an escape prefix.
	secrets := map[string]string{
		"WRANGLER_OAUTH_TOKEN":   `tok"en\value`,
		"WRANGLER_REFRESH_TOKEN": `ref\res"h`,
		"WRANGLER_EXPIRATION":    "2099-01-01T00:00:00Z",
	}
	if err := WriteWranglerConfig("testuser", homeDir, secrets); err != nil {
		t.Fatalf("WriteWranglerConfig: %v", err)
	}
	configPath := filepath.Join(homeDir, ".config", ".wrangler", "config", "default.toml")
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	got := string(b)
	// Values must appear verbatim inside TOML literal strings (single-quoted).
	if !strings.Contains(got, `oauth_token = 'tok"en\value'`) {
		t.Errorf("oauth_token not written as literal string, got:\n%s", got)
	}
	if !strings.Contains(got, `refresh_token = 'ref\res"h'`) {
		t.Errorf("refresh_token not written as literal string, got:\n%s", got)
	}
	info, _ := os.Stat(configPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWriteWranglerConfig_SkipsWhenSecretsAbsent(t *testing.T) {
	homeDir := t.TempDir()
	if err := WriteWranglerConfig("testuser", homeDir, map[string]string{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	configPath := filepath.Join(homeDir, ".config", ".wrangler", "config", "default.toml")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Error("config file should not be written when secrets absent")
	}
}
