package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
// write:ssh_signing_key (admin:ssh_signing_key) scope. Idempotent: returns nil
// if the key is already registered. The title argument lets callers
// distinguish keys per agent in the GitHub UI (e.g. "clem-cdev-lead").
func RegisterSSHSigningKey(pubKey, ghToken, title string) error {
	if ghToken == "" {
		return fmt.Errorf("GH_TOKEN required to register SSH signing key; grant write:ssh_signing_key scope to the agent PAT")
	}
	if title == "" {
		title = "clem-signing"
	}

	type payload struct {
		Title string `json:"title"`
		Key   string `json:"key"`
	}
	body, err := json.Marshal(payload{Title: title, Key: pubKey})
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

// EnsureSystemUser creates a dedicated non-login system user (no home, nologin
// shell) used to run a host service such as the pipelock egress proxy. Keeping
// it distinct from the agent users is what lets the nftables UID firewall allow
// the proxy to egress while rejecting every agent UID. Idempotent.
func EnsureSystemUser(username string) error {
	if _, err := sys.Run("id", username); err == nil {
		fmt.Printf("  system user %s already exists\n", username)
		return nil
	}
	fmt.Printf("  creating system user %s\n", username)
	out, err := sys.Run("useradd",
		"--system",
		"--no-create-home",
		"--shell", "/usr/sbin/nologin",
		"--comment", "clem egress proxy",
		username,
	)
	if err != nil {
		return fmt.Errorf("useradd --system %s: %w\n%s", username, err, out)
	}
	return nil
}

// PipelockVersion is the pinned pipelock release clem installs. Bump
// deliberately; the binary is a security boundary (the egress firewall/DLP).
const PipelockVersion = "v2.5.0"

// InstallPipelock installs the pinned pipelock release to /usr/local/bin,
// verifying the download against the release's checksums.txt. Idempotent:
// skips when the pinned version is already present. We do NOT use the
// `curl | sh` installer or `go install` (the latter needs Go 1.25+ and yields
// a community-only binary) for a security-critical component.
//
// SUPPLY-CHAIN SCOPE: this verifies INTEGRITY (the tarball matches the
// checksums.txt shipped in the same release) but not AUTHENTICITY against an
// out-of-band trust root — an attacker who can replace the release asset can
// replace checksums.txt too (TOFU). Hardening to a clem-pinned digest or a
// cosign signature is a tracked follow-up.
//
// Asset naming follows the goreleaser default the project ships:
// pipelock_<version-no-v>_linux_<arch>.tar.gz. Bump PipelockVersion to upgrade.
func InstallPipelock() error {
	// Accept either `pipelock --version` (flag) or `pipelock version`
	// (subcommand) so the idempotency check doesn't force a re-download.
	verCheck := "/usr/local/bin/pipelock --version 2>/dev/null || /usr/local/bin/pipelock version 2>/dev/null"
	if out, err := sys.Run("bash", "-c", verCheck); err == nil &&
		strings.Contains(string(out), strings.TrimPrefix(PipelockVersion, "v")) {
		fmt.Printf("  pipelock %s already installed\n", PipelockVersion)
		return nil
	}

	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return fmt.Errorf("unsupported arch %q for pipelock install", runtime.GOARCH)
	}

	ver := PipelockVersion
	verNoV := strings.TrimPrefix(ver, "v")
	// Download tarball + checksums, verify sha256, extract the binary. set -e so
	// any failed step (download, checksum mismatch, extract) aborts non-zero.
	// The explicit empty-line guard prevents `sha256sum -c` from succeeding on
	// empty stdin when the asset name doesn't match a checksums.txt line.
	script := fmt.Sprintf(`set -euo pipefail
VER=%q; VNV=%q; ARCH=%q
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT; cd "$TMP"
BASE="https://github.com/luckyPipewrench/pipelock/releases/download/${VER}"
ASSET="pipelock_${VNV}_linux_${ARCH}.tar.gz"
curl -fsSL -o "$ASSET" "${BASE}/${ASSET}"
curl -fsSL -o checksums.txt "${BASE}/checksums.txt"
LINE=$(grep " ${ASSET}\$" checksums.txt || true)
[ -n "$LINE" ] || { echo "no checksum line for ${ASSET} in checksums.txt" >&2; exit 1; }
printf '%%s\n' "$LINE" | sha256sum -c -
tar -xzf "$ASSET" pipelock
install -m 0755 pipelock /usr/local/bin/pipelock`, ver, verNoV, arch)

	fmt.Printf("  installing pipelock %s (%s)\n", ver, arch)
	if out, err := sys.Run("bash", "-c", script); err != nil {
		return fmt.Errorf("installing pipelock %s: %w\n%s", ver, err, out)
	}
	return nil
}

// AgentVaultVersion is the pinned agent-vault release clem installs.
const AgentVaultVersion = "v0.22.0"

// InstallAgentVault installs the pinned agent-vault release to /usr/local/bin,
// verifying against the release checksums.txt (integrity; same TOFU caveat as
// InstallPipelock — pin a digest/signature before production). Idempotent.
func InstallAgentVault() error {
	verCheck := "/usr/local/bin/agent-vault --version 2>/dev/null || /usr/local/bin/agent-vault version 2>/dev/null"
	if out, err := sys.Run("bash", "-c", verCheck); err == nil &&
		strings.Contains(string(out), strings.TrimPrefix(AgentVaultVersion, "v")) {
		fmt.Printf("  agent-vault %s already installed\n", AgentVaultVersion)
		return nil
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return fmt.Errorf("unsupported arch %q for agent-vault install", runtime.GOARCH)
	}
	ver := AgentVaultVersion
	verNoV := strings.TrimPrefix(ver, "v")
	script := fmt.Sprintf(`set -euo pipefail
VER=%q; VNV=%q; ARCH=%q
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT; cd "$TMP"
BASE="https://github.com/Infisical/agent-vault/releases/download/${VER}"
ASSET="agent-vault_${VNV}_linux_${ARCH}.tar.gz"
curl -fsSL -o "$ASSET" "${BASE}/${ASSET}"
curl -fsSL -o checksums.txt "${BASE}/checksums.txt"
LINE=$(grep " ${ASSET}\$" checksums.txt || true)
[ -n "$LINE" ] || { echo "no checksum line for ${ASSET} in checksums.txt" >&2; exit 1; }
printf '%%s\n' "$LINE" | sha256sum -c -
tar -xzf "$ASSET" agent-vault
install -m 0755 agent-vault /usr/local/bin/agent-vault`, ver, verNoV, arch)

	fmt.Printf("  installing agent-vault %s (%s)\n", ver, arch)
	if out, err := sys.Run("bash", "-c", script); err != nil {
		return fmt.Errorf("installing agent-vault %s: %w\n%s", ver, err, out)
	}
	return nil
}

// BrokeredEnv builds the .env contents for an agent-vault-brokered agent: each
// brokered secret becomes a placeholder (the real value lives only in
// agent-vault and is injected on egress), non-brokered secrets keep their real
// value, and the agent-vault connection + CA-trust env is added. The agent
// holds only a scoped, inject-only token — not the upstream credentials.
//
// HTTPS_PROXY points at agent-vault (overriding any Phase-1 pipelock export,
// since .env is sourced after that export). vaultName selects the proxy's
// active vault context (the agent's first vault).
//
// INJECTION: emitting the placeholder is necessary but not sufficient —
// agent-vault must also be told to swap it. That mapping is an agent-vault
// "service" rule (host-matched, auth.type). clem generates those rules at
// provision from vault.services (see config.Service / vault.ApplyServices), so a
// brokered request reaches upstream with the real credential while .env holds
// only the placeholder. A brokered secret with no matching service is flagged at
// config-load and would egress as a placeholder.
func BrokeredEnv(av config.VaultBackend, ac config.AgentConfig, token, vaultName string, flat map[string]string) map[string]string {
	// Match the agent-vault-side vault name (sops names may contain '_'/uppercase
	// which agent-vault rejects; see config.AgentVaultName).
	vaultName = config.AgentVaultName(vaultName)
	env := make(map[string]string, len(flat)+12)
	for k, v := range flat {
		if ac.IsBrokered(k) {
			env[k] = "__" + strings.ToLower(k) + "__"
		} else {
			env[k] = v
		}
	}
	ca := av.CACertPathOrDefault()
	// url.UserPassword percent-encodes token/vault so reserved chars (@ : / #)
	// in a minted token can't corrupt the proxy URL.
	proxyURL := (&url.URL{
		Scheme: "https",
		User:   url.UserPassword(token, vaultName),
		Host:   av.ProxyHostOrDefault(),
	}).String()
	// SECURITY NOTE: a brokered agent has egress containment disabled (the two are
	// mutually exclusive — see config validation), so it can reach the agent-vault
	// management API at AGENT_VAULT_ADDR directly. The inject-only guarantee
	// therefore rests entirely on the minted token being instance-role no-access +
	// vault-role proxy ONLY (enforced in vault.EnsureAgentIdentity): that token
	// cannot read credentials or mutate vaults/services. Do not widen the token's
	// role, and do not expose role config to operators. Hardening follow-up:
	// firewall the management port off the agent UID even when brokering.
	env["AGENT_VAULT_ADDR"] = av.AddrOrDefault()
	env["AGENT_VAULT_TOKEN"] = token
	env["AGENT_VAULT_VAULT"] = vaultName
	env["HTTPS_PROXY"] = proxyURL
	env["HTTP_PROXY"] = proxyURL
	env["NO_PROXY"] = "127.0.0.1,localhost,::1"
	// claude-code (Node) ignores the system trust store; the others honor their
	// own bundle vars. All point at agent-vault's CA so intercepted TLS verifies.
	env["NODE_EXTRA_CA_CERTS"] = ca
	env["SSL_CERT_FILE"] = ca
	env["REQUESTS_CA_BUNDLE"] = ca
	env["CURL_CA_BUNDLE"] = ca
	env["GIT_SSL_CAINFO"] = ca
	// HTTPS_PROXY is an HTTPS (TLS) proxy, so git tunnels to it over TLS and must
	// trust the agent-vault CA for the PROXY connection too — not just the
	// upstream (GIT_SSL_CAINFO). git has no env var for http.proxySSLCAInfo, so
	// inject it via GIT_CONFIG_*. Without this, EVERY git operation through the
	// broker (push/pull/clone to any host) fails proxy certificate verification.
	env["GIT_CONFIG_COUNT"] = "1"
	env["GIT_CONFIG_KEY_0"] = "http.proxySSLCAInfo"
	env["GIT_CONFIG_VALUE_0"] = ca
	return env
}

// WriteEnvFile writes decrypted secrets to <homeDir>/.env with mode 0600.
// Also writes a global gitignore that blocks .env, .git-credentials, and
// secrets.sops.yaml from accidental commits.
func WriteEnvFile(username, homeDir string, secrets map[string]string) error {
	envPath := filepath.Join(homeDir, ".env")

	var sb strings.Builder
	for k, v := range secrets {
		// Strip vault-name prefix ("vaultName.keyName" → "keyName") so the
		// exported name is the bare secret key. Keys without a dot pass through.
		envKey := k
		if i := strings.IndexByte(k, '.'); i >= 0 {
			envKey = k[i+1:]
		}
		// Single-quote the value so bash treats it as fully literal.
		// Only ' needs escaping: end the quoted string, append literal ', reopen.
		escaped := strings.ReplaceAll(v, "'", `'\''`)
		sb.WriteString(fmt.Sprintf("export %s='%s'\n", envKey, escaped))
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
//     hidden-instruction-smuggling bypass. No allow-marker — zero-width
//     and bidi-override characters are never legitimate in source.
//  4. Code that reads protected secret env vars (Go/Python/Node). Closes
//     indirect runtime-exfil bypass. Skip with CLEM_HOOK_SKIP_CODE_SCAN=1.
//
// Two scoping rules apply to passes 1, 2, and 4 to keep the hook usable
// when forks legitimately need to sync history that contains test fixtures
// for the very regexes we install:
//
//   a. Only ADDED diff lines (lines starting with `+`, excluding the
//      `+++ b/path` file headers) are scanned. A removed line cannot
//      introduce a secret to the remote even if it textually matches —
//      it was already there. This eliminates the most common false
//      positive: cumulative-diff fork-sync where an unannotated old
//      version of a fixture line is being replaced by an annotated new
//      version, and both versions appear in the diff.
//
//   b. Lines containing the literal marker `clem:allow-secret` are
//      dropped before scanning. This is the documented escape hatch for
//      legitimate test fixtures (e.g. positives tables for the hook's
//      own regex tests). The marker is namespaced to avoid colliding
//      with prose mentions and is scoped to the same line as the
//      secret-shaped string. Pass 3 ignores the marker by design.
const PrePushAllowSecretMarker = "clem:allow-secret"

var prePushHookContent = fmt.Sprintf(`#!/bin/bash
# Installed by clem provision. Do not edit by hand - will be overwritten.
# Pass 1: literal credential patterns (tokens, keys, PEM blocks).
# Pass 2: base64-encoded secrets (decoded + re-scanned).
# Pass 3: Unicode traps (zero-width / bidi / BOM - hidden-instruction smuggling).
# Pass 4: code that reads protected secret env vars (Go/Python/Node).
#         Skip with: CLEM_HOOK_SKIP_CODE_SCAN=1 git push
#
# Scope (passes 1, 2, 4): added lines only (^\+ excluding ^\+\+\+), and
# any line carrying the marker '%s' is skipped. Pass 3 scans every line
# regardless because hidden-character smuggling has no legitimate use.

zero="0000000000000000000000000000000000000000"
patterns='%s'
code_patterns='%s'
unicode_traps='%s'
allow_marker='%s'

# extract_added prints diff lines that introduce content on the remote:
# they start with a single '+' (the '+++' file-header line is excluded)
# and do not carry the allow-secret marker.
extract_added() {
  grep -E '^\+([^+]|$)' | grep -v -F "$allow_marker"
}

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
  added=$(echo "$diff" | extract_added)

  # Pass 1: direct literal secret match (added lines, no allow-marker).
  hits=$(echo "$added" | grep -E "$patterns" | head -3)
  if [ -n "$hits" ]; then
    echo "clem pre-push hook: push blocked - secret pattern detected in $range" >&2
    echo "$hits" | sed 's/^/  /' >&2
    echo "" >&2
    echo "Rotate the leaked credential immediately if it is real. To override" >&2
    echo "for an intentional test fixture, append '$allow_marker' to the same" >&2
    echo "line. As a last resort, push with --no-verify (think first)." >&2
    exit 1
  fi

  # Pass 2: base64-decode + re-scan added lines. Long base64 runs that
  # decode to secret-shaped bytes are blocked.
  while IFS= read -r chunk; do
    [ -z "$chunk" ] && continue
    decoded=$(echo "$chunk" | base64 -d 2>/dev/null) || continue
    if echo "$decoded" | grep -qE "$patterns"; then
      echo "clem pre-push hook: push blocked - base64-encoded secret detected in $range" >&2
      echo "  $chunk -> decoded hit" >&2
      exit 1
    fi
  done < <(echo "$added" | grep -oE '[A-Za-z0-9+/]{40,}={0,2}')

  # Pass 3: unicode traps. Scans the entire diff (added + removed +
  # context) since hidden control characters never have a legitimate
  # source-code use; allow-marker intentionally ignored.
  uhits=$(echo "$diff" | grep -P "$unicode_traps" | head -3)
  if [ -n "$uhits" ]; then
    echo "clem pre-push hook: push blocked - unicode control/override characters detected in $range (possible prompt-injection smuggling)" >&2
    echo "$uhits" | sed 's/^/  /' >&2
    exit 1
  fi

  # Pass 4: indirect runtime exfil via os.Getenv on protected names.
  if [ "${CLEM_HOOK_SKIP_CODE_SCAN:-0}" != "1" ]; then
    code_hits=$(echo "$added" | grep -E "$code_patterns" | head -3)
    if [ -n "$code_hits" ]; then
      echo "clem pre-push hook: push blocked - diff reads a protected secret env var in $range" >&2
      echo "$code_hits" | sed 's/^/  /' >&2
      echo "" >&2
      echo "Set CLEM_HOOK_SKIP_CODE_SCAN=1 if this read is intentional and reviewed," >&2
      echo "or append '$allow_marker' to the line if it is a test fixture." >&2
      exit 1
    fi
  fi
done
exit 0
`, PrePushAllowSecretMarker, SecretPatternRegex, SecretCodePatternRegex, UnicodeTrapRegex, PrePushAllowSecretMarker)

// stripGitConfigKey removes every line of `content` whose leading text is
// "\t<key> = ". Used to clear stale name/email entries before re-writing them
// with the current configured values.
func stripGitConfigKey(content, key string) string {
	prefix := "\t" + key + " = "
	lines := strings.Split(content, "\n")
	out := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ConfigureGit writes SSH commit-signing configuration and the git user
// identity to the agent's ~/.gitconfig. Idempotent — safe to call every
// provision. pubKey is the agent's ed25519 public key (returned by
// EnsureSSHKey). When gitName / gitEmail are non-empty, any pre-existing
// "\tname = " / "\temail = " line is stripped and replaced so clem.yaml stays
// authoritative across re-provisions; empty inputs leave the existing value
// untouched.
func ConfigureGit(username, homeDir, pubKey, gitName, gitEmail string) error {
	sshDir := filepath.Join(homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("creating .ssh dir: %w", err)
	}

	commitEmail := username + "@clem"
	if gitEmail != "" {
		commitEmail = gitEmail
	}
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
	if gitName != "" {
		content = stripGitConfigKey(content, "name")
	}
	if gitEmail != "" {
		content = stripGitConfigKey(content, "email")
	}
	var identityLines []string
	if gitName != "" {
		identityLines = append(identityLines, "\tname = "+gitName)
	}
	if gitEmail != "" {
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

// flatSecret looks up a bare key in a vault-qualified secrets map
// ("vaultName.keyName" format). Returns the value for the first matching key
// suffix; returns "" if not found.
func flatSecret(secrets map[string]string, key string) string {
	for k, v := range secrets {
		if k == key {
			return v
		}
		if i := strings.IndexByte(k, '.'); i >= 0 && k[i+1:] == key {
			return v
		}
	}
	return ""
}

// WriteWranglerConfig writes a wrangler OAuth config for the agent if the
// matching env vars are present in the secrets map. Idempotent — safe to call
// every provision. The wrangler binary auto-refreshes the OAuth token using
// the refresh token, so this stays valid as long as the refresh token does.
func WriteWranglerConfig(username, homeDir string, secrets map[string]string) error {
	oauth := flatSecret(secrets, "WRANGLER_OAUTH_TOKEN")
	refresh := flatSecret(secrets, "WRANGLER_REFRESH_TOKEN")
	expiration := flatSecret(secrets, "WRANGLER_EXPIRATION")
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
// first-run onboarding prompts. effort sets effortLevel when non-empty
// (accepted: "low", "medium", "high"); empty leaves Claude Code's default.
//
// Claude Code stores two flavours of config:
//   - ~/.claude/settings.json     — user-level flags + permissions
//   - ~/.claude.json              — app-level state (onboarding gates, per-project trust)
//
// We write both. Without ~/.claude.json, fresh agents hit the "Security notes —
// Press Enter" screen and the "Quick safety check: trust this folder?" prompt
// before the runner can inject its prompt, causing lost first iterations.
func WriteSettings(username, homeDir, project, effort string) error {
	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return fmt.Errorf("creating .claude dir: %w", err)
	}

	// includeCoAuthoredBy=false suppresses the "Co-authored-by: Claude ..."
	// trailer Claude Code otherwise appends to commits it creates. Agents
	// should author commits under their own identity, not leak that an LLM
	// drove them - clem PRs go through normal human review regardless.
	fields := []string{
		`"hasTrustDialogAccepted": true`,
		`"hasCompletedProjectOnboarding": true`,
		`"skipDangerousModePermissionPrompt": true`,
		`"includeCoAuthoredBy": false`,
	}
	if effort != "" {
		fields = append(fields, fmt.Sprintf(`"effortLevel": %q`, effort))
	}
	settings := "{\n  " + strings.Join(fields, ",\n  ") + "\n}\n"
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(settings), 0644); err != nil {
		return fmt.Errorf("writing settings.json: %w", err)
	}

	// ~/.claude.json gates the top-level onboarding screens. A future-dated
	// lastOnboardingVersion prevents the next claude upgrade from re-prompting.
	// projects.<workdir>.hasTrustDialogAccepted dismisses the folder-trust
	// dialog for the agent's working directory.
	workDirKey := filepath.Join(homeDir, project)
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
		ExpiresAt    int64  `json:"expiresAt"`
		RefreshToken string `json:"refreshToken"`
	} `json:"claudeAiOauth"`
}

func readCredentials(homeDir string) (credentials, bool) {
	credPath := filepath.Join(homeDir, ".claude", ".credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return credentials{}, false
	}
	var creds credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return credentials{}, false
	}
	return creds, true
}

// TokenExpiry reads the Claude access-token expiry from <homeDir>/.claude/.credentials.json.
// Returns zero time if missing or unreadable. Note: Claude Max access tokens
// have an ~8-hour life and are auto-refreshed by Claude Code using the
// refresh token — see HasRefreshToken / NeedsLogin.
func TokenExpiry(homeDir string) time.Time {
	creds, ok := readCredentials(homeDir)
	if !ok || creds.ClaudeAiOauth.ExpiresAt == 0 {
		return time.Time{}
	}
	return time.Unix(creds.ClaudeAiOauth.ExpiresAt/1000, 0)
}

// HasRefreshToken reports whether <homeDir>/.claude/.credentials.json contains
// a non-empty OAuth refresh token. This is the real "logged in" signal —
// Claude Code uses the refresh token to mint new ~8h access tokens
// automatically, so a present refresh token means no manual login is needed
// even if the access token has already expired.
func HasRefreshToken(homeDir string) bool {
	creds, ok := readCredentials(homeDir)
	return ok && creds.ClaudeAiOauth.RefreshToken != ""
}

// NeedsLogin returns true only when manual `claude /login` is actually
// required: the credentials file is missing, unreadable, or carries no
// refresh token. Access-token expiry is intentionally NOT checked — those
// are short-lived (~8h) and refreshed transparently by Claude Code, so
// gating on access expiry would prompt unnecessary daily logins.
func NeedsLogin(homeDir string) bool {
	return !HasRefreshToken(homeDir)
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

// InstallCaveman installs the caveman plugin for the agent user. Idempotent.
// https://github.com/JuliusBrussee/caveman
func InstallCaveman(username string) error {
	return InstallExtensions(username, fmt.Sprintf("/home/%s", username),
		config.ExtensionsConfig{}, config.CavemanUltra, nil)
}

// InstallMarketplace clones a GitHub marketplace and registers it in
// known_marketplaces.json. Idempotent. Verifies HEAD against m.Commit if set.
func InstallMarketplace(username string, m config.MarketplaceConfig) error {
	home := fmt.Sprintf("/home/%s", username)
	marketplaceDir := filepath.Join(home, ".claude", "plugins", "marketplaces", m.Name)
	knownPath := filepath.Join(home, ".claude", "plugins", "known_marketplaces.json")
	if _, err := os.Stat(marketplaceDir); os.IsNotExist(err) {
		parentDir := filepath.Join(home, ".claude", "plugins", "marketplaces")
		if out, err := exec.Command("sudo", "-iu", username, "mkdir", "-p", parentDir).CombinedOutput(); err != nil {
			return fmt.Errorf("creating marketplace dir for %s: %w\n%s", username, err, out)
		}
		cloneURL := "https://github.com/" + m.Repo + ".git"
		if out, err := exec.Command("sudo", "-iu", username, "git", "clone", cloneURL, marketplaceDir).CombinedOutput(); err != nil {
			return fmt.Errorf("cloning marketplace %s for %s: %w\n%s", m.Name, username, err, out)
		}
	}
	if m.Commit != "" {
		out, err := exec.Command("sudo", "-iu", username, "git", "-C", marketplaceDir, "rev-parse", "HEAD").CombinedOutput()
		if err != nil {
			return fmt.Errorf("reading HEAD for marketplace %s: %w\n%s", m.Name, err, out)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(out)), m.Commit) {
			return fmt.Errorf("marketplace %s HEAD does not match pinned commit %s for %s: got %s", m.Name, m.Commit, username, strings.TrimSpace(string(out)))
		}
	}
	var known map[string]json.RawMessage
	if raw, _ := os.ReadFile(knownPath); len(raw) > 0 {
		_ = json.Unmarshal(raw, &known)
	}
	if known == nil {
		known = make(map[string]json.RawMessage)
	}
	if _, exists := known[m.Name]; !exists {
		entry, _ := json.Marshal(map[string]any{
			"source":          map[string]string{"source": m.Source, "repo": m.Repo},
			"installLocation": marketplaceDir,
			"lastUpdated":     "1970-01-01T00:00:00.000Z",
		})
		known[m.Name] = entry
		out, _ := json.Marshal(known)
		if err := os.MkdirAll(filepath.Dir(knownPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(knownPath, out, 0644); err != nil {
			return fmt.Errorf("writing known_marketplaces.json: %w", err)
		}
		ChownPath(filepath.Dir(knownPath), username)
	}
	return nil
}

// installPlugin installs and enables a plugin from the named marketplace. Idempotent.
func installPlugin(username, pluginName, marketplaceName string) error {
	ref := pluginName + "@" + marketplaceName
	installOut, _ := exec.Command("sudo", "-iu", username, "claude", "plugin", "install", ref).CombinedOutput()
	if !strings.Contains(string(installOut), "Successfully installed plugin") && !strings.Contains(string(installOut), "already installed") {
		return fmt.Errorf("plugin %s install did not confirm success for %s:\n%s", ref, username, installOut)
	}
	enableOut, _ := exec.Command("sudo", "-iu", username, "claude", "plugin", "enable", ref).CombinedOutput()
	if !strings.Contains(string(enableOut), "Successfully enabled plugin") && !strings.Contains(string(enableOut), "already enabled") {
		return fmt.Errorf("plugin %s enable did not confirm success for %s:\n%s", ref, username, enableOut)
	}
	return nil
}

// InstallSkill clones a GitHub skill into ~/.claude/skills/<name>/. Idempotent.
// When s.Path is set the entrypoint is at ~/.claude/skills/<name>/<path>.
func InstallSkill(username string, s config.SkillConfig) error {
	skillDir := filepath.Join(fmt.Sprintf("/home/%s", username), ".claude", "skills", s.Name)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		parentDir := filepath.Join(fmt.Sprintf("/home/%s", username), ".claude", "skills")
		if out, err := exec.Command("sudo", "-iu", username, "mkdir", "-p", parentDir).CombinedOutput(); err != nil {
			return fmt.Errorf("creating skills dir for %s: %w\n%s", username, err, out)
		}
		cloneURL := "https://github.com/" + s.Repo + ".git"
		if out, err := exec.Command("sudo", "-iu", username, "git", "clone", cloneURL, skillDir).CombinedOutput(); err != nil {
			return fmt.Errorf("cloning skill %s for %s: %w\n%s", s.Name, username, err, out)
		}
	}
	ChownPath(skillDir, username)
	return nil
}

// SetMCPServers overwrites the mcpServers key in ~/.claude/settings.json while
// preserving all other settings. Vault refs in env values are expanded via secrets.
func SetMCPServers(homeDir string, servers []config.MCPServerConfig, secrets map[string]string) error {
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("reading settings.json: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parsing settings.json: %w", err)
	}
	mcpMap := make(map[string]any, len(servers))
	for _, srv := range servers {
		entry := make(map[string]any)
		if srv.URL != "" {
			entry["type"] = "sse"
			entry["url"] = srv.URL
		} else {
			entry["type"] = "stdio"
			entry["command"] = srv.Command
			if len(srv.Args) > 0 {
				entry["args"] = srv.Args
			}
		}
		if len(srv.Env) > 0 {
			env := make(map[string]string, len(srv.Env))
			for k, v := range srv.Env {
				env[k] = config.ExpandVaultRefs(v, secrets)
			}
			entry["env"] = env
		}
		mcpMap[srv.Name] = entry
	}
	doc["mcpServers"] = mcpMap
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling settings.json: %w", err)
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0644)
}

// InstallExtensions installs marketplaces, plugins, skills, and MCP servers for
// an agent. caveman: true is translated to the equivalent marketplace+plugin
// entries if not already explicit. Idempotent. Removed entries are not
// auto-uninstalled — use `claude plugin list` to audit manually.
func InstallExtensions(username, homeDir string, ext config.ExtensionsConfig, caveman config.CavemanLevel, secrets map[string]string) error {
	if caveman.Enabled() {
		if !slices.ContainsFunc(ext.Marketplaces, func(m config.MarketplaceConfig) bool { return m.Name == "caveman" }) {
			ext.Marketplaces = append([]config.MarketplaceConfig{{Name: "caveman", Source: "github", Repo: "JuliusBrussee/caveman"}}, ext.Marketplaces...)
		}
		if !slices.ContainsFunc(ext.Plugins, func(p config.PluginConfig) bool { return p.Name == "caveman" && p.Marketplace == "caveman" }) {
			ext.Plugins = append([]config.PluginConfig{{Name: "caveman", Marketplace: "caveman"}}, ext.Plugins...)
		}
	}
	for _, mp := range ext.Marketplaces {
		if err := InstallMarketplace(username, mp); err != nil {
			return err
		}
	}
	for _, pl := range ext.Plugins {
		if err := installPlugin(username, pl.Name, pl.Marketplace); err != nil {
			return err
		}
	}
	for _, sk := range ext.Skills {
		if err := InstallSkill(username, sk); err != nil {
			return err
		}
	}
	if len(ext.MCPServers) > 0 {
		if err := SetMCPServers(homeDir, ext.MCPServers, secrets); err != nil {
			return err
		}
	}
	return nil
}

// WriteHostManagedSettings writes the merged deny list from all agents in cfg
// to path (typically /etc/claude-code/managed-settings.json). The file is
// root-owned and cannot be overridden by agent users. Idempotent.
func WriteHostManagedSettings(cfg *config.Config, path string) error {
	seen := make(map[string]bool)
	denies := []string{}
	for _, ac := range cfg.Agents {
		for _, d := range ac.Permissions.Deny {
			if !seen[d] {
				seen[d] = true
				denies = append(denies, d)
			}
		}
	}
	sort.Strings(denies)
	doc := map[string]any{
		"permissions": map[string]any{
			"deny": denies,
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating managed-settings dir: %w", err)
	}
	return os.WriteFile(path, out, 0644)
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
