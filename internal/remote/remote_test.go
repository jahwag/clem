package remote

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// chdirTempGitRepo creates a temp git repo with the given origin remote and
// makes it the working directory for the test (RepoName/CloneURL read the
// remote of the current directory). Restores the previous cwd on cleanup.
// os.Chdir (not t.Chdir) because the module still builds with Go 1.22.
func chdirTempGitRepo(t *testing.T, remoteURL string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH - skipping integration test")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if remoteURL != "" {
		run("remote", "add", "origin", remoteURL)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prev); err != nil {
			t.Fatalf("restoring cwd: %v", err)
		}
	})
}

func TestRepoName_SSHAndHTTPSRemotes(t *testing.T) {
	cases := []struct {
		remote string
		want   string
	}{
		{"git@github.com:org/my-team.git", "my-team"},
		{"git@github.com:org/my-team", "my-team"},
		{"https://github.com/org/my-team.git", "my-team"},
		{"https://github.com/org/my-team", "my-team"},
	}
	for _, tc := range cases {
		t.Run(tc.remote, func(t *testing.T) {
			chdirTempGitRepo(t, tc.remote)
			got, err := RepoName()
			if err != nil {
				t.Fatalf("RepoName: %v", err)
			}
			if got != tc.want {
				t.Errorf("RepoName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepoName_NoRemoteErrors(t *testing.T) {
	chdirTempGitRepo(t, "")
	if _, err := RepoName(); err == nil {
		t.Fatal("RepoName should error when origin is not configured")
	}
}

func TestCloneURL_ConvertsSSHAndInjectsToken(t *testing.T) {
	chdirTempGitRepo(t, "git@github.com:org/my-team.git")

	plain, err := CloneURL("")
	if err != nil {
		t.Fatalf("CloneURL(\"\"): %v", err)
	}
	if plain != "https://github.com/org/my-team.git" {
		t.Errorf("tokenless CloneURL = %q", plain)
	}

	withTok, err := CloneURL("ghp_faketoken") // clem:allow-secret
	if err != nil {
		t.Fatalf("CloneURL(token): %v", err)
	}
	if withTok != "https://oauth2:ghp_faketoken@github.com/org/my-team.git" { // clem:allow-secret
		t.Errorf("tokenized CloneURL = %q", withTok)
	}
}

func TestCloneURL_TokenWithReservedCharsIsEscaped(t *testing.T) {
	// url.UserPassword must percent-encode reserved characters so a token
	// can never corrupt the URL structure (e.g. inject a different host).
	chdirTempGitRepo(t, "https://github.com/org/my-team.git")
	got, err := CloneURL("a:b@evil.example/")
	if err != nil {
		t.Fatalf("CloneURL: %v", err)
	}
	if !strings.HasSuffix(got, "@github.com/org/my-team.git") {
		t.Errorf("token must not alter the target host, got %q", got)
	}
	if strings.Contains(got, "@evil.example") {
		t.Errorf("reserved chars in token leaked into URL structure: %q", got)
	}
}

func TestAgeKeyPath_UnderHomeConfig(t *testing.T) {
	p := AgeKeyPath()
	if !strings.HasSuffix(p, ".config/sops/age/keys.txt") {
		t.Errorf("AgeKeyPath = %q, want ~/.config/sops/age/keys.txt", p)
	}
}

func TestRemoteCloneCmd_NoTokenInURL(t *testing.T) {
	cmd := remoteCloneCmd("clem", "https://github.com/org/clem.git", "ghp_secrettoken") // clem:allow-secret
	if strings.Contains(cmd, "oauth2:") || strings.Contains(cmd, "ghp_secrettoken@") {
		t.Fatalf("token must not be embedded in clone URL:\n%s", cmd)
	}
	// GitHub rejects Bearer for classic PATs; the clone must use HTTP Basic
	// with the token as password (x-access-token username).
	wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghp_secrettoken")) // clem:allow-secret
	if strings.Contains(cmd, "bearer") || strings.Contains(cmd, "ghp_secrettoken") {
		t.Fatalf("clone cmd must not use bearer or expose the raw token:\n%s", cmd)
	}
	for _, want := range []string{
		`http.extraheader`,
		wantAuth,
		`clone https://github.com/org/clem.git`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("clone cmd missing %q:\n%s", want, cmd)
		}
	}
}

func TestRemoteCloneCmd_PublicRepoNoHeader(t *testing.T) {
	cmd := remoteCloneCmd("clem", "https://github.com/org/clem.git", "")
	if strings.Contains(cmd, "extraheader") {
		t.Fatalf("public clone should not set extraheader:\n%s", cmd)
	}
}

func TestProvision_AbortsBeforeStep3WhenCloneFails(t *testing.T) {
	chdirTempGitRepo(t, "git@github.com:org/clem.git")
	oldSSH := remoteSSH
	oldSCP := remoteSCP
	defer func() {
		remoteSSH = oldSSH
		remoteSCP = oldSCP
	}()
	var calls []string
	remoteSSH = func(host, cmd string) error {
		calls = append(calls, cmd)
		if strings.Contains(cmd, "mkdir -p ~/.config/sops/age") {
			return nil
		}
		if strings.Contains(cmd, "git ") {
			return fmt.Errorf("simulated clone failure")
		}
		return fmt.Errorf("unexpected ssh: %s", cmd)
	}
	remoteSCP = func(localPath, host, remotePath string) error { return nil }

	err := Provision("myhost", "ghp_secrettoken") // clem:allow-secret
	if err == nil {
		t.Fatal("expected Provision to fail when clone fails")
	}
	if !strings.Contains(err.Error(), "cloning repo") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, cmd := range calls {
		if strings.Contains(cmd, "clem provision") {
			t.Fatalf("step 3/3 must not run after clone failure, saw: %q", cmd)
		}
	}
}

func TestBug118_MaliciousRepoNameInjectsShellCommand(t *testing.T) {
	malicious := `legit; touch /tmp/pwned #`
	// Same pattern as Provision/Login — repoName is unquoted in SSH shell.
	cmd := fmt.Sprintf("cd ~/%s && clem provision", malicious)
	if !strings.Contains(cmd, "; touch") {
		t.Fatalf("expected injectable command, got %q", cmd)
	}
	if err := validateRepoName(malicious); err == nil {
		t.Fatal("validateRepoName should reject shell metacharacters")
	}
}

func TestRepoName_MaliciousRemoteRejectedByValidation(t *testing.T) {
	chdirTempGitRepo(t, "git@github.com:org/legit;touch.git")
	name, err := RepoName()
	if err != nil {
		t.Fatalf("RepoName: %v", err)
	}
	if name != "legit;touch" {
		t.Fatalf("RepoName = %q, want %q", name, "legit;touch")
	}
	if _, err := ValidatedRepoName(); err == nil {
		t.Fatal("ValidatedRepoName should reject malicious repo name from origin URL")
	}
}

func TestValidateRepoName_AcceptsNormalNames(t *testing.T) {
	for _, name := range []string{"clem", "my-team", "my_team", "repo.v2"} {
		if err := validateRepoName(name); err != nil {
			t.Errorf("validateRepoName(%q): %v", name, err)
		}
	}
}

func TestValidateRepoName_RejectsUnsafe(t *testing.T) {
	for _, name := range []string{
		`legit; touch /tmp/pwned #`,
		"$(id)",
		"repo name",
		"../etc",
		"",
	} {
		if err := validateRepoName(name); err == nil {
			t.Errorf("validateRepoName(%q) expected error", name)
		}
	}
}

func TestValidateAgentKey_AcceptsNormal(t *testing.T) {
	for _, key := range []string{"lead", "worker", "eng", "agent-1"} {
		if err := validateAgentKey(key); err != nil {
			t.Errorf("validateAgentKey(%q): %v", key, err)
		}
	}
}

func TestValidateAgentKey_RejectsUnsafe(t *testing.T) {
	for _, key := range []string{
		`lead; touch /tmp/pwned`,
		"$(id)",
		"agent name",
		"../etc",
		"--remote",
		"-h",
		"Lead",
		"my_agent",
		strings.Repeat("a", 32),
		"",
	} {
		if err := validateAgentKey(key); err == nil {
			t.Errorf("validateAgentKey(%q) expected error", key)
		}
	}
}

func TestLogin_ForwardsAgentsToRemoteCommand(t *testing.T) {
	chdirTempGitRepo(t, "git@github.com:org/clem.git")
	old := remoteSSHT
	defer func() { remoteSSHT = old }()

	var gotHost, gotCmd string
	remoteSSHT = func(host, cmd string) error {
		gotHost = host
		gotCmd = cmd
		return nil
	}

	if err := Login("myhost", []string{"lead", "worker"}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if gotHost != "myhost" {
		t.Errorf("SSH host = %q, want myhost", gotHost)
	}
	if !strings.Contains(gotCmd, "clem login -- lead worker") {
		t.Errorf("command %q does not include option-terminated agent args", gotCmd)
	}
}

func TestLogin_NoAgents_RunsAllLogin(t *testing.T) {
	chdirTempGitRepo(t, "git@github.com:org/clem.git")
	old := remoteSSHT
	defer func() { remoteSSHT = old }()

	var gotCmd string
	remoteSSHT = func(host, cmd string) error {
		gotCmd = cmd
		return nil
	}

	if err := Login("myhost", nil); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.HasSuffix(gotCmd, "clem login") {
		t.Errorf("command %q should end with bare clem login", gotCmd)
	}
}

func TestLogin_InvalidAgentKeyRejected(t *testing.T) {
	chdirTempGitRepo(t, "git@github.com:org/clem.git")
	old := remoteSSHT
	defer func() { remoteSSHT = old }()
	called := false
	remoteSSHT = func(host, cmd string) error {
		called = true
		return nil
	}

	err := Login("myhost", []string{"--remote"})
	if err == nil {
		t.Fatal("expected error for dash-prefixed agent key")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Errorf("error %q should explain the valid agent-key grammar", err)
	}
	if called {
		t.Fatal("invalid agent key must be rejected before SSH")
	}
}
