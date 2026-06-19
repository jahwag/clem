// agent-vault credential-broker configuration (Phase 2): backend
// selection, injection (service) rules, and validation.

package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// serviceNameRe matches an agent-vault service slug: 3–64 lowercase
// alphanumeric/hyphen characters (agent-vault's own constraint).
var serviceNameRe = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)

// avNameInvalid matches characters not allowed in an agent-vault vault name.
var avNameInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// AgentVaultName maps a clem/sops vault name to an agent-vault-compatible vault
// name: lowercased, with any run of characters outside [a-z0-9-] collapsed to a
// single hyphen. sops vault names may contain '_' (e.g. dev_to) or uppercase,
// which agent-vault rejects (names are lowercase alphanumeric/hyphen). Used
// everywhere clem hands a vault name to agent-vault so e.g. dev_to -> dev-to
// consistently across seed, token mint, service rules, and the agent proxy env.
func AgentVaultName(sopsVault string) string {
	return avNameInvalid.ReplaceAllString(strings.ToLower(sopsVault), "-")
}

// VaultBackend selects how agent secrets are materialized (Phase 2). Default
// "env" preserves the legacy flow: secrets decrypted from sops are written
// verbatim into each agent's .env. "agent-vault" routes HTTP-brokerable
// credentials through an Infisical agent-vault credential proxy so the real
// secret never reaches the agent — the agent holds only a placeholder and a
// scoped, inject-only token. sops remains the git-committable source of truth
// and seeds agent-vault at provision time.
type VaultBackend struct {
	// Backend is "env" (default) or "agent-vault".
	Backend string `yaml:"backend"`
	// SystemUser is the dedicated non-login user that owns the vault store and
	// runs agent-vault. Default "clem-vault".
	SystemUser string `yaml:"system_user"`
	// Addr is the agent-vault management API the provisioner seeds/mints against.
	// Default "http://127.0.0.1:14321".
	Addr string `yaml:"addr"`
	// ProxyHost is the host:port of agent-vault's TLS-MITM proxy that agents
	// point HTTPS_PROXY at. Default "127.0.0.1:14322".
	ProxyHost string `yaml:"proxy_host"`
	// CACertPath is where agent-vault's CA cert is written for agents to trust
	// the intercepted TLS. Default "/etc/clem/agent-vault-ca.pem".
	CACertPath string `yaml:"ca_cert_path"`
	// Services are the global injection rules. agent-vault matches outbound
	// requests by host and swaps in the real credential, so a brokered agent
	// egresses with the real value while its .env holds only a placeholder. Each
	// service names credential KEYS (token_key etc.); at provision clem applies
	// every service whose keys an agent brokers into that agent's consolidated
	// vault. A brokered secret with no matching service egresses as a placeholder.
	Services []Service `yaml:"services"`
	// Backends lists secret source backends in merge order (#174). Empty means
	// the implicit default [{name: local, type: sops}] — today's behavior.
	// Later sources win on key conflicts, mirroring the per-source bucket
	// merge. Distinct from Backend above, which selects how secrets are
	// materialized for agents (env vs agent-vault broker); Backends selects
	// where secrets come from.
	Backends []VaultSource `yaml:"backends"`
	// ExposurePolicy controls what happens when a granted vault key is neither
	// listed in an agent's brokered_secrets nor its reveal_secrets: "warn"
	// (default) prints a warning at provision and continues; "strict" blocks
	// provisioning; "off" silences the check (legacy behaviour).
	ExposurePolicy string `yaml:"exposure_policy"`
}

// ValidVaultSourceTypes is the authoritative set of vault.backends source
// types. Load() validates against it; the dispatch switch in internal/vault
// must handle every type listed here, so add new types (#174: infisical) to
// both in one change.
var ValidVaultSourceTypes = []string{"sops"}

// VaultSource is one entry in vault.backends: a named secret source backend.
// Only sops is implemented today; Infisical Agent lands behind the same
// interface (#174).
type VaultSource struct {
	// Name qualifies bucket refs and appears in error messages. Must match
	// validName and be unique across backends.
	Name string `yaml:"name"`
	// Type is the backend implementation. Empty defaults to sops.
	Type string `yaml:"type"`
}

// Service is one agent-vault injection rule: for requests to Host, attach the
// credential(s) named below using the AuthType scheme. Services are not bound to
// a vault — clem applies each one into the consolidated vault of every agent
// that brokers the referenced credential keys. Mirrors `agent-vault vault
// service add` flags 1:1.
type Service struct {
	// Name is the service slug (3-64 lowercase alphanumeric/hyphen chars).
	Name string `yaml:"name"`
	// Host is the target: a bare host (api.x.com), one-level wildcard
	// (*.x.com), or inline path form (slack.com/api/*).
	Host string `yaml:"host"`
	// AuthType is bearer | basic | api-key | custom | passthrough.
	AuthType string `yaml:"auth_type"`
	// TokenKey is the credential key for bearer auth.
	TokenKey string `yaml:"token_key"`
	// UsernameKey / PasswordKey are the credential keys for basic auth.
	UsernameKey string `yaml:"username_key"`
	PasswordKey string `yaml:"password_key"`
	// APIKeyKey is the credential key for api-key auth; APIKeyHeader (default
	// Authorization) and APIKeyPrefix (e.g. "Bot ") shape the injected header.
	APIKeyKey    string `yaml:"api_key_key"`
	APIKeyHeader string `yaml:"api_key_header"`
	APIKeyPrefix string `yaml:"api_key_prefix"`
}

// CredentialKeys returns the credential keys this service injects, per auth type.
func (s Service) CredentialKeys() []string {
	switch s.AuthType {
	case "bearer":
		return []string{s.TokenKey}
	case "basic":
		return []string{s.UsernameKey, s.PasswordKey}
	case "api-key":
		return []string{s.APIKeyKey}
	}
	return nil
}

// ValidAuthTypes are the agent-vault service auth schemes clem accepts.
var ValidAuthTypes = map[string]bool{
	"bearer": true, "basic": true, "api-key": true, "custom": true, "passthrough": true,
}

// UnbrokerableSecrets are credential keys that agent-vault cannot broker over
// HTTP and must therefore stay as real values in .env: the Discord gateway
// token (sent in a post-upgrade WebSocket frame, not an HTTP header) and the
// SSH/Elasticsearch creds (not HTTP at all). Listing any of these in
// brokered_secrets is a hard config error.
var UnbrokerableSecrets = map[string]bool{
	"DISCORD_TOKEN": true,
	"SSH_HOST":      true,
	"SSH_USER":      true,
	"SSH_KEY_PATH":  true,
	"ES_USER":       true,
	"ES_PASSWORD":   true,
}

// IsAgentVault reports whether the agent-vault backend is selected.
func (v VaultBackend) IsAgentVault() bool { return v.Backend == "agent-vault" }

// SystemUserOrDefault returns the vault system user, default clem-vault.
func (v VaultBackend) SystemUserOrDefault() string {
	if v.SystemUser == "" {
		return "clem-vault"
	}
	return v.SystemUser
}

// AddrOrDefault returns the management API address, default localhost:14321.
func (v VaultBackend) AddrOrDefault() string {
	if v.Addr == "" {
		return "http://127.0.0.1:14321"
	}
	return v.Addr
}

// ProxyHostOrDefault returns the MITM proxy host:port, default 127.0.0.1:14322.
func (v VaultBackend) ProxyHostOrDefault() string {
	if v.ProxyHost == "" {
		return "127.0.0.1:14322"
	}
	return v.ProxyHost
}

// CACertPathOrDefault returns the CA cert path, default under /etc/clem.
func (v VaultBackend) CACertPathOrDefault() string {
	if v.CACertPath == "" {
		return "/etc/clem/agent-vault-ca.pem"
	}
	return v.CACertPath
}

// IsBrokered reports whether a secret key is HTTP-brokered for this agent
// (placeholder in .env, real value only inside agent-vault).
func (ac AgentConfig) IsBrokered(key string) bool {
	if !ac.VaultBroker {
		return false
	}
	for _, k := range ac.BrokeredSecrets {
		if k == key {
			return true
		}
	}
	return false
}

// validateVaultServices checks the agent-vault injection rules. Services are
// global (not vault-bound): clem applies each into the consolidated vault of any
// agent that brokers the referenced credential keys. Warns on services no agent
// can use and on brokered secrets with no service to inject them.
func (cfg *Config) validateVaultServices() error {
	if len(cfg.Vault.Services) == 0 {
		return nil
	}
	if !cfg.Vault.IsAgentVault() {
		return fmt.Errorf("vault.services requires vault.backend: agent-vault")
	}
	// All credential keys brokered by some agent.
	allBrokered := map[string]bool{}
	for _, ac := range cfg.Agents {
		if !ac.VaultBroker {
			continue
		}
		for _, s := range ac.BrokeredSecrets {
			allBrokered[s] = true
		}
	}
	serviceKeys := map[string]bool{}
	for i, s := range cfg.Vault.Services {
		where := fmt.Sprintf("vault.services[%d]", i)
		if s.Name != "" {
			where = "vault.services " + s.Name
		}
		if !serviceNameRe.MatchString(s.Name) {
			return fmt.Errorf("%s: name must be 3-64 lowercase alphanumeric/hyphen chars", where)
		}
		if s.Host == "" {
			return fmt.Errorf("%s: host is required", where)
		}
		if !ValidAuthTypes[s.AuthType] {
			return fmt.Errorf("%s: auth_type must be bearer, basic, api-key, custom, or passthrough, got %q", where, s.AuthType)
		}
		switch s.AuthType {
		case "bearer":
			if s.TokenKey == "" {
				return fmt.Errorf("%s: auth_type bearer requires token_key", where)
			}
		case "basic":
			if s.UsernameKey == "" || s.PasswordKey == "" {
				return fmt.Errorf("%s: auth_type basic requires username_key and password_key", where)
			}
		case "api-key":
			if s.APIKeyKey == "" {
				return fmt.Errorf("%s: auth_type api-key requires api_key_key", where)
			}
		}
		anyBrokered := false
		for _, k := range s.CredentialKeys() {
			serviceKeys[k] = true
			if allBrokered[k] {
				anyBrokered = true
			}
		}
		if len(allBrokered) > 0 && !anyBrokered {
			fmt.Fprintf(os.Stderr, "warning: %s references credential keys no agent brokers — it will not be applied\n", where)
		}
	}
	// Warn on any brokered secret with no service to inject it.
	for key, ac := range cfg.Agents {
		if !ac.VaultBroker {
			continue
		}
		for _, s := range ac.BrokeredSecrets {
			if !serviceKeys[s] {
				fmt.Fprintf(os.Stderr, "warning: agent %s: brokered secret %q has no matching vault.service — it would egress as a placeholder\n", key, s)
			}
		}
	}
	return nil
}
