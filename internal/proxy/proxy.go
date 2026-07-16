// Package proxy generates the on-disk artifacts for clem's credential and
// egress-containment stack: the agent-vault systemd service, the per-agent
// nftables UID firewall + its systemd loader, and the MCP sidecar listeners.
// clem writes these at provision time; at runtime clem is not in the loop.
//
// Containment model: egress-contained agents are brokered agents whose outbound
// HTTP(S) is forced through agent-vault's TLS-MITM proxy on loopback (a single
// agent-vault instance runs as a dedicated non-login system user). agent-vault
// allowlists the configured domains as passthrough services and denies every
// unmatched host. A root-owned nftables `output` chain rejects all egress for
// each agent UID except loopback to agent-vault's MITM port (and any
// explicitly-allowed loopback ports), so a compromised agent cannot reach the
// network except through agent-vault — the kernel UID firewall is the boundary,
// not the cooperative HTTPS_PROXY env var.
package proxy

import (
	"fmt"
	"net"
	"os/user"
	"sort"
	"strconv"
	"strings"

	"github.com/jahwag/clem/internal/config"
)

// userUIDLookup resolves an OS username to its numeric UID. Replaced in tests
// via package-level assignment (same pattern as runner.userHomeLookup).
var userUIDLookup = func(username string) (int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, fmt.Errorf("user %q not found: %w", username, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, fmt.Errorf("parsing uid for %q: %w", username, err)
	}
	return uid, nil
}

// NftablesPath returns the path to a project's nftables ruleset file.
func NftablesPath(project string) string {
	return fmt.Sprintf("/etc/clem/clem-egress-%s.nft", project)
}

// nftTableName returns the nftables table identifier for a project. nft
// identifiers disallow hyphens, so they are mapped to underscores.
func nftTableName(project string) string {
	return "clem_egress_" + strings.ReplaceAll(project, "-", "_")
}

// sidecarNftTableName returns the nftables table identifier for a project's
// sidecar loopback firewall (separate from the egress table so it applies even
// on hosts with no egress containment).
func sidecarNftTableName(project string) string {
	return "clem_sidecar_" + strings.ReplaceAll(project, "-", "_")
}

// SidecarNftablesPath returns the path to a project's sidecar firewall ruleset.
func SidecarNftablesPath(project string) string {
	return fmt.Sprintf("/etc/clem/clem-sidecar-%s.nft", project)
}

// SidecarEnvFile returns the root-owned (0600) EnvironmentFile holding a sidecar
// listener's upstream secret(s). systemd reads it as root and injects it into
// the service env before dropping to the mcp user; the secret never reaches an
// agent .env and never appears in the process argv.
func SidecarEnvFile(project, name, agentKey string) string {
	if agentKey != "" {
		return fmt.Sprintf("/etc/clem/clem-mcp-%s-%s-%s.env", project, name, agentKey)
	}
	return fmt.Sprintf("/etc/clem/clem-mcp-%s-%s.env", project, name)
}

// MCPProxyBin is the stdio→streamable-HTTP bridge clem installs (pipx-global,
// reachable by the mcp user). Sidecar listeners run a wrapped stdio MCP server
// behind it on loopback.
const MCPProxyBin = "/opt/pipx/bin/mcp-proxy"

// SidecarStateDir is the writable HOME/scratch dir for the mcp user, so wrapped
// stdio servers (node/python) that touch a cache dir work under ProtectSystem=strict.
const SidecarStateDir = "/var/lib/clem-mcp"

const nftablesServiceTemplate = `[Unit]
Description=clem egress firewall (nftables) for {{.Project}}
After=nftables.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f {{.NftPath}}

[Install]
WantedBy=multi-user.target
`

// GenerateNftablesService renders the oneshot systemd unit that applies the
// egress firewall ruleset on boot (before agent units, which order After it).
func GenerateNftablesService(cfg *config.Config) string {
	r := strings.NewReplacer(
		"{{.Project}}", cfg.Project,
		"{{.NftPath}}", NftablesPath(cfg.Project),
	)
	return r.Replace(nftablesServiceTemplate)
}

// AgentVaultEnvFile is the root-owned file holding agent-vault's master
// password, loaded by the systemd unit via EnvironmentFile (agent-vault has no
// systemd-credential support). Never readable by agent users.
const AgentVaultEnvFile = "/etc/clem/agent-vault.env"

// AgentVaultDataDir is the agent-vault encrypted store, owned by the vault user.
const AgentVaultDataDir = "/var/lib/clem-vault"

const agentVaultServiceTemplate = `[Unit]
Description=clem credential proxy (agent-vault) for {{.Project}}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{.VaultUser}}
# agent-vault has no data-dir flag; its encrypted store lives at $HOME/.agent-vault.
# Point HOME at the dedicated data dir so the store lands under ReadWritePaths.
Environment=HOME={{.DataDir}}
# Opt out of agent-vault's PostHog product-analytics telemetry.
# --log-level debug: the watchdog's deny-event alerting tails per-request
# proxy_request journal lines, which agent-vault only emits at debug level.
Environment=AGENT_VAULT_TELEMETRY=false
EnvironmentFile={{.EnvFile}}
# Keep large model/runtime artifacts streaming. agent-vault v0.22 silently
# truncated responses at 100 MiB; 0 means unlimited streaming, not buffering.
ExecStart=/usr/local/bin/agent-vault server --port {{.MgmtPort}} --mitm-port {{.MitmPort}} --max-response-bytes 0 --log-level debug
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths={{.DataDir}}
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`

// GenerateAgentVaultService renders the systemd unit running the agent-vault
// credential proxy as the dedicated vault system user. The master password is
// supplied via a root-owned EnvironmentFile (agent-vault reads
// AGENT_VAULT_MASTER_PASSWORD from env; it has no systemd-credential support).
func GenerateAgentVaultService(cfg *config.Config) string {
	addr := cfg.Vault.AddrOrDefault()
	mgmtPort := portOf(addr, "14321")
	mitmPort := portOf(cfg.Vault.ProxyHostOrDefault(), "14322")
	r := strings.NewReplacer(
		"{{.Project}}", cfg.Project,
		"{{.VaultUser}}", cfg.Vault.SystemUserOrDefault(),
		"{{.EnvFile}}", AgentVaultEnvFile,
		"{{.DataDir}}", AgentVaultDataDir,
		"{{.MgmtPort}}", mgmtPort,
		"{{.MitmPort}}", mitmPort,
	)
	return r.Replace(agentVaultServiceTemplate)
}

// portOf extracts the port from a host:port or scheme://host:port string
// (IPv6-safe via net.SplitHostPort), falling back to def.
func portOf(s, def string) string {
	s = strings.TrimRight(strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://"), "/")
	if _, port, err := net.SplitHostPort(s); err == nil && port != "" {
		return port
	}
	return def
}

// agentVaultPort returns the numeric loopback port agents reach agent-vault's
// MITM proxy on, or 0 if the agent-vault backend is not active.
func agentVaultPort(cfg *config.Config) int {
	if !cfg.Vault.IsAgentVault() {
		return 0
	}
	n, err := strconv.Atoi(portOf(cfg.Vault.ProxyHostOrDefault(), "14322"))
	if err != nil {
		return 14322
	}
	return n
}

// GenerateNftables renders the per-agent UID egress firewall for a project.
// Each egress-enabled agent's UID is rejected for all traffic except loopback
// to agent-vault's MITM port (and any AllowLocalhostPorts); the agent-vault
// system user egresses freely so it can reach allowlisted upstreams on the
// agent's behalf. The default-accept policy leaves all other users (root,
// services) untouched. Idempotent reload: ensure-then-delete-then-create.
func GenerateNftables(cfg *config.Config) (string, error) {
	proxyUID, err := userUIDLookup(cfg.Vault.SystemUserOrDefault())
	if err != nil {
		return "", fmt.Errorf("resolving agent-vault system user: %w", err)
	}

	// Base allowed loopback destination ports: any operator extras (e.g. Ollama).
	// agent-vault's MITM port is added per brokered agent below.
	basePorts := append([]int(nil), cfg.Egress.AllowLocalhostPorts...)
	avPort := agentVaultPort(cfg) // 0 unless agent-vault backend active

	// Sidecar loopback ports each agent subscribes to. A contained agent reaches
	// its sidecar over loopback, so those ports must be in its allowed set or the
	// per-UID reject below would block them. (Cross-agent isolation on those
	// ports is enforced separately by GenerateSidecarNftables.)
	sidecarPortsByAgent := map[string][]int{}
	for _, l := range cfg.SidecarListeners() {
		for _, ak := range l.Subscribers {
			sidecarPortsByAgent[ak] = append(sidecarPortsByAgent[ak], l.Port)
		}
	}

	// portListFor returns the deduped, sorted "p1, p2" loopback port set for an
	// agent: the base ports, plus agent-vault's MITM port for brokered agents
	// (their HTTPS_PROXY points at agent-vault), plus any sidecar ports the
	// agent subscribes to.
	portListFor := func(key string, ac config.AgentConfig) string {
		set := map[int]struct{}{}
		for _, p := range basePorts {
			set[p] = struct{}{}
		}
		if avPort > 0 && ac.VaultBroker {
			set[avPort] = struct{}{}
		}
		for _, p := range sidecarPortsByAgent[key] {
			set[p] = struct{}{}
		}
		ports := make([]int, 0, len(set))
		for p := range set {
			ports = append(ports, p)
		}
		sort.Ints(ports)
		strs := make([]string, len(ports))
		for i, p := range ports {
			strs[i] = strconv.Itoa(p)
		}
		return strings.Join(strs, ", ")
	}

	// Collect contained agent UIDs, sorted for deterministic output.
	keys := make([]string, 0, len(cfg.Agents))
	for k := range cfg.Agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type agentUID struct {
		key string
		uid int
	}
	var agents []agentUID
	for _, k := range keys {
		if !cfg.EgressEnabledFor(k) {
			continue
		}
		uid, err := userUIDLookup(cfg.OSUsername(k))
		if err != nil {
			return "", fmt.Errorf("resolving agent %s: %w", k, err)
		}
		agents = append(agents, agentUID{k, uid})
	}

	table := nftTableName(cfg.Project)
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("# Generated by clem provision — egress containment for " + cfg.Project + ".\n")
	b.WriteString("# Rejects all egress for each agent UID except loopback to agent-vault's MITM proxy.\n")
	// Idempotent reload idiom: create-if-absent, delete, recreate.
	fmt.Fprintf(&b, "table inet %s { }\n", table)
	fmt.Fprintf(&b, "delete table inet %s\n", table)
	fmt.Fprintf(&b, "table inet %s {\n", table)
	b.WriteString("\tchain output {\n")
	b.WriteString("\t\ttype filter hook output priority 0; policy accept;\n")
	fmt.Fprintf(&b, "\t\t# %s runs agent-vault and egresses freely\n", cfg.Vault.SystemUserOrDefault())
	fmt.Fprintf(&b, "\t\tmeta skuid %d accept\n", proxyUID)
	for _, a := range agents {
		ac := cfg.Agents[a.key]
		fmt.Fprintf(&b, "\t\t# agent %s (%s)\n", a.key, cfg.OSUsername(a.key))
		// ttyd web-terminal replies are inbound-initiated, so their outbound
		// packets must be allowed — but ONLY from the ttyd source port. A blanket
		// `ct state established` allow would let a compromised agent run a bind
		// or reverse shell and exfiltrate over an attacker-initiated connection,
		// defeating containment. Scope it to the ttyd port; agents with no web
		// terminal get no established-state allowance at all. (The proxy path
		// needs none: outbound packets to the proxy always match the dport rule.)
		if ac.WebTerminalPort > 0 {
			fmt.Fprintf(&b, "\t\tmeta skuid %d tcp sport %d ct state established,related accept\n", a.uid, ac.WebTerminalPort)
		}
		portList := portListFor(a.key, ac)
		fmt.Fprintf(&b, "\t\tmeta skuid %d ip daddr 127.0.0.1 tcp dport { %s } accept\n", a.uid, portList)
		fmt.Fprintf(&b, "\t\tmeta skuid %d ip6 daddr ::1 tcp dport { %s } accept\n", a.uid, portList)
		fmt.Fprintf(&b, "\t\tmeta skuid %d reject with icmpx type admin-prohibited\n", a.uid)
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String(), nil
}

const sidecarServiceTemplate = `[Unit]
Description=clem MCP sidecar {{.Name}} ({{.Project}})
# Fail-closed: the loopback firewall is the ONLY thing keeping non-subscriber
# agents off this credential-holding port (mcp-proxy has no incoming auth), so a
# listener must NOT start if the firewall failed to load — mirrors the egress
# agent unit's Requires= on its nftables service.
Requires={{.SidecarNftService}}
After=network-online.target {{.SidecarNftService}}
Wants=network-online.target

[Service]
Type=simple
User={{.MCPUser}}
# clem-mcp has no home; point HOME at a writable scratch dir so node/python
# wrapped servers can use their cache under ProtectSystem=strict.
Environment=HOME={{.StateDir}}
# Upstream secret(s) loaded root-side, injected into the spawned stdio server
# via --pass-environment. Never on the command line (argv is world-readable).
EnvironmentFile={{.EnvFile}}
ExecStart={{.ExecStart}}
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths={{.StateDir}}
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`

// GenerateSidecarService renders the systemd unit running one sidecar listener:
// mcp-proxy exposes the wrapped stdio MCP server over loopback streamable-HTTP
// on the listener's port, as the dedicated mcp system user. The upstream secret
// arrives via EnvironmentFile + --pass-environment, so it is neither in any
// agent .env nor in the process argv.
func GenerateSidecarService(cfg *config.Config, l config.SidecarListener) string {
	args := []string{
		MCPProxyBin,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(l.Port),
		"--stateless",
		"--pass-environment",
		"--",
		l.Server.Command,
	}
	args = append(args, l.Server.Args...)
	r := strings.NewReplacer(
		"{{.Name}}", l.Server.Name,
		"{{.Project}}", cfg.Project,
		"{{.MCPUser}}", cfg.MCPSidecars.SystemUserOrDefault(),
		"{{.StateDir}}", SidecarStateDir,
		"{{.EnvFile}}", SidecarEnvFile(cfg.Project, l.Server.Name, l.AgentKey),
		"{{.ExecStart}}", strings.Join(args, " "),
		"{{.SidecarNftService}}", cfg.SidecarNftablesServiceName(),
	)
	return r.Replace(sidecarServiceTemplate)
}

// GenerateSidecarNftables renders the loopback firewall for sidecar listeners:
// for each listener's port, only the subscribing agent UID(s) may open a
// connection; every other UID (including other agents and root) is dropped.
// This is the containment boundary on hosts without egress firewalling, where
// agents would otherwise reach each other's sidecar ports freely on loopback.
// Idempotent reload: ensure-then-delete-then-create the table.
func GenerateSidecarNftables(cfg *config.Config) (string, error) {
	listeners := cfg.SidecarListeners()
	table := sidecarNftTableName(cfg.Project)
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("# Generated by clem provision — sidecar loopback containment for " + cfg.Project + ".\n")
	b.WriteString("# Each sidecar port is reachable only by its subscribing agent UID(s).\n")
	fmt.Fprintf(&b, "table inet %s { }\n", table)
	fmt.Fprintf(&b, "delete table inet %s\n", table)
	fmt.Fprintf(&b, "table inet %s {\n", table)
	b.WriteString("\tchain output {\n")
	// priority -10 evaluates before the egress table (priority 0) so the
	// cross-UID drop is guaranteed to run first rather than relying on the
	// undefined ordering between two base chains at the same hook+priority.
	b.WriteString("\t\ttype filter hook output priority -10; policy accept;\n")
	for _, l := range listeners {
		uids := make([]int, 0, len(l.Subscribers))
		for _, ak := range l.Subscribers {
			uid, err := userUIDLookup(cfg.OSUsername(ak))
			if err != nil {
				return "", fmt.Errorf("resolving sidecar %s subscriber %s: %w", l.Server.Name, ak, err)
			}
			uids = append(uids, uid)
		}
		sort.Ints(uids)
		strs := make([]string, len(uids))
		for i, u := range uids {
			strs[i] = strconv.Itoa(u)
		}
		set := strings.Join(strs, ", ")
		fmt.Fprintf(&b, "\t\t# sidecar %s on port %d — subscribers: %s\n", l.Server.Name, l.Port, strings.Join(l.Subscribers, ", "))
		fmt.Fprintf(&b, "\t\tip daddr 127.0.0.1 tcp dport %d meta skuid != { %s } drop\n", l.Port, set)
		fmt.Fprintf(&b, "\t\tip6 daddr ::1 tcp dport %d meta skuid != { %s } drop\n", l.Port, set)
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String(), nil
}

const sidecarNftablesServiceTemplate = `[Unit]
Description=clem sidecar loopback firewall (nftables) for {{.Project}}
After=nftables.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f {{.NftPath}}

[Install]
WantedBy=multi-user.target
`

// GenerateSidecarNftablesService renders the oneshot unit that applies the
// sidecar firewall ruleset (before agent + sidecar units, which order After it).
func GenerateSidecarNftablesService(cfg *config.Config) string {
	r := strings.NewReplacer(
		"{{.Project}}", cfg.Project,
		"{{.NftPath}}", SidecarNftablesPath(cfg.Project),
	)
	return r.Replace(sidecarNftablesServiceTemplate)
}
