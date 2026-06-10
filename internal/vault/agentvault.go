package vault

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/jahwag/clem/internal/config"
)

// agent-vault integration (Phase 2). sops remains the git-committable source of
// truth; these helpers seed a running agent-vault instance from it, write its
// injection (service) rules, and mint scoped, inject-only per-agent tokens. We
// drive agent-vault through its CLI (the documented stable surface) for
// owner-login/seed/mint/service, and plain HTTP GETs for health + CA fetch —
// mirroring how the rest of clem shells out to sops/yq/gh.
//
// AUTH MODEL: privileged operations (vault create, credential set, agent create,
// service add) require an authenticated *owner session*, not a token —
// agent-vault has no admin-token concept. The provisioner logs in (or, on a
// fresh instance, registers the first owner) with EnsureOwner; the resulting
// session persists in the invoking user's $HOME (root's, during provision) and
// authorizes the subsequent CLI calls. The minted per-agent token is no-access +
// vault proxy ONLY and cannot perform any of these operations. Validated live
// against agent-vault v0.22.0.

// AgentVaultBin is the installed agent-vault CLI path.
const AgentVaultBin = "/usr/local/bin/agent-vault"

// avRun executes the agent-vault CLI with extra env (e.g. AGENT_VAULT_ADDR).
// Replaced in tests.
var avRun = func(env []string, args ...string) ([]byte, error) {
	cmd := exec.Command(AgentVaultBin, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// avRunStdin executes the agent-vault CLI feeding stdin (for --password-stdin).
// It inherits the process env so the owner session lands in the invoker's HOME.
// Replaced in tests.
var avRunStdin = func(stdin string, args ...string) ([]byte, error) {
	cmd := exec.Command(AgentVaultBin, args...)
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader(stdin + "\n")
	return cmd.CombinedOutput()
}

// avHTTPGet performs a GET against the agent-vault management API. Replaced in tests.
var avHTTPGet = func(url string) (*http.Response, error) {
	return http.Get(url) //nolint:gosec // addr is operator-controlled loopback
}

// avEnv builds the connection env passed to every privileged CLI invocation.
// Only AGENT_VAULT_ADDR is set — auth rides the owner session in $HOME.
// Deliberately NO AGENT_VAULT_TOKEN: setting it would switch the CLI to
// agent-token auth and fail owner operations.
func avEnv(addr string) []string {
	return []string{"AGENT_VAULT_ADDR=" + addr}
}

// EnsureOwner authenticates the provisioner as the agent-vault instance owner so
// the subsequent seed/mint/service calls are authorized. It logs in if the owner
// account already exists, or registers it (the first registrant becomes the
// instance owner) on a fresh instance. Idempotent across provisions. The
// password is passed via stdin and never appears in the process table.
func EnsureOwner(addr, email, password string) error {
	if email == "" || password == "" {
		return fmt.Errorf("agent-vault owner email and password required")
	}
	if _, err := avRunStdin(password, "auth", "login", "--email", email, "--password-stdin", "--address", addr); err == nil {
		return nil
	} else {
		loginErr := err
		out, rerr := avRunStdin(password, "auth", "register", "--email", email, "--password-stdin", "--address", addr)
		if rerr != nil {
			return fmt.Errorf("agent-vault owner login failed (%v) and register failed: %w\n%s", loginErr, rerr, out)
		}
	}
	return nil
}

// ApplyServices writes each injection rule into the given agent-vault vault via
// `vault service add` (upsert — idempotent). These are what make agent-vault
// swap a brokered agent's placeholder for the real credential on a host-matched
// outbound request. Requires an owner session (call EnsureOwner first) and that
// avVault already exists (call SeedVault first).
func ApplyServices(addr, avVault string, services []config.Service) error {
	env := avEnv(addr)
	name := config.AgentVaultName(avVault)
	for _, s := range services {
		args := []string{"vault", "service", "add",
			"--name", s.Name, "--host", s.Host, "--auth-type", s.AuthType, "--vault", name}
		switch s.AuthType {
		case "bearer":
			args = append(args, "--token-key", s.TokenKey)
		case "basic":
			args = append(args, "--username-key", s.UsernameKey, "--password-key", s.PasswordKey)
		case "api-key":
			args = append(args, "--api-key-key", s.APIKeyKey)
			if s.APIKeyHeader != "" {
				args = append(args, "--api-key-header", s.APIKeyHeader)
			}
			if s.APIKeyPrefix != "" {
				args = append(args, "--api-key-prefix", s.APIKeyPrefix)
			}
		}
		if out, err := avRun(env, args...); err != nil {
			return fmt.Errorf("agent-vault service add %s: %w\n%s", s.Name, err, out)
		}
		fmt.Printf("  applied service %s (%s → %s)\n", s.Name, s.Host, s.AuthType)
	}
	return nil
}

// Health reports whether a running agent-vault answers on its management API.
func Health(addr string) error {
	resp, err := avHTTPGet(strings.TrimRight(addr, "/") + "/health")
	if err != nil {
		return fmt.Errorf("agent-vault health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent-vault health: status %d", resp.StatusCode)
	}
	return nil
}

// FetchCA downloads agent-vault's MITM CA cert to destPath so agents can trust
// the intercepted TLS. The cert is public; destPath should be world-readable.
func FetchCA(addr, destPath string) error {
	resp, err := avHTTPGet(strings.TrimRight(addr, "/") + "/v1/mitm/ca.pem")
	if err != nil {
		return fmt.Errorf("fetching agent-vault CA: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching agent-vault CA: status %d", resp.StatusCode)
	}
	pem, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading agent-vault CA: %w", err)
	}
	if err := os.WriteFile(destPath, pem, 0644); err != nil {
		return fmt.Errorf("writing agent-vault CA to %s: %w", destPath, err)
	}
	return nil
}

// AllVaults decrypts secrets.sops.yaml and returns every vault as a map of
// key→value. Used by Migrate to seed agent-vault from the sops source of truth.
func AllVaults() (map[string]map[string]string, error) {
	if err := ensureSops(); err != nil {
		return nil, err
	}
	decrypted, err := sopsDecrypt()
	if err != nil {
		return nil, err
	}
	out, err := runYQ(".vaults | keys | .[]", decrypted)
	if err != nil {
		return nil, fmt.Errorf("listing vaults: %w", err)
	}
	result := make(map[string]map[string]string)
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if name == "" {
			continue
		}
		kvOut, err := runYQ(fmt.Sprintf(".vaults.%s | to_entries | .[] | .key + \"=\" + .value", name), decrypted)
		if err != nil {
			return nil, fmt.Errorf("reading vault %s: %w", name, err)
		}
		kv := make(map[string]string)
		for _, line := range strings.Split(strings.TrimSpace(kvOut), "\n") {
			if line == "" {
				continue
			}
			if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
				kv[parts[0]] = parts[1]
			}
		}
		result[name] = kv
	}
	return result, nil
}

// SeedVault creates the named vault in agent-vault (idempotent) and sets every
// key/value.
//
// KNOWN EXPOSURE: secret values pass through the agent-vault process argv,
// which is world-readable in /proc for the lifetime of each (sub-second) set
// call. This is an upstream constraint, not a choice: per the agent-vault CLI
// reference (docs.agent-vault.dev/reference/cli), `vault credential set` takes
// only KEY=VALUE positional args — there is no stdin, file, or env input form
// (verified against v0.22.0). Risk is bounded to the provisioning host during
// `clem provision` (run as root, transiently); on a host that also runs agent
// users, an agent polling /proc during provision could observe a value, so
// prefer provisioning before agents are started (clem's normal flow). Replace
// with a non-argv transport when upstream ships one.
func SeedVault(addr, vaultName string, kv map[string]string) error {
	env := avEnv(addr)
	avName := config.AgentVaultName(vaultName)
	// create is idempotent-by-intent; ignore an "already exists" failure.
	if out, err := avRun(env, "vault", "create", avName); err != nil &&
		!strings.Contains(strings.ToLower(string(out)), "exist") {
		return fmt.Errorf("agent-vault vault create %s: %w\n%s", avName, err, out)
	}
	for k, v := range kv {
		if out, err := avRun(env, "vault", "credential", "set", k+"="+v, "--vault", avName); err != nil {
			return fmt.Errorf("agent-vault credential set %s.%s: %w\n%s", avName, k, err, out)
		}
	}
	return nil
}

// EnsureAgentIdentity creates (or rotates) an agent-vault identity scoped to the
// given vaults and returns a freshly-minted token. The agent is created with
// instance role no-access and vault role proxy ONLY — this is the inject-only
// guarantee: a proxy-role token can cause the proxy to inject the real secret
// on an outbound request but can never read the plaintext back. Re-provision
// rotates the token (old one invalidated), which is the intended idempotency.
func EnsureAgentIdentity(addr, agentName string, vaultNames []string) (string, error) {
	env := avEnv(addr)
	args := []string{"agent", "create", agentName, "--role", "no-access", "--token-only"}
	for _, v := range vaultNames {
		args = append(args, "--vault", config.AgentVaultName(v)+":proxy")
	}
	out, err := avRun(env, args...)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "exist") {
			// Already enrolled — rotate to obtain a fresh scoped token.
			rot, rerr := avRun(env, "agent", "rotate", agentName, "--token-only")
			if rerr != nil {
				return "", fmt.Errorf("agent-vault agent rotate %s: %w\n%s", agentName, rerr, rot)
			}
			return strings.TrimSpace(string(rot)), nil
		}
		return "", fmt.Errorf("agent-vault agent create %s: %w\n%s", agentName, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// Migrate seeds every sops vault into a running agent-vault instance. Used by
// `clem vault migrate` and by provision when the agent-vault backend is active.
func Migrate(addr string) error {
	vaults, err := AllVaults()
	if err != nil {
		return err
	}
	for name, kv := range vaults {
		if err := SeedVault(addr, name, kv); err != nil {
			return err
		}
		fmt.Printf("  seeded vault %s (%d secrets) into agent-vault\n", name, len(kv))
	}
	return nil
}
