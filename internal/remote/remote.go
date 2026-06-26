package remote

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RepoName returns the repository name derived from the local git remote URL.
func RepoName() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("could not read git remote origin — are you in a git repo?")
	}
	raw := strings.TrimSpace(string(out))
	// handles both SSH (git@github.com:org/repo.git) and HTTPS
	if strings.HasPrefix(raw, "git@") {
		// git@github.com:org/repo.git -> repo
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(filepath.Base(parts[1]), ".git"), nil
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("could not parse remote URL: %s", raw)
	}
	return strings.TrimSuffix(filepath.Base(u.Path), ".git"), nil
}

// CloneURL converts the local git remote URL to an HTTPS URL with a token injected.
// token may be empty if the repo is public.
func CloneURL(token string) (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("could not read git remote origin")
	}
	raw := strings.TrimSpace(string(out))

	var httpsURL string
	if strings.HasPrefix(raw, "git@") {
		// git@github.com:org/repo.git -> https://github.com/org/repo.git
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("unexpected SSH remote format: %s", raw)
		}
		host := strings.TrimPrefix(parts[0], "git@")
		httpsURL = fmt.Sprintf("https://%s/%s", host, parts[1])
	} else {
		httpsURL = raw
	}

	if token == "" {
		return httpsURL, nil
	}

	u, err := url.Parse(httpsURL)
	if err != nil {
		return "", fmt.Errorf("parsing remote URL: %w", err)
	}
	u.User = url.UserPassword("oauth2", token)
	return u.String(), nil
}

// SSH runs a command on the remote host, streaming output to the terminal.
func SSH(host, command string) error {
	cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=accept-new", host, command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// remoteSSH is the SSH implementation used by remote provisioning helpers.
// Tests may replace it to simulate failures.
var remoteSSH = SSH

// SSHT runs a command on the remote host with a TTY allocated (required for interactive prompts).
func SSHT(host, command string) error {
	cmd := exec.Command("ssh", "-t", "-o", "StrictHostKeyChecking=accept-new", host, command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CopyFile copies a local file to the remote host via scp.
func CopyFile(localPath, host, remotePath string) error {
	return remoteSCP(localPath, host, remotePath)
}

func scpCopy(localPath, host, remotePath string) error {
	cmd := exec.Command("scp", "-o", "StrictHostKeyChecking=accept-new", localPath, fmt.Sprintf("%s:%s", host, remotePath))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// remoteSCP is the SCP implementation used by remote provisioning helpers.
// Tests may replace it to simulate failures.
var remoteSCP = scpCopy

// AgeKeyPath returns the default local age private key path.
func AgeKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sops", "age", "keys.txt")
}

// Provision runs the full remote provisioning sequence:
// 1. copy age key
// 2. clone repo
// 3. clem provision
func Provision(host, ghToken string) error {
	repoName, err := ValidatedRepoName()
	if err != nil {
		return err
	}

	fmt.Printf("Remote: %s\n", host)
	fmt.Printf("Repo:   %s\n\n", repoName)

	fmt.Println("--- step 1/3: copy age key")
	if err := remoteSSH(host, "mkdir -p ~/.config/sops/age"); err != nil {
		return fmt.Errorf("creating age dir on remote: %w", err)
	}
	if err := CopyFile(AgeKeyPath(), host, "~/.config/sops/age/keys.txt"); err != nil {
		return fmt.Errorf("copying age key: %w\nManual: scp ~/.config/sops/age/keys.txt %s:~/.config/sops/age/keys.txt", err, host)
	}

	fmt.Println("\n--- step 2/3: clone repo")
	cleanURL, err := CloneURL("")
	if err != nil {
		return err
	}
	cloneCmd := remoteCloneCmd(repoName, cleanURL, ghToken)
	if err := remoteSSH(host, cloneCmd); err != nil {
		return fmt.Errorf("cloning repo: %w\nManual: ssh %s 'cd ~ && git clone %s ~/%s'", err, host, cleanURL, repoName)
	}

	fmt.Println("\n--- step 3/3: clem provision")
	provisionCmd := fmt.Sprintf("cd ~/%s && clem provision", repoName)
	if err := remoteSSH(host, provisionCmd); err != nil {
		return fmt.Errorf("remote provision: %w\nManual: ssh %s 'cd ~/%s && clem provision'", err, host, repoName)
	}

	fmt.Printf("\nDone. Run: clem login --remote %s\n", host)
	return nil
}

// Login runs clem login on the remote host with a TTY for interactive OAuth.
func Login(host string) error {
	repoName, err := ValidatedRepoName()
	if err != nil {
		return err
	}
	fmt.Printf("Remote: %s\n\n", host)
	loginCmd := fmt.Sprintf("cd ~/%s && clem login", repoName)
	if err := SSHT(host, loginCmd); err != nil {
		return fmt.Errorf("remote login: %w\nManual: ssh -t %s 'cd ~/%s && clem login'", err, host, repoName)
	}
	return nil
}
