package agent

import (
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

// writeTestableHook writes a copy of prePushHookContent to a temp file with
// the $diff_cmd substring replaced by a stubbed command that emits a fixed
// payload. Returns the path.
func writeTestableHook(t *testing.T, stubDiffCmd string) string {
	t.Helper()
	dir := t.TempDir()
	hookPath := filepath.Join(dir, "pre-push")
	patched := strings.Replace(prePushHookContent, "$diff_cmd", stubDiffCmd, 1)
	if err := os.WriteFile(hookPath, []byte(patched), 0755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return hookPath
}
