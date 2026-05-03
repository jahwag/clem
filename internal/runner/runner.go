package runner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jahwag/clem/internal/config"
	"github.com/jahwag/clem/internal/coordination"
)

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
# Write ephemeral .mcp.json from env (python3 ensures correct JSON encoding)
python3 -c "
import json, os
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
    cfg['mcpServers']['discord-bot'] = {'command': '/usr/local/bin/mcp-discord', 'env': _discord_env}
# Slack (korotovsky/slack-mcp-server). Read access is free; write access
# (conversations_add_message) requires SLACK_MCP_ADD_MESSAGE_TOOL — enabled
# here by default so agents can actually post, matching the Discord default.
#
# SLACK_MCP_ENABLED_TOOLS is optional: comma-separated list to restrict the
# exposed toolset. Useful for small local models (e.g. Nemotron 4B) that get
# confused by the full 13-tool surface. Leave unset on cloud Claude / Opus.
if os.environ.get('SLACK_MCP_XOXP_TOKEN'):
    slack_args = ['--transport', 'stdio']
    if os.environ.get('SLACK_MCP_ENABLED_TOOLS'):
        slack_args += ['--enabled-tools', os.environ['SLACK_MCP_ENABLED_TOOLS']]
    cfg['mcpServers']['slack-mcp'] = {
        'command': '/usr/local/bin/slack-mcp-server',
        'args': slack_args,
        'env': {
            'SLACK_MCP_XOXP_TOKEN': os.environ['SLACK_MCP_XOXP_TOKEN'],
            'SLACK_MCP_ADD_MESSAGE_TOOL': os.environ.get('SLACK_MCP_ADD_MESSAGE_TOOL', 'true'),
        },
    }
# Prefect MCP (needs SSH_HOST + ES_PASSWORD)
if os.environ.get('SSH_HOST') and os.environ.get('ES_PASSWORD'):
    cfg['mcpServers']['prefect'] = {
        'command': '/usr/local/bin/prefect-mcp',
        'env': {
            'SSH_HOST': os.environ['SSH_HOST'],
            'SSH_USER': os.environ.get('SSH_USER', 'ubuntu'),
            'SSH_KEY_PATH': os.path.expanduser('~/.ssh/id_ed25519'),
            'PREFECT_API_PORT': os.environ.get('PREFECT_API_PORT', '4200'),
            'ES_USER': os.environ.get('ES_USER', 'elastic'),
            'ES_PASSWORD': os.environ['ES_PASSWORD'],
        }
    }
# GitHub MCP (needs GH_TOKEN)
if os.environ.get('GH_TOKEN'):
    cfg['mcpServers']['github'] = {
        'command': '/usr/local/bin/github-mcp-server',
        'args': ['stdio'],
        'env': {'GITHUB_PERSONAL_ACCESS_TOKEN': os.environ['GH_TOKEN']}
    }
# Context7 (library docs lookup — no auth required)
cfg['mcpServers']['context7'] = {
    'command': 'npx',
    'args': ['-y', '@upstash/context7-mcp']
}
# Social media (Typefully backend — local MCP server)
if os.environ.get('TYPEFULLY_API_KEY'):
    cfg['mcpServers']['social'] = {
        'command': '/usr/local/bin/social-mcp',
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
python3 -c "
import json, os
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
        'command': ['/usr/local/bin/mcp-discord'],
        'enabled': True,
        'environment': _discord_env,
    }
if os.environ.get('SLACK_MCP_XOXP_TOKEN'):
    slack_cmd = ['/usr/local/bin/slack-mcp-server', '--transport', 'stdio']
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

while true; do
    START=$(date +%s)
    PROMPT='{{.Prompt}}'

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
{{.EgressDirectives}}
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

[Service]
Type=simple
User={{.OSUser}}
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
	EgressDirectives  string
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

	promptText := ac.Prompt
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
		SleepNight:      iterSec * 2,
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

// GenerateService renders the systemd service unit content for an agent.
func GenerateService(cfg *config.Config, agentKey string) string {
	ac := cfg.Agents[agentKey]
	egress := ""
	if ac.EgressRestrictionExperimental {
		egress = egressDirectives
	}
	p := RunnerParams{
		Project:          cfg.Project,
		AgentKey:         agentKey,
		AgentName:        ac.Name,
		OSUser:           cfg.OSUsername(agentKey),
		HomeDir:          fmt.Sprintf("/home/%s", cfg.OSUsername(agentKey)),
		EgressDirectives: egress,
	}
	return renderTemplate(serviceTemplate, p)
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
