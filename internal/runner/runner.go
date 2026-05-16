package runner

import (
	"fmt"
	"os/user"
	"sort"
	"strings"

	"github.com/jahwag/clem/internal/agentdoc"
	"github.com/jahwag/clem/internal/config"
	"github.com/jahwag/clem/internal/coordination"
)

// userHomeLookup returns the home directory for the named OS user.
// Replaced in tests via package-level assignment.
var userHomeLookup = func(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", fmt.Errorf("user %q not found: %w", username, err)
	}
	return u.HomeDir, nil
}

const runnerTemplate = `#!/bin/bash
set -m
BACKOFF=10
MAX_BACKOFF=900
RESET_AFTER=300
CLAUDE="$HOME/.local/bin/claude"
WORKDIR="$HOME/{{.Project}}"
LOGFILE="$HOME/.claude/{{.AgentKey}}-runner.log"

cd "$WORKDIR" || exit 1

log() { echo "$(date -Iseconds) $1" | tee -a "$LOGFILE"; }

tail -500 "$LOGFILE" > "$LOGFILE.tmp" 2>/dev/null && mv "$LOGFILE.tmp" "$LOGFILE" 2>/dev/null

# Disable claude.ai connector MCPs (Figma/Gmail/Drive/M365/...) — agents are
# headless workers, never need human-account connectors, and the bundled tool
# lists eat ~1-2k tokens per session. Exported BEFORE sourcing .env so
# operators can re-enable per-host by setting the var in $HOME/.env.
export ENABLE_CLAUDEAI_MCP_SERVERS=false
# Skip IDE extension auto-install probe — agents run in headless tmux, no IDE.
export CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL=1

# Load secrets (written by clem provision, never committed)
[ -f "$HOME/.env" ] && source "$HOME/.env"
{{.SubagentExport}}
# Write ephemeral .mcp.json from env (python3 ensures correct JSON encoding).
# Each Python-based MCP server is resolved via _mcp_bin which prefers an
# isolated pipx venv at /opt/pipx/bin if present and falls back to the
# system pip install at /usr/local/bin. Pipx is the supported install path
# (see README): each MCP gets its own pydantic + pydantic-core pair, so a
# system pydantic-core upgrade cannot desync from the wheel an MCP was
# built against. Pre-0.9.5 hardcoded /usr/local/bin and broke every time
# system Python state drifted, requiring jahwag to re-edit .mcp.json every
# iteration because the runner overwrites it.
python3 -c "
import json, os
def _mcp_bin(name):
    pipx = '/opt/pipx/bin/' + name
    sysbin = '/usr/local/bin/' + name
    return pipx if os.path.exists(pipx) else sysbin
cfg = {'mcpServers': {}}
# Discord bot. When channel IDs are configured the MCP server also runs a
# gateway watcher that pushes one debounced notification per burst into this
# agent's tmux session — see mcp-discord's CLEM_TMUX_TARGET docs.
if os.environ.get('DISCORD_TOKEN'):
    _discord_env = {'DISCORD_TOKEN': os.environ['DISCORD_TOKEN']}
    _watch = '{{.WatchChannelIDs}}'
    if _watch:
        _discord_env['DISCORD_WATCH_CHANNELS'] = _watch
        _discord_env['CLEM_TMUX_TARGET'] = '{{.AgentKey}}'
    cfg['mcpServers']['discord-bot'] = {'command': _mcp_bin('mcp-discord'), 'env': _discord_env}
# Slack (korotovsky/slack-mcp-server). Read access is free; write access
# (conversations_add_message) requires SLACK_MCP_ADD_MESSAGE_TOOL — enabled
# here by default so agents can actually post, matching the Discord default.
#
# SLACK_MCP_ENABLED_TOOLS is optional: comma-separated list to restrict the
# exposed toolset. Useful for small local models (e.g. Nemotron 4B) that get
# confused by the full 13-tool surface. Leave unset on cloud Claude / Opus.
#
# slack-mcp-server is a Go binary (not Python) so the pipx fallback does
# not apply; we still resolve it through _mcp_bin for symmetry / future-
# proofing in case the upstream ships a Python version.
if os.environ.get('SLACK_MCP_XOXP_TOKEN'):
    slack_args = ['--transport', 'stdio']
    if os.environ.get('SLACK_MCP_ENABLED_TOOLS'):
        slack_args += ['--enabled-tools', os.environ['SLACK_MCP_ENABLED_TOOLS']]
    cfg['mcpServers']['slack-mcp'] = {
        'command': _mcp_bin('slack-mcp-server'),
        'args': slack_args,
        'env': {
            'SLACK_MCP_XOXP_TOKEN': os.environ['SLACK_MCP_XOXP_TOKEN'],
            'SLACK_MCP_ADD_MESSAGE_TOOL': os.environ.get('SLACK_MCP_ADD_MESSAGE_TOOL', 'true'),
        },
    }
# Prefect MCP (needs SSH_HOST + ES_PASSWORD)
if os.environ.get('SSH_HOST') and os.environ.get('ES_PASSWORD'):
    cfg['mcpServers']['prefect'] = {
        'command': _mcp_bin('prefect-mcp'),
        'env': {
            'SSH_HOST': os.environ['SSH_HOST'],
            'SSH_USER': os.environ.get('SSH_USER', 'ubuntu'),
            'SSH_KEY_PATH': os.path.expanduser('~/.ssh/id_ed25519'),
            'PREFECT_API_PORT': os.environ.get('PREFECT_API_PORT', '4200'),
            'ES_USER': os.environ.get('ES_USER', 'elastic'),
            'ES_PASSWORD': os.environ['ES_PASSWORD'],
        }
    }
# GitHub MCP and context7 are NOT registered here by default — agents use
# the gh CLI directly (more context-efficient per Anthropic's cost docs) and
# can opt in to context7 per-project by checking a .mcp.json into the workdir.
# Social media (Typefully backend — local MCP server)
if os.environ.get('TYPEFULLY_API_KEY'):
    cfg['mcpServers']['social'] = {
        'command': _mcp_bin('social-mcp'),
        'env': {'TYPEFULLY_API_KEY': os.environ['TYPEFULLY_API_KEY']}
    }
print(json.dumps(cfg, indent=2))
" > "$WORKDIR/.mcp.json"

SLEEP_ACTIVE={{.SleepActive}}
SLEEP_NIGHT={{.SleepNight}}
MAX_CLAUDE_MD_BYTES=12288
MAX_LESSONS_MESSAGES=25

while true; do
    START=$(date +%s)
    PROMPT='{{.Prompt}}'

    # Guard: CLAUDE.local.md too large (token waste)
    if [ -f "$WORKDIR/CLAUDE.local.md" ]; then
        SIZE=$(stat -c %s "$WORKDIR/CLAUDE.local.md" 2>/dev/null || echo 0)
        if (( SIZE > MAX_CLAUDE_MD_BYTES )); then
            log "WARNING: CLAUDE.local.md is ${SIZE} bytes (max ${MAX_CLAUDE_MD_BYTES}) — alerting"
            source "$HOME/.env" 2>/dev/null
            {{.AlertCurl}}
        fi
    fi

    log "Updating claude"
    "$CLAUDE" install 2>&1 | tail -5 | tee -a "$LOGFILE" || log "claude install failed, continuing with current version"

    log "Starting {{.AgentName}} (fresh session)"
    (sleep 1 && tmux send-keys -t {{.AgentKey}} "" Enter
     sleep 25 && tmux send-keys -l -t {{.AgentKey}} "$PROMPT"
     sleep 2 && tmux send-keys -t {{.AgentKey}} Enter) &
    timeout 7200 $CLAUDE --dangerously-skip-permissions \
        --model '{{.Model}}' \
        --name '{{.AgentName}}' \
        --add-dir ~/.claude

    EXIT_CODE=$?
    ELAPSED=$(( $(date +%s) - START ))
    log "Exited $EXIT_CODE after ${ELAPSED}s"

    HOUR=$(date +%H)
    if [ "$HOUR" -ge 7 ] && [ "$HOUR" -lt 22 ]; then
        SLEEP_BETWEEN=$SLEEP_ACTIVE
    else
        SLEEP_BETWEEN=$SLEEP_NIGHT
    fi

    if [ $EXIT_CODE -eq 143 ] || [ $ELAPSED -gt $RESET_AFTER ]; then
        BACKOFF=$SLEEP_BETWEEN
    else
        BACKOFF=$(( BACKOFF * 2 ))
        [ $BACKOFF -gt $MAX_BACKOFF ] && BACKOFF=$MAX_BACKOFF
    fi

    log "Sleeping ${BACKOFF}s"
    sleep $BACKOFF
done
`

// opencodeRunnerTemplate is the runner loop for agents using the opencode CLI.
// Opencode talks natively to 75+ providers (including Ollama) via models.dev, so
// no Anthropic-format translator is in the middle. MCP servers are configured
// via opencode.json in the workdir.
const opencodeRunnerTemplate = `#!/bin/bash
set -m
BACKOFF=10
MAX_BACKOFF=900
RESET_AFTER=300
OPENCODE="$HOME/.opencode/bin/opencode"
WORKDIR="$HOME/{{.Project}}"
LOGFILE="$HOME/.claude/{{.AgentKey}}-runner.log"

mkdir -p "$HOME/.claude"
cd "$WORKDIR" || exit 1

log() { echo "$(date -Iseconds) $1" | tee -a "$LOGFILE"; }

tail -500 "$LOGFILE" > "$LOGFILE.tmp" 2>/dev/null && mv "$LOGFILE.tmp" "$LOGFILE" 2>/dev/null

[ -f "$HOME/.env" ] && source "$HOME/.env"
{{.SubagentExport}}
# Write opencode.json with Ollama provider + discord-bot MCP (if token is set).
# MCP binary paths come from _mcp_bin (pipx-isolated venv preferred over system
# pip install — see the claude-code runner template above for the rationale).
python3 -c "
import json, os
def _mcp_bin(name):
    pipx = '/opt/pipx/bin/' + name
    sysbin = '/usr/local/bin/' + name
    return pipx if os.path.exists(pipx) else sysbin
cfg = {
    '\$schema': 'https://opencode.ai/config.json',
    'provider': {},
    'mcp': {},
}
base_url = os.environ.get('ANTHROPIC_BASE_URL', 'http://127.0.0.1:11434') + '/v1'
if os.environ.get('ANTHROPIC_MODEL'):
    cfg['provider']['ollama'] = {
        'name': 'Ollama',
        'npm': '@ai-sdk/openai-compatible',
        'options': {'baseURL': base_url},
        'models': {os.environ['ANTHROPIC_MODEL']: {}},
    }
if os.environ.get('DISCORD_TOKEN'):
    _discord_env = {'DISCORD_TOKEN': os.environ['DISCORD_TOKEN']}
    _watch = '{{.WatchChannelIDs}}'
    if _watch:
        _discord_env['DISCORD_WATCH_CHANNELS'] = _watch
        _discord_env['CLEM_TMUX_TARGET'] = '{{.AgentKey}}'
    cfg['mcp']['discord-bot'] = {
        'type': 'local',
        'command': [_mcp_bin('mcp-discord')],
        'enabled': True,
        'environment': _discord_env,
    }
if os.environ.get('SLACK_MCP_XOXP_TOKEN'):
    slack_cmd = [_mcp_bin('slack-mcp-server'), '--transport', 'stdio']
    if os.environ.get('SLACK_MCP_ENABLED_TOOLS'):
        slack_cmd += ['--enabled-tools', os.environ['SLACK_MCP_ENABLED_TOOLS']]
    cfg['mcp']['slack-mcp'] = {
        'type': 'local',
        'command': slack_cmd,
        'enabled': True,
        'environment': {
            'SLACK_MCP_XOXP_TOKEN': os.environ['SLACK_MCP_XOXP_TOKEN'],
            'SLACK_MCP_ADD_MESSAGE_TOOL': os.environ.get('SLACK_MCP_ADD_MESSAGE_TOOL', 'true'),
        },
    }
print(json.dumps(cfg, indent=2))
" > "$WORKDIR/opencode.json"

SLEEP_ACTIVE={{.SleepActive}}
SLEEP_NIGHT={{.SleepNight}}
MAX_CLAUDE_MD_BYTES=12288
MAX_LESSONS_MESSAGES=25

while true; do
    START=$(date +%s)
    PROMPT='{{.Prompt}}'

    # Guard: CLAUDE.local.md too large (token waste)
    if [ -f "$WORKDIR/CLAUDE.local.md" ]; then
        SIZE=$(stat -c %s "$WORKDIR/CLAUDE.local.md" 2>/dev/null || echo 0)
        if (( SIZE > MAX_CLAUDE_MD_BYTES )); then
            log "WARNING: CLAUDE.local.md is ${SIZE} bytes (max ${MAX_CLAUDE_MD_BYTES}) — alerting"
            source "$HOME/.env" 2>/dev/null
            {{.AlertCurl}}
        fi
    fi

    log "Starting {{.AgentName}} (opencode, fresh session)"
    MODEL_ARG=""
    [ -n "$ANTHROPIC_MODEL" ] && MODEL_ARG="--model ollama/$ANTHROPIC_MODEL"
    (sleep 1 && tmux send-keys -t {{.AgentKey}} "" Enter
     sleep 10 && tmux send-keys -l -t {{.AgentKey}} "$PROMPT"
     sleep 2 && tmux send-keys -t {{.AgentKey}} Enter) &
    timeout 7200 $OPENCODE $MODEL_ARG

    EXIT_CODE=$?
    ELAPSED=$(( $(date +%s) - START ))
    log "Exited $EXIT_CODE after ${ELAPSED}s"

    HOUR=$(date +%H)
    if [ "$HOUR" -ge 7 ] && [ "$HOUR" -lt 22 ]; then
        SLEEP_BETWEEN=$SLEEP_ACTIVE
    else
        SLEEP_BETWEEN=$SLEEP_NIGHT
    fi

    if [ $EXIT_CODE -eq 143 ] || [ $ELAPSED -gt $RESET_AFTER ]; then
        BACKOFF=$SLEEP_BETWEEN
    else
        BACKOFF=$(( BACKOFF * 2 ))
        [ $BACKOFF -gt $MAX_BACKOFF ] && BACKOFF=$MAX_BACKOFF
    fi

    log "Sleeping ${BACKOFF}s"
    sleep $BACKOFF
done
`

const serviceTemplate = `[Unit]
Description=Clem agent: {{.AgentName}} ({{.Project}})
After=network.target
# Pull the web-terminal sidecar up alongside the agent. The ttyd unit's
# BindsTo+PartOf already propagate stops back, but neither propagates a fresh
# start, so without a Wants here a "systemctl start" of the agent leaves the
# terminal dead until provision re-enables it.
Wants=clem-ttyd-{{.Project}}-{{.AgentKey}}.service

[Service]
Type=forking
User={{.OSUser}}
ExecStart=/usr/bin/tmux new-session -d -s {{.AgentKey}} {{.HomeDir}}/.local/bin/clem-runner.sh
ExecStop=/usr/bin/tmux kill-session -t {{.AgentKey}}
RemainAfterExit=yes
Restart=no
{{.HardeningDirectives}}{{.EgressDirectives}}
[Install]
WantedBy=multi-user.target
`

// egressDirectives is the systemd IP firewall block injected when
// egress_restriction_experimental is enabled. Allows GitHub (git + API),
// Anthropic/Discord (via Cloudflare), and localhost (Ollama, MCP unix sockets).
//
// KNOWN LIMITATIONS — see AgentConfig.EgressRestrictionExperimental doc for full detail:
//   - DNS: only works with systemd-resolved (127.0.0.53); external resolvers fail.
//   - Cloudflare CIDRs cover millions of CF-hosted sites, not just Anthropic/Discord.
//   - CIDRs are hardcoded and will drift. Refresh with:
//       curl https://api.github.com/meta | jq '[.web[], .api[], .git[]] | unique[]'
//   - DNS exfil (base64 in subdomain labels) is NOT blocked by IP-level filtering.
const egressDirectives = `# Egress restriction (egress_restriction_experimental: true)
# EXPERIMENTAL: see clem.yaml AgentConfig docs for known limitations.
IPAddressDeny=any
IPAddressAllow=localhost
IPAddressAllow=127.0.0.0/8
IPAddressAllow=::1/128
# GitHub (web + API + git)
IPAddressAllow=140.82.112.0/20
IPAddressAllow=185.199.108.0/22
IPAddressAllow=192.30.252.0/22
IPAddressAllow=143.55.64.0/20
# Anthropic API + Discord (both served via Cloudflare)
IPAddressAllow=104.16.0.0/13
IPAddressAllow=104.24.0.0/14
IPAddressAllow=172.64.0.0/13
# Discord own ASN (AS36459)
IPAddressAllow=66.22.192.0/20
`

const ttydServiceTemplate = `[Unit]
Description=Clem web terminal: {{.AgentName}} ({{.Project}})
After=clem-{{.Project}}-{{.AgentKey}}.service
BindsTo=clem-{{.Project}}-{{.AgentKey}}.service
PartOf=clem-{{.Project}}-{{.AgentKey}}.service
# The agent unit runs with PrivateTmp=yes, so its tmux socket lives in a
# private /tmp namespace. ttyd must enter that same namespace to attach.
# JoinsNamespaceOf belongs in [Unit] (not [Service]); systemd silently
# ignores it elsewhere. The directive is also a no-op unless this unit
# itself enables PrivateTmp below.
JoinsNamespaceOf=clem-{{.Project}}-{{.AgentKey}}.service

[Service]
Type=simple
User={{.OSUser}}
PrivateTmp=yes
ExecStart=/usr/local/bin/ttyd -R -i {{.TtydBind}} -p {{.TtydPort}} tmux attach-session -t {{.AgentKey}}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`

type RunnerParams struct {
	Project           string
	AgentKey          string
	AgentName         string
	Model             string
	SubagentExport    string
	Prompt            string
	OSUser            string
	HomeDir           string
	SleepActive       int
	SleepNight        int
	TtydPort          int
	TtydBind          string
	AlertChannel      string
	AlertCurl         string
	EgressDirectives    string
	HardeningDirectives string
	// WatchChannelIDs is the comma-separated list of Discord channel IDs the
	// MCP server's gateway watcher should observe. Empty disables the watcher
	// even when DISCORD_TOKEN is set, preserving the original tool-only mode.
	WatchChannelIDs   string
}

// Generate renders the runner.sh content for an agent. Dispatches on the
// agent's runtime (claude-code default, or opencode).
func Generate(cfg *config.Config, agentKey string) string {
	ac := cfg.Agents[agentKey]
	iterDur, _ := ac.IterationDuration() // validated at load time
	iterSec := int(iterDur.Seconds())

	// Render {{agent.name}}, {{channels.*}}, etc. in the operator-authored
	// prompt the same way CLAUDE.local.md is rendered. Without this, agents
	// receive the literal placeholder text and cannot identify themselves.
	promptText := agentdoc.Substitute(ac.Prompt, cfg, agentKey)
	if ac.Caveman.Enabled() {
		promptText = "/caveman " + ac.Caveman.Level() + "\n" + promptText
	}
	// Interactive TUIs (claude-code, opencode) do not exit after completing a
	// prompt — they wait for the next tmux-injected input. The runner loop
	// only advances when the session ends, so the agent itself must kill the
	// shell ($PPID of claude = the tmux window's bash). Auto-append the
	// instruction when the operator didn't include it, so short-loop demos
	// and forgetful configs still cycle correctly.
	if !strings.Contains(promptText, "kill $PPID") {
		promptText = strings.TrimRight(promptText, " \n") + "\nWhen done with this iteration, run bash: kill $PPID"
	}

	alertChannel := cfg.Coordination.Channels["alerts"]
	backend, _ := coordination.Known(cfg.Coordination.Backend) // validated at load time
	alertMsg := fmt.Sprintf(`⚠️ %s: CLAUDE.local.md is ${SIZE} bytes (>${MAX_CLAUDE_MD_BYTES}). Trim it to reduce token waste.`, ac.Name)
	alertCurl := fmt.Sprintf(`[ -n "$%s" ] && %s`, backend.TokenEnvVar, fmt.Sprintf(backend.AlertTemplate, alertChannel, alertMsg))

	subagentExport := ""
	if ac.SubagentModel != "" {
		subagentExport = fmt.Sprintf("export CLAUDE_CODE_SUBAGENT_MODEL=%q", ac.SubagentModel)
	}
	p := RunnerParams{
		Project:         cfg.Project,
		AgentKey:        agentKey,
		AgentName:       ac.Name,
		Model:           ac.Model,
		SubagentExport:  subagentExport,
		Prompt:          strings.ReplaceAll(promptText, "'", `'\''`),
		OSUser:          cfg.OSUsername(agentKey),
		HomeDir:         fmt.Sprintf("/home/%s", cfg.OSUsername(agentKey)),
		SleepActive:     iterSec,
		// Night sleep matches active. The previous 2x doubler hurt spend:
		// Anthropic's prompt cache TTL is 5 min, so any iter > 5m at night
		// guaranteed a cache miss every session — same session count cut
		// you'd get from cold-cache cost increase. Match active to keep cache
		// hot, or override per-iteration in clem.yaml directly.
		SleepNight:      iterSec,
		AlertChannel:    alertChannel,
		AlertCurl:       alertCurl,
		WatchChannelIDs: discordWatchChannels(cfg),
	}
	switch ac.RuntimeKind() {
	case "opencode":
		return renderTemplate(opencodeRunnerTemplate, p)
	default:
		return renderTemplate(runnerTemplate, p)
	}
}

// buildHardeningDirectives returns the systemd filesystem hardening block for
// an agent service. homeDir must come from os/user.Lookup — not %h, which
// resolves to the service manager's home (root) in system units (systemd #12389).
//
// Design: cross-agent isolation is enforced by Unix permissions on
// /home/<agent> (mode 0750, owner = agent, others = none — provisioned by
// useradd and not loosened anywhere). One agent cannot read or write
// another agent's home regardless of systemd hardening, so layering
// ProtectHome=read-only on top of those permissions adds no security
// against the threat model and creates a steady stream of false positives:
//
//   - v0.8.3 (#109) added ReadWritePaths=~/.claude.json to fix the first
//     EROFS surfaced by Claude Code at startup.
//   - v0.9.1 (#133) added ~/.cache/claude, ~/.cache/claude-cli-nodejs,
//     ~/.local/share/claude, ~/.npm to fix self-update + OAuth refresh
//     EROFS spam in the runner log.
//   - v0.9.3 (this change) hits the next mole: Claude Code writes
//     ~/.claude.json atomically by creating ~/.claude.json.tmp and
//     renaming it, which requires write to the PARENT directory ($HOME
//     itself). ReadWritePaths grants write to specific inodes only, not
//     to their containing directory, so atomic-write tempfiles in
//     read-only $HOME always EROFS. The web terminal at port 7681
//     surfaces this as a bun openSync error from the cli entrypoint.
//
// Rather than continue adding paths every time Claude Code writes
// somewhere new, drop ProtectHome entirely. The agent retains full write
// access to its own $HOME (already restricted to itself by Unix perms)
// and is still blocked from /etc, /usr, and other system locations by
// ProtectSystem=strict. CLAUDE.md remains explicitly locked via
// ReadOnlyPaths so the operator's instructions cannot be silently
// rewritten by the agent.
func buildHardeningDirectives(homeDir, _ string) string {
	// The leading '-' on each ReadOnlyPaths entry tells systemd to ignore
	// the path if it does not exist. Without it, missing CLAUDE.md or
	// CLAUDE.local.md at $HOME root causes "Failed to set up mount
	// namespacing: No such file or directory" (status=226/NAMESPACE) and
	// the agent service refuses to start. Both files are operator-owned
	// and may legitimately be absent (Daisy keeps her CLAUDE.local.md in
	// the project subdir, not at $HOME root) — they should be locked
	// when present, not required.
	return fmt.Sprintf(
		"NoNewPrivileges=yes\nProtectSystem=strict\nPrivateTmp=yes\n"+
			"ReadOnlyPaths=-%s/CLAUDE.md -%s/CLAUDE.local.md\n",
		homeDir, homeDir,
	)
}

// GenerateService renders the systemd service unit content for an agent.
// Returns an error if the agent OS user does not exist on the host.
func GenerateService(cfg *config.Config, agentKey string) (string, error) {
	ac := cfg.Agents[agentKey]
	osUser := cfg.OSUsername(agentKey)
	homeDir, err := userHomeLookup(osUser)
	if err != nil {
		return "", fmt.Errorf("generating service for agent %s: %w", agentKey, err)
	}
	egress := ""
	if ac.EgressRestrictionExperimental {
		egress = egressDirectives
	}
	p := RunnerParams{
		Project:             cfg.Project,
		AgentKey:            agentKey,
		AgentName:           ac.Name,
		OSUser:              osUser,
		HomeDir:             homeDir,
		EgressDirectives:    egress,
		HardeningDirectives: buildHardeningDirectives(homeDir, cfg.Project),
	}
	return renderTemplate(serviceTemplate, p), nil
}

// GenerateTtydService renders the systemd service unit for the agent's web terminal.
func GenerateTtydService(cfg *config.Config, agentKey string) string {
	ac := cfg.Agents[agentKey]
	bind := ac.WebTerminalBind
	if bind == "" {
		bind = "127.0.0.1"
	}
	p := RunnerParams{
		Project:   cfg.Project,
		AgentKey:  agentKey,
		AgentName: ac.Name,
		OSUser:    cfg.OSUsername(agentKey),
		TtydPort:  ac.WebTerminalPort,
		TtydBind:  bind,
	}
	return renderTemplate(ttydServiceTemplate, p)
}

// renderTemplate does simple {{.Field}} substitution without importing text/template
// to keep the runner output readable and avoid escaping issues with bash.
func renderTemplate(tmpl string, p RunnerParams) string {
	r := strings.NewReplacer(
		"{{.Project}}", p.Project,
		"{{.AgentKey}}", p.AgentKey,
		"{{.AgentName}}", p.AgentName,
		"{{.Model}}", p.Model,
		"{{.Prompt}}", p.Prompt,
		"{{.OSUser}}", p.OSUser,
		"{{.HomeDir}}", p.HomeDir,
		"{{.SleepActive}}", fmt.Sprintf("%d", p.SleepActive),
		"{{.SleepNight}}", fmt.Sprintf("%d", p.SleepNight),
		"{{.TtydBind}}", p.TtydBind,
		"{{.TtydPort}}", fmt.Sprintf("%d", p.TtydPort),
		"{{.AlertChannel}}", p.AlertChannel,
		"{{.AlertCurl}}", p.AlertCurl,
		"{{.SubagentExport}}", p.SubagentExport,
		"{{.EgressDirectives}}", p.EgressDirectives,
		"{{.HardeningDirectives}}", p.HardeningDirectives,
		"{{.WatchChannelIDs}}", p.WatchChannelIDs,
	)
	return r.Replace(tmpl)
}

// discordWatchChannels returns a deterministic comma-separated list of
// configured Discord channel IDs for the gateway watcher to observe.
// Sorted by channel name (the map key) so renders are stable across
// Go map-iteration orderings, which keeps generated runner.sh diffs
// minimal between provisions.
func discordWatchChannels(cfg *config.Config) string {
	if cfg == nil || cfg.Coordination.Backend != "discord" {
		return ""
	}
	names := make([]string, 0, len(cfg.Coordination.Channels))
	for name := range cfg.Coordination.Channels {
		names = append(names, name)
	}
	sort.Strings(names)
	ids := make([]string, 0, len(names))
	for _, name := range names {
		if id := strings.TrimSpace(cfg.Coordination.Channels[name]); id != "" {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ",")
}
