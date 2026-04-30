package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jahwag/clem/internal/config"
)

// Executor runs a system command and returns its combined output.
// The package-level sys variable holds the production implementation;
// tests may replace it with a stub to avoid requiring root or real binaries.
type Executor interface {
	Run(name string, args ...string) ([]byte, error)
}

// OSExecutor is the production Executor backed by exec.Command.
type OSExecutor struct{}

// Run executes name with args and returns combined stdout+stderr.
func (OSExecutor) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// sys is the active Executor. Replaced in tests to avoid root/binary requirements.
var sys Executor = OSExecutor{}

// ghHTTPClient performs GitHub API calls. Replaced in tests to avoid real network calls.
var ghHTTPClient = &http.Client{}

// RegisterSSHSigningKey registers pubKey on the agent's GitHub account as a
// signing key via POST /user/ssh_signing_keys. Requires a GH_TOKEN with the
// write:public_key scope. Idempotent: returns nil if the key is already registered.
func RegisterSSHSigningKey(pubKey, ghToken string) error {
	if ghToken == "" {
		return fmt.Errorf("GH_TOKEN required to register SSH signing key; grant write:public_key scope to the agent PAT")
	}

	type payload struct {
		Title string `json:"title"`
		Key   string `json:"key"`
	}
	body, err := json.Marshal(payload{Title: "clem-signing", Key: pubKey})
	if err != nil {
		return fmt.Errorf("marshaling signing key payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.github.com/user/ssh_signing_keys", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating signing key request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ghToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := ghHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("registering SSH signing key: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated {
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnprocessableEntity &&
		strings.Contains(string(respBody), "key is already in use") {
		return nil
	}

	return fmt.Errorf("GitHub /user/ssh_signing_keys returned %d: %s", resp.StatusCode, respBody)
}

// EnsureUser creates the OS user if it doesn't already exist.
func EnsureUser(username string) error {
	if _, err := sys.Run("id", username); err == nil {
		fmt.Printf("  user %s already exists\n", username)
		return nil
	}
	fmt.Printf("  creating user %s\n", username)
	out, err := sys.Run("useradd",
		"--create-home",
		"--shell", "/bin/bash",
		"--comment", "clem managed agent",
		username,
	)
	if err != nil {
		return fmt.Errorf("useradd %s: %w\n%s", username, err, out)
	}
	return nil
}

// WriteEnvFile writes decrypted secrets to <homeDir>/.env with mode 0600.
// Also writes a global gitignore that blocks .env, .git-credentials, and
// secrets.sops.yaml from accidental commits.
func WriteEnvFile(username, homeDir string, secrets map[string]string) error {
	envPath := filepath.Join(homeDir, ".env")

	var sb strings.Builder
	for k, v := range secrets {
		sb.WriteString(fmt.Sprintf("export %s=%q\n", k, v))
	}

	if err := os.WriteFile(envPath, []byte(sb.String()), 0600); err != nil {
		return fmt.Errorf("writing .env for %s: %w", username, err)
	}

	if out, err := sys.Run("chown", fmt.Sprintf("%s:%s", username, username), envPath); err != nil {
		return fmt.Errorf("chown .env for %s: %w\n%s", username, err, out)
	}

	// Defense: write a global gitignore that blocks secret-bearing files.
	// Even if the agent runs `git add .env` from any directory, this prevents staging.
	globalIgnore := filepath.Join(homeDir, ".gitignore_global")
	ignoreContent := `.env
.env.*
.git-credentials
secrets.sops.yaml
id_ed25519
id_rsa
*.pem
*.key
`
	if err := os.WriteFile(globalIgnore, []byte(ignoreContent), 0644); err != nil {
		return fmt.Errorf("writing gitignore_global: %w", err)
	}
	if err := chownToUser(globalIgnore, username); err != nil {
		return fmt.Errorf("chowning %s: %w", globalIgnore, err)
	}

	// Write/update ~/.gitconfig directly to avoid sudo subshell quoting issues
	gitConfigPath := filepath.Join(homeDir, ".gitconfig")
	existing, _ := os.ReadFile(gitConfigPath)
	if !strings.Contains(string(existing), "excludesfile") {
		appended := string(existing) + fmt.Sprintf("\n[core]\n\texcludesfile = %s\n", globalIgnore)
		if err := os.WriteFile(gitConfigPath, []byte(appended), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", gitConfigPath, err)
		}
		if err := chownToUser(gitConfigPath, username); err != nil {
			return fmt.Errorf("chowning %s: %w", gitConfigPath, err)
		}
	}

	return nil
}

// chownToUser sets owner/group on path to username:username. Fatal for the
// caller because an agent-owned file left root-owned will silently break
// subsequent agent operations (git reads, claude writes).
func chownToUser(path, username string) error {
	out, err := sys.Run("chown", fmt.Sprintf("%s:%s", username, username), path)
	if err != nil {
		return fmt.Errorf("chown %s: %w\n%s", path, err, out)
	}
	return nil
}

// SecretPatternRegex is the ERE alternation the pre-push hook uses to detect
// credentials in diffs. Exported for testing and for any other code that
// wants to reuse the same pattern set. Covers the classes of exfil most
// likely to succeed: GitHub tokens (classic, OAuth, App server, fine-grained),
// Slack tokens (bot / user / refresh / app), AWS access keys, age/sops keys,
// OpenSSH/RSA/EC/DSA private-key blocks. Pattern set is deliberately tight -
// false positives block pushes, which is annoying but safe.
const SecretPatternRegex = `ghp_[A-Za-z0-9]{36}|gho_[A-Za-z0-9]{36}|ghs_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{70,}|sk-[A-Za-z0-9_-]{20,}|xox[bapr]-[0-9A-Za-z-]{10,}|AKIA[0-9A-Z]{16}|AGE-SECRET-KEY-1[A-Z0-9]+|-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`

// SecretCodePatternRegex is the ERE alternation the pre-push hook uses as a
// second pass: it detects code that reads protected secret env vars at runtime
// (Go, Python, Node). Exported so tests share one source of truth with the
// bash hook. Shell variable expansions are excluded — false-positive rate is
// too high in shell scripts that legitimately forward these vars.
// To skip this pass for a repo where the reads are intentional and reviewed,
// push with CLEM_HOOK_SKIP_CODE_SCAN=1 in the environment.
// Single-quoted Python access (os.environ['KEY']) is intentionally excluded:
// the single quote cannot appear inside a bash single-quoted assignment without
// complex escaping, and double-quoted access is the dominant style for secrets.
const SecretCodePatternRegex = `os\.Getenv\("(GH_TOKEN|DISCORD_TOKEN|ANTHROPIC_API_KEY|AWS_SECRET_ACCESS_KEY|SLACK_MCP_XOXP_TOKEN)"\)|os\.environ\["(GH_TOKEN|DISCORD_TOKEN|ANTHROPIC_API_KEY|AWS_SECRET_ACCESS_KEY|SLACK_MCP_XOXP_TOKEN)"\]|process\.env\.(GH_TOKEN|DISCORD_TOKEN|ANTHROPIC_API_KEY|AWS_SECRET_ACCESS_KEY|SLACK_MCP_XOXP_TOKEN)`

// UnicodeTrapRegex matches Unicode code points commonly used to smuggle
// hidden instructions or text past human review. Zero-width chars (U+200B-F),
// bidi overrides (U+2028-E), and BOM (U+FEFF) should never appear in source
// code or config and flag a likely injection attempt when they do.
const UnicodeTrapRegex = `[\x{200B}-\x{200F}\x{2028}-\x{202E}\x{FEFF}]`

// prePushHookContent is the pre-push hook installed for every agent user.
// Pure bash + grep + base64 (from coreutils); no gitleaks dependency. The
// regexes come from SecretPatternRegex, SecretCodePatternRegex, and
// UnicodeTrapRegex so Go tests and the bash hook share one source of truth.
//
// Passes:
//  1. Literal credential patterns (tokens, keys, PEM blocks).
//  2. Base64-encoded secrets: long base64 runs are decoded and re-scanned
//     against SecretPatternRegex. Closes encoded-exfil bypass.
//  3. Unicode traps: zero-width + bidi-override + BOM mid-content. Closes
//     hidden-instruction-smuggling bypass.
//  4. Code that reads protected secret env vars (Go/Python/Node). Closes
//     indirect runtime-exfil bypass. Skip with CLEM_HOOK_SKIP_CODE_SCAN=1.
var prePushHookContent = fmt.Sprintf(`#!/bin/bash
# Installed by clem provision. Do not edit by hand - will be overwritten.
# Pass 1: literal credential patterns (tokens, keys, PEM blocks).
# Pass 2: base64-encoded secrets (decoded + re-scanned).
# Pass 3: Unicode traps (zero-width / bidi / BOM - hidden-instruction smuggling).
# Pass 4: code that reads protected secret env vars (Go/Python/Node).
#         Skip with: CLEM_HOOK_SKIP_CODE_SCAN=1 git push

zero="0000000000000000000000000000000000000000"
patterns='%s'
code_patterns='%s'
unicode_traps='%s'

while read local_ref local_sha remote_ref remote_sha; do
  [ "$local_sha" = "$zero" ] && continue
  if [ "$remote_sha" = "$zero" ]; then
    # new branch: scan all reachable commits not yet on remote
    range="$local_sha"
    diff_cmd="git log --all --not --remotes --pretty=format: -p $local_sha"
  else
    range="${remote_sha}..${local_sha}"
    diff_cmd="git diff $range"
  fi
  diff=$($diff_cmd 2>/dev/null)

  # Pass 1: direct literal secret match
  hits=$(echo "$diff" | grep -E "$patterns" | head -3)
  if [ -n "$hits" ]; then
    echo "clem pre-push hook: push blocked - secret pattern detected in $range" >&2
    echo "$hits" | sed 's/^/  /' >&2
    echo "" >&2
    echo "Rotate the leaked credential immediately if it is real. To override" >&2
    echo "for a false positive, push with --no-verify (think first)." >&2
    exit 1
  fi

  # Pass 2: base64-decode + re-scan. Finds long base64 runs, decodes each,
  # greps the decoded bytes for secret patterns. Skips chunks that fail to
  # decode (normal diff content).
  while IFS= read -r chunk; do
    [ -z "$chunk" ] && continue
    decoded=$(echo "$chunk" | base64 -d 2>/dev/null) || continue
    if echo "$decoded" | grep -qE "$patterns"; then
      echo "clem pre-push hook: push blocked - base64-encoded secret detected in $range" >&2
      echo "  $chunk -> decoded hit" >&2
      exit 1
    fi
  done < <(echo "$diff" | grep -oE '[A-Za-z0-9+/]{40,}={0,2}')

  # Pass 3: unicode traps for hidden-instruction smuggling.
  uhits=$(echo "$diff" | grep -P "$unicode_traps" | head -3)
  if [ -n "$uhits" ]; then
    echo "clem pre-push hook: push blocked - unicode control/override characters detected in $range (possible prompt-injection smuggling)" >&2
    echo "$uhits" | sed 's/^/  /' >&2
    exit 1
  fi

  # Pass 4: indirect runtime exfil via os.Getenv on protected names.
  if [ "${CLEM_HOOK_SKIP_CODE_SCAN:-0}" != "1" ]; then
    code_hits=$(echo "$diff" | grep -E "$code_patterns" | head -3)
    if [ -n "$code_hits" ]; then
      echo "clem pre-push hook: push blocked - diff reads a protected secret env var in $range" >&2
      echo "$code_hits" | sed 's/^/  /' >&2
      echo "" >&2
      echo "Set CLEM_HOOK_SKIP_CODE_SCAN=1 if this read is intentional and reviewed." >&2
      exit 1
    fi
  fi
done
exit 0
`, SecretPatternRegex, SecretCodePatternRegex, UnicodeTrapRegex)

// ConfigureGit writes SSH commit-signing configuration and optionally the git
// user identity to the agent's ~/.gitconfig. Idempotent — safe to call every
// provision. pubKey is the agent's ed25519 public key (returned by EnsureSSHKey).
// gitName and gitEmail, when non-empty, set git user.name / user.email; existing
// values are preserved (operator-set identity is never overwritten).
func ConfigureGit(username, homeDir, pubKey, gitName, gitEmail string) error {
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("creating .ssh dir: %w", err)
	}

	commitEmail := username + "@clem"
	allowedSignersPath := filepath.Join(sshDir, "allowed_signers")
	if err := os.WriteFile(allowedSignersPath, []byte(commitEmail+" "+pubKey+"\n"), 0644); err != nil {
		return fmt.Errorf("writing allowed_signers: %w", err)
	}
	if err := chownToUser(allowedSignersPath, username); err != nil {
		return fmt.Errorf("chowning allowed_signers: %w", err)
	}

	gitConfigPath := filepath.Join(homeDir, ".gitconfig")
	existing, _ := os.ReadFile(gitConfigPath)
	content := string(existing)

	var extra string
	if !strings.Contains(content, "gpgsign") {
		signingKey := filepath.Join(sshDir, "id_ed25519.pub")
		extra += fmt.Sprintf(
			"\n[user]\n\tsigningkey = %s\n[commit]\n\tgpgsign = true\n[gpg]\n\tformat = ssh\n[gpg \"ssh\"]\n\tallowedSignersFile = %s",
			signingKey, allowedSignersPath,
		)
	}
	var identityLines []string
	if gitName != "" && !strings.Contains(content, "\tname = ") {
		identityLines = append(identityLines, "\tname = "+gitName)
	}
	if gitEmail != "" && !strings.Contains(content, "\temail = ") {
		identityLines = append(identityLines, "\temail = "+gitEmail)
	}
	if len(identityLines) > 0 {
		extra += "\n[user]\n" + strings.Join(identityLines, "\n")
	}
	if extra == "" {
		return nil
	}

	if err := os.WriteFile(gitConfigPath, []byte(content+extra+"\n"), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", gitConfigPath, err)
	}
	return chownToUser(gitConfigPath, username)
}

// InstallGitHooks writes a global pre-push hook for the agent user and points
// their git config at it via core.hooksPath. Idempotent - safe to call every
// provision. The hook rejects pushes whose diff contains credential patterns,
// as a client-side defense layer on top of GitHub's push protection.
func InstallGitHooks(username, homeDir string) error {
	hooksDir := filepath.Join(homeDir, ".config", "git", "hooks")
	hookPath := filepath.Join(hooksDir, "pre-push")

	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("creating hooks dir: %w", err)
	}
	if err := os.WriteFile(hookPath, []byte(prePushHookContent), 0755); err != nil {
		return fmt.Errorf("writing pre-push hook: %w", err)
	}
	ChownPath(filepath.Join(homeDir, ".config"), username)

	// Point the user's git at the global hooks dir so every repo clone uses it.
	gitConfigPath := filepath.Join(homeDir, ".gitconfig")
	existing, _ := os.ReadFile(gitConfigPath)
	if !strings.Contains(string(existing), "hooksPath") {
		appended := string(existing) + fmt.Sprintf("\n[core]\n\thooksPath = %s\n", hooksDir)
		if err := os.WriteFile(gitConfigPath, []byte(appended), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", gitConfigPath, err)
		}
		if err := chownToUser(gitConfigPath, username); err != nil {
			return fmt.Errorf("chowning %s: %w", gitConfigPath, err)
		}
	}
	return nil
}

// WriteWranglerConfig writes a wrangler OAuth config for the agent if the
// matching env vars are present in the secrets map. Idempotent — safe to call
// every provision. The wrangler binary auto-refreshes the OAuth token using
// the refresh token, so this stays valid as long as the refresh token does.
func WriteWranglerConfig(username, homeDir string, secrets map[string]string) error {
	oauth := secrets["WRANGLER_OAUTH_TOKEN"]
	refresh := secrets["WRANGLER_REFRESH_TOKEN"]
	expiration := secrets["WRANGLER_EXPIRATION"]
	if oauth == "" || refresh == "" {
		return nil // not configured for this agent
	}

	configDir := filepath.Join(homeDir, ".config", ".wrangler", "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("creating wrangler config dir: %w", err)
	}

	configContent := fmt.Sprintf(`oauth_token = "%s"
expiration_time = "%s"
refresh_token = "%s"
scopes = [ "account:read", "user:read", "workers:write", "workers_kv:write", "workers_routes:write", "workers_scripts:write", "workers_tail:read", "d1:write", "pages:write", "zone:read", "ssl_certs:write", "ai:write", "queues:write", "pipelines:write", "secrets_store:write", "containers:write", "cloudchamber:write", "connectivity:admin", "offline_access" ]
`, oauth, expiration, refresh)

	configPath := filepath.Join(configDir, "default.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		return fmt.Errorf("writing wrangler config: %w", err)
	}
	ChownPath(filepath.Join(homeDir, ".config"), username)
	return nil
}

// EnsureSSHKey generates an ed25519 SSH keypair for the agent if one doesn't exist.
// The public key is returned so it can be displayed/distributed.
func EnsureSSHKey(username, homeDir string) (string, error) {
	sshDir := filepath.Join(homeDir, ".ssh")
	keyPath := filepath.Join(sshDir, "id_ed25519")
	pubPath := keyPath + ".pub"

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return "", fmt.Errorf("creating .ssh dir: %w", err)
	}
	if err := chownToUser(sshDir, username); err != nil {
		return "", fmt.Errorf("chowning %s: %w", sshDir, err)
	}
	if out, err := sys.Run("chmod", "700", sshDir); err != nil {
		return "", fmt.Errorf("chmod 700 %s: %w\n%s", sshDir, err, out)
	}

	if _, err := os.Stat(keyPath); err == nil {
		// Already exists; return the existing public key
		data, err := os.ReadFile(pubPath)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}

	out, err := sys.Run("sudo", "-u", username, "ssh-keygen",
		"-t", "ed25519",
		"-N", "",
		"-f", keyPath,
		"-C", username+"@clem",
	)
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: %w\n%s", err, out)
	}

	data, err := os.ReadFile(pubPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteSettings writes Claude Code settings to skip MCP trust dialog and
// first-run onboarding prompts.
//
// Claude Code stores two flavours of config:
//   - ~/.claude/settings.json     — user-level flags + permissions
//   - ~/.claude.json              — app-level state (onboarding gates, per-project trust)
//
// We write both. Without ~/.claude.json, fresh agents hit the "Security notes —
// Press Enter" screen and the "Quick safety check: trust this folder?" prompt
// before the runner can inject its prompt, causing lost first iterations.
func WriteSettings(username, homeDir string) error {
	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("creating .claude dir: %w", err)
	}

	// includeCoAuthoredBy=false suppresses the "Co-authored-by: Claude ..."
	// trailer Claude Code otherwise appends to commits it creates. Agents
	// should author commits under their own identity, not leak that an LLM
	// drove them - clem PRs go through normal human review regardless.
	settings := `{
  "hasTrustDialogAccepted": true,
  "hasCompletedProjectOnboarding": true,
  "skipDangerousModePermissionPrompt": true,
  "includeCoAuthoredBy": false
}
`
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(settings), 0644); err != nil {
		return fmt.Errorf("writing settings.json: %w", err)
	}

	// ~/.claude.json gates the top-level onboarding screens. A future-dated
	// lastOnboardingVersion prevents the next claude upgrade from re-prompting.
	// projects.<workdir>.hasTrustDialogAccepted dismisses the folder-trust
	// dialog for the agent's working directory.
	workDirKey := filepath.Join(homeDir, projectFromUsername(username))
	appState := fmt.Sprintf(`{
  "hasCompletedOnboarding": true,
  "lastOnboardingVersion": "99.0.0",
  "bypassPermissionsModeAccepted": true,
  "projects": {
    %q: {
      "hasTrustDialogAccepted": true,
      "projectOnboardingSeenCount": 1,
      "allowedTools": [],
      "mcpServers": {}
    }
  }
}
`, workDirKey)
	appStatePath := filepath.Join(homeDir, ".claude.json")
	if err := os.WriteFile(appStatePath, []byte(appState), 0644); err != nil {
		return fmt.Errorf("writing .claude.json: %w", err)
	}

	ChownPath(claudeDir, username)
	ChownPath(appStatePath, username)
	return nil
}

// projectFromUsername extracts the project name from a clem-provisioned OS
// username of the form "<project>-<agentkey>". Used to locate the agent's
// working directory for the per-project trust entry in ~/.claude.json.
func projectFromUsername(username string) string {
	if i := strings.LastIndex(username, "-"); i > 0 {
		return username[:i]
	}
	return username
}

// InstallService writes and enables a systemd service for an agent.
func InstallService(cfg *config.Config, agentKey string, serviceContent string) error {
	serviceName := cfg.ServiceName(agentKey)
	servicePath := filepath.Join("/etc/systemd/system", serviceName)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("writing service file %s: %w", servicePath, err)
	}

	if out, err := sys.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}

	if out, err := sys.Run("systemctl", "enable", serviceName); err != nil {
		return fmt.Errorf("systemctl enable %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// InstallServiceByName writes and enables a systemd service by explicit name.
func InstallServiceByName(serviceName string, serviceContent string) error {
	servicePath := filepath.Join("/etc/systemd/system", serviceName)

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("writing service file %s: %w", servicePath, err)
	}

	if out, err := sys.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}

	if out, err := sys.Run("systemctl", "enable", serviceName); err != nil {
		return fmt.Errorf("systemctl enable %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// InstallWatchdogTimer writes and enables the watchdog service + timer.
func InstallWatchdogTimer(cfg *config.Config, serviceContent, timerContent string) error {
	svcName := cfg.WatchdogServiceName()
	timerName := cfg.WatchdogTimerName()

	svcPath := filepath.Join("/etc/systemd/system", svcName)
	timerPath := filepath.Join("/etc/systemd/system", timerName)

	if err := os.WriteFile(svcPath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("writing watchdog service: %w", err)
	}
	if err := os.WriteFile(timerPath, []byte(timerContent), 0644); err != nil {
		return fmt.Errorf("writing watchdog timer: %w", err)
	}

	if out, err := sys.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w\n%s", err, out)
	}
	if out, err := sys.Run("systemctl", "enable", "--now", timerName); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w\n%s", timerName, err, out)
	}
	return nil
}

// StartService starts a systemd service.
func StartService(serviceName string) error {
	out, err := sys.Run("systemctl", "start", serviceName)
	if err != nil {
		return fmt.Errorf("systemctl start %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// StopService stops a systemd service.
func StopService(serviceName string) error {
	out, err := sys.Run("systemctl", "stop", serviceName)
	if err != nil {
		return fmt.Errorf("systemctl stop %s: %w\n%s", serviceName, err, out)
	}
	return nil
}

// SystemdState returns the ActiveState of a systemd unit.
func SystemdState(serviceName string) string {
	out, err := sys.Run("systemctl", "show", "-p", "ActiveState", "--value", serviceName)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// TmuxAlive returns true if a tmux session with the given name exists in the
// agent user's tmux server. Tmux servers are per-UID, so checking from the
// invoking shell (typically root running `clem status`) sees an empty server
// and reports every agent as down. Always invoke under sudo -u <osUser>.
func TmuxAlive(osUser, sessionName string) bool {
	if osUser == "" {
		// Backwards-compat path for callers that still query their own server.
		_, err := sys.Run("tmux", "has-session", "-t", sessionName)
		return err == nil
	}
	_, err := sys.Run("sudo", "-n", "-u", osUser, "tmux", "has-session", "-t", sessionName)
	return err == nil
}

// credentials is a subset of ~/.claude/.credentials.json
type credentials struct {
	ClaudeAiOauth struct {
		ExpiresAt int64 `json:"expiresAt"`
	} `json:"claudeAiOauth"`
}

// TokenExpiry reads the Claude token expiry from <homeDir>/.claude/.credentials.json.
// Returns zero time if missing or unreadable.
func TokenExpiry(homeDir string) time.Time {
	credPath := filepath.Join(homeDir, ".claude", ".credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return time.Time{}
	}
	var creds credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return time.Time{}
	}
	if creds.ClaudeAiOauth.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(creds.ClaudeAiOauth.ExpiresAt/1000, 0)
}

// NeedsLogin returns true if the token is missing or expires within 7 days.
func NeedsLogin(homeDir string) bool {
	expiry := TokenExpiry(homeDir)
	if expiry.IsZero() {
		return true
	}
	return time.Until(expiry) < 7*24*time.Hour
}

// ChownPath changes ownership of a path to the given user (best effort).
// Errors are intentionally swallowed — callers use this for tidy-up where
// a failure doesn't block the operation. Prefer chownToUser for fatal paths.
func ChownPath(path, username string) {
	sys.Run("chown", "-R", fmt.Sprintf("%s:%s", username, username), path) //nolint:errcheck // best-effort by design; see function comment
}

// EnsureOwnedDir creates path (and any missing parents) and chowns the full
// tree to username. Use this instead of os.MkdirAll when the caller is root
// but the resulting directory must belong to an agent user.
func EnsureOwnedDir(path, username string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	// Chown every intermediate directory between the user's home and path.
	// os.MkdirAll runs as root, so parents (e.g. ~/.local above ~/.local/bin)
	// default to root-owned, which blocks runtimes like opencode/bun that
	// want to create sibling dirs (~/.local/share) later.
	home := fmt.Sprintf("/home/%s", username)
	current := path
	for strings.HasPrefix(current, home) {
		out, err := sys.Run("chown", fmt.Sprintf("%s:%s", username, username), current)
		if err != nil {
			return fmt.Errorf("chown %s to %s: %w\n%s", current, username, err, out)
		}
		if current == home {
			break
		}
		current = filepath.Dir(current)
	}
	// Recursive chown inside path itself for nested files.
	out, err := sys.Run("chown", "-R", fmt.Sprintf("%s:%s", username, username), path)
	if err != nil {
		return fmt.Errorf("chown -R %s to %s: %w\n%s", path, username, err, out)
	}
	return nil
}

// InstallRuntime installs the CLI for the given runtime kind as the agent's
// OS user. Supported: "claude-code" (default), "opencode".
func InstallRuntime(username, kind string) error {
	switch kind {
	case "", "claude-code":
		return InstallClaude(username)
	case "opencode":
		return InstallOpencode(username)
	default:
		return fmt.Errorf("unknown runtime %q", kind)
	}
}

// InstallOpencode runs the official opencode install script as the given user.
// Lands at /home/<user>/.opencode/bin/opencode.
func InstallOpencode(username string) error {
	cmd := exec.Command("sudo", "-iu", username, "bash", "-c",
		"curl -fsSL https://opencode.ai/install | bash")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("installing opencode for %s: %w\n%s", username, err, out)
	}
	binPath := fmt.Sprintf("/home/%s/.opencode/bin/opencode", username)
	info, err := os.Stat(binPath)
	if err != nil {
		return fmt.Errorf("opencode not found at %s after install: %w", binPath, err)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("opencode at %s is not executable", binPath)
	}
	return nil
}

// InstallClaude runs the official Claude install script as the given user so
// the binary lands in ~/.local/bin/claude owned by that user. Idempotent —
// the install script handles re-runs and applies the latest version.
func InstallClaude(username string) error {
	cmd := exec.Command("sudo", "-iu", username, "bash", "-c",
		"curl -fsSL https://claude.ai/install.sh | bash")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("installing claude for %s: %w\n%s", username, err, out)
	}
	claudePath := fmt.Sprintf("/home/%s/.local/bin/claude", username)
	info, err := os.Stat(claudePath)
	if err != nil {
		return fmt.Errorf("claude not found at %s after install: %w", claudePath, err)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("claude at %s is not executable", claudePath)
	}
	return nil
}

// InstallCaveman installs and enables the caveman Claude Code plugin for the agent user.
// Caveman reduces output tokens ~75% via terse response style. Idempotent.
// https://github.com/JuliusBrussee/caveman
func InstallCaveman(username string) error {
	home := fmt.Sprintf("/home/%s", username)
	marketplaceDir := filepath.Join(home, ".claude", "plugins", "marketplaces", "caveman")
	knownPath := filepath.Join(home, ".claude", "plugins", "known_marketplaces.json")

	// Clone marketplace (idempotent — skip if directory exists)
	if _, err := os.Stat(marketplaceDir); os.IsNotExist(err) {
		cloneCmd := exec.Command("sudo", "-iu", username, "bash", "-c",
			fmt.Sprintf("mkdir -p ~/.claude/plugins/marketplaces && git clone https://github.com/JuliusBrussee/caveman.git %s", marketplaceDir))
		if out, err := cloneCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("cloning caveman for %s: %w\n%s", username, err, out)
		}
	}

	// Register in known_marketplaces.json if missing
	existing, _ := os.ReadFile(knownPath)
	if !strings.Contains(string(existing), `"caveman"`) {
		base := strings.TrimSpace(string(existing))
		entry := fmt.Sprintf(`"caveman":{"source":{"source":"github","repo":"JuliusBrussee/caveman"},"installLocation":"%s","lastUpdated":"1970-01-01T00:00:00.000Z"}`, marketplaceDir)
		var merged string
		switch {
		case base == "" || base == "{}":
			merged = "{" + entry + "}"
		case strings.HasSuffix(base, "}"):
			merged = strings.TrimSuffix(base, "}") + "," + entry + "}"
		default:
			return fmt.Errorf("unexpected known_marketplaces.json format for %s", username)
		}
		if err := os.MkdirAll(filepath.Dir(knownPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(knownPath, []byte(merged), 0644); err != nil {
			return fmt.Errorf("writing known_marketplaces.json: %w", err)
		}
		ChownPath(filepath.Dir(knownPath), username)
	}

	// Install + enable plugin. "enable" returns non-zero when already
	// enabled, so we inspect the output instead of trusting the exit code.
	installCmd := exec.Command("sudo", "-iu", username, "bash", "-c",
		"claude plugin install caveman@caveman 2>&1; claude plugin enable caveman@caveman 2>&1; true")
	out, err := installCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running claude plugin commands for %s: %w\n%s", username, err, out)
	}
	output := string(out)
	installedOK := strings.Contains(output, "Successfully installed plugin") || strings.Contains(output, "already installed")
	enabledOK := strings.Contains(output, "Successfully enabled plugin") || strings.Contains(output, "already enabled")
	if !installedOK || !enabledOK {
		return fmt.Errorf("caveman install/enable did not confirm success for %s:\n%s", username, output)
	}

	return nil
}

// LastLogLine returns the last non-empty line of a log file.
func LastLogLine(logPath string) string {
	out, err := exec.Command("tail", "-n", "1", logPath).Output()
	if err != nil {
		return "-"
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "-"
	}
	// truncate to 60 chars for table display
	if len(line) > 60 {
		return line[:57] + "..."
	}
	return line
}
