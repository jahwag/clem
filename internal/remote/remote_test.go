package remote

import (
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
