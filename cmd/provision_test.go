package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jahwag/clem/internal/config"
)

// --- brokeredSeedInputs: the pure pre-flight for agent-vault brokering. A
// mistake here either aborts provisioning or seeds the wrong credentials, so
// every branch is pinned.

func bearerSvc(name, host, key string) config.Service {
	return config.Service{Name: name, Host: host, AuthType: "bearer", TokenKey: key}
}

func TestBrokeredSeedInputs_MissingSecretAbortsBeforeSideEffects(t *testing.T) {
	ac := config.AgentConfig{
		VaultBroker:     true,
		BrokeredSecrets: []string{"GH_TOKEN", "MISSING_KEY"},
	}
	flat := map[string]string{"GH_TOKEN": "tok"}
	_, _, _, err := brokeredSeedInputs("proj-lead", ac, nil, flat)
	if err == nil {
		t.Fatal("missing brokered secret must error")
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("error should name the missing key, got: %v", err)
	}
}

func TestBrokeredSeedInputs_HappyPath(t *testing.T) {
	ac := config.AgentConfig{
		VaultBroker:     true,
		BrokeredSecrets: []string{"GH_TOKEN"},
	}
	services := []config.Service{
		bearerSvc("github", "api.github.com", "GH_TOKEN"),
		bearerSvc("unrelated", "api.x.com", "X_TOKEN"), // brokered by no one
	}
	flat := map[string]string{"GH_TOKEN": "tok", "X_TOKEN": "xtok"}

	consolidated, kv, svcs, err := brokeredSeedInputs("proj-lead", ac, services, flat)
	if err != nil {
		t.Fatalf("brokeredSeedInputs: %v", err)
	}
	if consolidated != "proj-lead-brokered" {
		t.Errorf("consolidated vault = %q, want proj-lead-brokered", consolidated)
	}
	if len(svcs) != 1 || svcs[0].Name != "github" {
		t.Errorf("want only the github service applied, got %+v", svcs)
	}
	if kv["GH_TOKEN"] != "tok" || len(kv) != 1 {
		t.Errorf("seed kv = %v, want only GH_TOKEN", kv)
	}
}

func TestBrokeredSeedInputs_ConsolidatedNameIsAgentVaultCompatible(t *testing.T) {
	// OS usernames may carry '_' via the sops vault naming path upstream;
	// AgentVaultName must normalize whatever comes in (agent-vault rejects
	// uppercase/underscore vault names).
	ac := config.AgentConfig{VaultBroker: true}
	consolidated, _, _, err := brokeredSeedInputs("Proj_Lead", ac, nil, map[string]string{})
	if err != nil {
		t.Fatalf("brokeredSeedInputs: %v", err)
	}
	if consolidated != "proj-lead-brokered" {
		t.Errorf("consolidated = %q, want normalized proj-lead-brokered", consolidated)
	}
}

func TestBrokeredSeedInputs_SeedsExtraServiceCredentialKeys(t *testing.T) {
	// A basic-auth service injects two keys; only the password is brokered.
	// The username must still be seeded so injection has both halves.
	ac := config.AgentConfig{
		VaultBroker:     true,
		BrokeredSecrets: []string{"ES_PW"},
	}
	services := []config.Service{{
		Name: "es", Host: "es.internal", AuthType: "basic",
		UsernameKey: "ES_USER_KEY", PasswordKey: "ES_PW",
	}}
	flat := map[string]string{"ES_PW": "pw", "ES_USER_KEY": "elastic"}

	_, kv, svcs, err := brokeredSeedInputs("proj-lead", ac, services, flat)
	if err != nil {
		t.Fatalf("brokeredSeedInputs: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("want the es service applied, got %+v", svcs)
	}
	if kv["ES_USER_KEY"] != "elastic" || kv["ES_PW"] != "pw" {
		t.Errorf("seed kv = %v, want both basic-auth halves", kv)
	}
}

func TestBrokeredServicesFor_RequiresAllCredentialKeysAvailable(t *testing.T) {
	// A service whose extra credential key is absent from the agent's vaults
	// must not be applied — half-seeded basic auth would inject garbage.
	brokered := map[string]bool{"ES_PW": true}
	services := []config.Service{{
		Name: "es", Host: "es.internal", AuthType: "basic",
		UsernameKey: "ES_USER_KEY", PasswordKey: "ES_PW",
	}}
	flat := map[string]string{"ES_PW": "pw"} // username missing
	if got := brokeredServicesFor(services, brokered, flat); len(got) != 0 {
		t.Errorf("service with unavailable credential key should be skipped, got %+v", got)
	}
}

// --- sidecarSecretEnv: resolves the upstream secrets a privileged sidecar
// listener holds. Wrong resolution either leaks the wrong agent's credential
// into a listener or half-installs a credential-holding service.

func sidecarCfg() *config.Config {
	return &config.Config{
		Project: "proj",
		Agents: map[string]config.AgentConfig{
			"lead": {Vaults: []string{"discord-lead"}},
		},
	}
}

func TestSidecarSecretEnv_SharedReadsNamedVault(t *testing.T) {
	l := config.SidecarListener{
		Server: config.SidecarServer{
			Name: "es", Secrets: []string{"ES_PASSWORD"}, SecretsVault: "es-vault",
		},
	}
	all := map[string]map[string]string{
		"es-vault": {"ES_PASSWORD": "hunter2", "UNRELATED": "x"},
	}
	env, err := sidecarSecretEnv(sidecarCfg(), l, all)
	if err != nil {
		t.Fatalf("sidecarSecretEnv: %v", err)
	}
	if len(env) != 1 || env["ES_PASSWORD"] != "hunter2" {
		t.Errorf("env = %v, want only ES_PASSWORD", env)
	}
}

func TestSidecarSecretEnv_SharedMissingVaultErrors(t *testing.T) {
	l := config.SidecarListener{
		Server: config.SidecarServer{
			Name: "es", Secrets: []string{"ES_PASSWORD"}, SecretsVault: "nope",
		},
	}
	if _, err := sidecarSecretEnv(sidecarCfg(), l, map[string]map[string]string{}); err == nil {
		t.Fatal("missing secrets_vault must error")
	}
}

func TestSidecarSecretEnv_MissingOrEmptySecretErrors(t *testing.T) {
	for name, vaultKV := range map[string]map[string]string{
		"missing key": {"OTHER": "x"},
		"empty value": {"ES_PASSWORD": ""},
	} {
		l := config.SidecarListener{
			Server: config.SidecarServer{
				Name: "es", Secrets: []string{"ES_PASSWORD"}, SecretsVault: "es-vault",
			},
		}
		all := map[string]map[string]string{"es-vault": vaultKV}
		if _, err := sidecarSecretEnv(sidecarCfg(), l, all); err == nil {
			t.Errorf("%s: must error rather than install a secretless listener", name)
		}
	}
}

func TestSidecarSecretEnv_PerAgentUsesSubscribersVaults(t *testing.T) {
	orig := decryptForAgent
	defer func() { decryptForAgent = orig }()
	var gotAgent string
	var gotVaults []string
	decryptForAgent = func(agentKey string, vaultNames []string) (map[string]string, error) {
		gotAgent, gotVaults = agentKey, vaultNames
		return map[string]string{"discord-lead.DISCORD_TOKEN": "tok-lead"}, nil
	}

	l := config.SidecarListener{
		Server:   config.SidecarServer{Name: "discord", Secrets: []string{"DISCORD_TOKEN"}, Identity: "per-agent"},
		AgentKey: "lead",
	}
	env, err := sidecarSecretEnv(sidecarCfg(), l, nil)
	if err != nil {
		t.Fatalf("sidecarSecretEnv: %v", err)
	}
	if gotAgent != "lead" || len(gotVaults) != 1 || gotVaults[0] != "discord-lead" {
		t.Errorf("decrypt called with agent=%q vaults=%v, want lead/[discord-lead]", gotAgent, gotVaults)
	}
	if env["DISCORD_TOKEN"] != "tok-lead" {
		t.Errorf("env = %v, want the subscriber's own token", env)
	}
}

func TestSidecarSecretEnv_PerAgentDecryptFailureErrors(t *testing.T) {
	orig := decryptForAgent
	defer func() { decryptForAgent = orig }()
	decryptForAgent = func(string, []string) (map[string]string, error) {
		return nil, fmt.Errorf("sops exploded")
	}
	l := config.SidecarListener{
		Server:   config.SidecarServer{Name: "discord", Secrets: []string{"DISCORD_TOKEN"}, Identity: "per-agent"},
		AgentKey: "lead",
	}
	if _, err := sidecarSecretEnv(sidecarCfg(), l, nil); err == nil {
		t.Fatal("decrypt failure must abort the listener, not install it secretless")
	}
}
