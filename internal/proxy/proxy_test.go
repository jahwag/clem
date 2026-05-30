package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jahwag/clem/internal/config"
)

// stubUIDs replaces userUIDLookup for a test and restores it on cleanup.
func stubUIDs(t *testing.T, uids map[string]int) {
	t.Helper()
	orig := userUIDLookup
	userUIDLookup = func(username string) (int, error) {
		if uid, ok := uids[username]; ok {
			return uid, nil
		}
		return 0, fmt.Errorf("user %q not found", username)
	}
	t.Cleanup(func() { userUIDLookup = orig })
}

func testCfg() *config.Config {
	return &config.Config{
		Project: "myteam",
		Egress: config.EgressConfig{
			Enabled:             true,
			Posture:             "strict",
			Domains:             []string{"*.anthropic.com", "github.com"},
			ProxyPort:           8888,
			AllowLocalhostPorts: []int{11434},
		},
		Agents: map[string]config.AgentConfig{
			"lead":   {Name: "Lead"},
			"worker": {Name: "Worker"},
		},
	}
}

func TestGeneratePipelockConfig(t *testing.T) {
	cfg := testCfg()
	out := GeneratePipelockConfig(cfg)
	for _, want := range []string{
		"mode: strict",
		"enforce: true",
		`listen: "127.0.0.1:8888"`,
		"enabled: true", // forward_proxy
		"sni_verification: true",
		"enabled: false", // tls_interception (CONNECT-only)
		`- "*.anthropic.com"`,
		`- "github.com"`,
		"file: /var/log/clem/pipelock-myteam-audit.jsonl",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pipelock config missing %q\n---\n%s", want, out)
		}
	}
}

func TestGeneratePipelockConfig_DefaultDomains(t *testing.T) {
	cfg := testCfg()
	cfg.Egress.Domains = nil
	out := GeneratePipelockConfig(cfg)
	if !strings.Contains(out, "*.anthropic.com") || !strings.Contains(out, "github.com") {
		t.Errorf("expected default domains, got:\n%s", out)
	}
}

func TestGeneratePipelockService(t *testing.T) {
	cfg := testCfg()
	out := GeneratePipelockService(cfg)
	for _, want := range []string{
		"User=clem-proxy",
		"ExecStart=/usr/local/bin/pipelock run --config /etc/clem/pipelock-myteam.yaml --listen 127.0.0.1:8888",
		"After=network-online.target clem-nftables-myteam.service",
		"ReadWritePaths=/var/log/clem",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pipelock service missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerateNftables(t *testing.T) {
	stubUIDs(t, map[string]int{
		"clem-proxy":    900,
		"myteam-lead":   1001,
		"myteam-worker": 1002,
	})
	cfg := testCfg()
	out, err := GenerateNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateNftables: %v", err)
	}
	for _, want := range []string{
		"table inet clem_egress_myteam {",
		"delete table inet clem_egress_myteam",
		"type filter hook output priority 0; policy accept;",
		"meta skuid 900 accept", // proxy egresses freely
		"meta skuid 1001 ip daddr 127.0.0.1 tcp dport { 8888, 11434 } accept",
		"meta skuid 1001 ip6 daddr ::1 tcp dport { 8888, 11434 } accept",
		"meta skuid 1001 reject with icmpx type admin-prohibited",
		"meta skuid 1002 ip daddr 127.0.0.1 tcp dport { 8888, 11434 } accept",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nftables missing %q\n---\n%s", want, out)
		}
	}
	// No agent has a web terminal here, so there must be NO blanket
	// established-state allow (that would be a reverse-shell exfil hole).
	if strings.Contains(out, "ct state established") {
		t.Errorf("unexpected established-state allow without a web terminal:\n%s", out)
	}
}

func TestGenerateNftables_TtydEstablishedScopedToPort(t *testing.T) {
	stubUIDs(t, map[string]int{"clem-proxy": 900, "myteam-lead": 1001})
	cfg := testCfg()
	cfg.Agents = map[string]config.AgentConfig{
		"lead": {Name: "Lead", WebTerminalPort: 7681},
	}
	out, err := GenerateNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateNftables: %v", err)
	}
	want := "meta skuid 1001 tcp sport 7681 ct state established,related accept"
	if !strings.Contains(out, want) {
		t.Errorf("expected ttyd-scoped established allow %q\n---\n%s", want, out)
	}
	// Must NOT be a blanket established allow (no sport qualifier).
	if strings.Contains(out, "meta skuid 1001 ct state established") {
		t.Errorf("established allow must be scoped to the ttyd sport, not blanket:\n%s", out)
	}
}

func TestGenerateNftables_SkipsDisabledAgent(t *testing.T) {
	stubUIDs(t, map[string]int{
		"clem-proxy":    900,
		"myteam-lead":   1001,
		"myteam-worker": 1002,
	})
	cfg := testCfg()
	off := false
	w := cfg.Agents["worker"]
	w.Egress = &off
	cfg.Agents["worker"] = w

	out, err := GenerateNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateNftables: %v", err)
	}
	if !strings.Contains(out, "meta skuid 1001") {
		t.Error("lead (enabled) should be in ruleset")
	}
	if strings.Contains(out, "meta skuid 1002") {
		t.Error("worker (egress: false) should NOT be in ruleset")
	}
}

func TestGenerateNftables_HyphenProjectSanitized(t *testing.T) {
	stubUIDs(t, map[string]int{"clem-proxy": 900, "my-team-lead": 1001})
	cfg := testCfg()
	cfg.Project = "my-team"
	cfg.Agents = map[string]config.AgentConfig{"lead": {Name: "Lead"}}
	out, err := GenerateNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateNftables: %v", err)
	}
	if !strings.Contains(out, "table inet clem_egress_my_team {") {
		t.Errorf("expected hyphen→underscore table name, got:\n%s", out)
	}
}

func TestGenerateNftablesService(t *testing.T) {
	cfg := testCfg()
	out := GenerateNftablesService(cfg)
	for _, want := range []string{
		"Type=oneshot",
		"RemainAfterExit=yes",
		"ExecStart=/usr/sbin/nft -f /etc/clem/clem-egress-myteam.nft",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nftables service missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerateAgentVaultService(t *testing.T) {
	cfg := testCfg()
	cfg.Vault = config.VaultBackend{Backend: "agent-vault"}
	out := GenerateAgentVaultService(cfg)
	for _, want := range []string{
		"User=clem-vault",
		"EnvironmentFile=/etc/clem/agent-vault.env",
		"Environment=HOME=/var/lib/clem-vault",
		"ReadWritePaths=/var/lib/clem-vault",
		"--port 14321",
		"--mitm-port 14322",
	} {
		if strings.Contains(out, "--data") {
			t.Errorf("agent-vault server has no --data flag; unit must not pass it\n---\n%s", out)
		}
		if !strings.Contains(out, want) {
			t.Errorf("agent-vault service missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerateNftables_BrokeredAgentAllowsAgentVaultPort(t *testing.T) {
	stubUIDs(t, map[string]int{"clem-proxy": 900, "myteam-lead": 1001, "myteam-worker": 1002})
	cfg := testCfg()
	cfg.Vault = config.VaultBackend{Backend: "agent-vault"}
	lead := cfg.Agents["lead"]
	lead.VaultBroker = true
	cfg.Agents["lead"] = lead

	out, err := GenerateNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateNftables: %v", err)
	}
	// Brokered lead may reach agent-vault's MITM port (14322); plain worker may not.
	if !strings.Contains(out, "meta skuid 1001 ip daddr 127.0.0.1 tcp dport { 8888, 11434, 14322 } accept") {
		t.Errorf("brokered agent should be allowed loopback to 14322:\n%s", out)
	}
	if strings.Contains(out, "meta skuid 1002 ip daddr 127.0.0.1 tcp dport { 8888, 11434, 14322 }") {
		t.Errorf("non-brokered agent must NOT get the agent-vault port:\n%s", out)
	}
}

// sidecarCfg builds a project with one shared sidecar (es-ro) subscribed by
// both agents and a custom base_port.
func sidecarCfg() *config.Config {
	return &config.Config{
		Project: "myteam",
		MCPSidecars: config.MCPSidecarsConfig{
			BasePort: 14500,
			Servers: []config.SidecarServer{{
				Name:         "es-ro",
				Identity:     "shared",
				Command:      "/usr/local/bin/es-mcp",
				Args:         []string{"--read-only"},
				Secrets:      []string{"ES_USER", "ES_PASSWORD"},
				SecretsVault: "infra",
			}},
		},
		Agents: map[string]config.AgentConfig{
			"lead":   {Name: "Lead", Sidecars: []string{"es-ro"}},
			"worker": {Name: "Worker", Sidecars: []string{"es-ro"}},
		},
	}
}

func TestGenerateSidecarService(t *testing.T) {
	cfg := sidecarCfg()
	l := cfg.SidecarListeners()[0]
	out := GenerateSidecarService(cfg, l)
	for _, want := range []string{
		"Description=clem MCP sidecar es-ro (myteam)",
		"User=clem-mcp",
		"Environment=HOME=/var/lib/clem-mcp",
		"EnvironmentFile=/etc/clem/clem-mcp-myteam-es-ro.env",
		"ExecStart=/opt/pipx/bin/mcp-proxy --host 127.0.0.1 --port 14500 --stateless --pass-environment -- /usr/local/bin/es-mcp --read-only",
		"ReadWritePaths=/var/lib/clem-mcp",
		"After=network-online.target clem-sidecar-nft-myteam.service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sidecar service missing %q\n---\n%s", want, out)
		}
	}
	// The secret must never appear on the command line (argv is world-readable).
	if strings.Contains(out, "ES_PASSWORD=") {
		t.Errorf("secret leaked into the unit:\n%s", out)
	}
}

func TestGenerateSidecarNftables(t *testing.T) {
	stubUIDs(t, map[string]int{"myteam-lead": 1001, "myteam-worker": 1002})
	cfg := sidecarCfg()
	out, err := GenerateSidecarNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateSidecarNftables: %v", err)
	}
	for _, want := range []string{
		"table inet clem_sidecar_myteam {",
		"delete table inet clem_sidecar_myteam",
		"type filter hook output priority -10; policy accept;",
		// Only the two subscriber UIDs may reach port 14500; everyone else dropped.
		"ip daddr 127.0.0.1 tcp dport 14500 meta skuid != { 1001, 1002 } drop",
		"ip6 daddr ::1 tcp dport 14500 meta skuid != { 1001, 1002 } drop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sidecar nftables missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerateSidecarNftables_PerAgentDistinctPortsAndUIDs(t *testing.T) {
	stubUIDs(t, map[string]int{"myteam-lead": 1001, "myteam-worker": 1002})
	cfg := sidecarCfg()
	cfg.MCPSidecars.Servers[0].Identity = "per-agent"
	out, err := GenerateSidecarNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateSidecarNftables: %v", err)
	}
	// per-agent → one listener per subscriber on successive ports, each locked to
	// that single subscriber's UID. Sorted subscribers: lead(14500), worker(14501).
	for _, want := range []string{
		"tcp dport 14500 meta skuid != { 1001 } drop",
		"tcp dport 14501 meta skuid != { 1002 } drop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("per-agent sidecar nftables missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerateSidecarNftablesService(t *testing.T) {
	cfg := sidecarCfg()
	out := GenerateSidecarNftablesService(cfg)
	for _, want := range []string{
		"Type=oneshot",
		"ExecStart=/usr/sbin/nft -f /etc/clem/clem-sidecar-myteam.nft",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sidecar nft service missing %q\n---\n%s", want, out)
		}
	}
}

// A contained agent that subscribes to a sidecar must be allowed (by the egress
// firewall) to reach the sidecar's loopback port, or egress's per-UID reject
// would block it before the sidecar firewall even applies.
func TestGenerateNftables_AllowsSidecarPortForContainedSubscriber(t *testing.T) {
	stubUIDs(t, map[string]int{"clem-proxy": 900, "myteam-lead": 1001})
	cfg := sidecarCfg()
	cfg.Egress = config.EgressConfig{Enabled: true, Posture: "strict", ProxyPort: 8888}
	cfg.Agents = map[string]config.AgentConfig{
		"lead": {Name: "Lead", Sidecars: []string{"es-ro"}},
	}
	out, err := GenerateNftables(cfg)
	if err != nil {
		t.Fatalf("GenerateNftables: %v", err)
	}
	want := "meta skuid 1001 ip daddr 127.0.0.1 tcp dport { 8888, 14500 } accept"
	if !strings.Contains(out, want) {
		t.Errorf("contained subscriber should be allowed to its sidecar port\nwant %q\n---\n%s", want, out)
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct{ in, def, want string }{
		{"http://127.0.0.1:14321", "x", "14321"},
		{"127.0.0.1:14322", "x", "14322"},
		{"[::1]:14322", "x", "14322"},
		{"::1", "9999", "9999"}, // bare IPv6, no port → default (n1 fix)
		{"", "14321", "14321"},
	}
	for _, c := range cases {
		if got := portOf(c.in, c.def); got != c.want {
			t.Errorf("portOf(%q,%q)=%q, want %q", c.in, c.def, got, c.want)
		}
	}
}
