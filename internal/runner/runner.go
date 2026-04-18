package runner

import (
	"fmt"
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

# Load secrets (written by clem provision, never committed)
[ -f "$HOME/.env" ] && source "$HOME/.env"

# Write ephemeral .mcp.json from env (python3 ensures correct JSON encoding)
python3 -c "
import json, os
cfg = {'mcpServers': {}}
# Discord bot
if os.environ.get('DISCORD_TOKEN'):
    cfg['mcpServers']['discord-bot'] = {'command': '/usr/local/bin/mcp-discord', 'env': {'DISCORD_TOKEN': os.environ['DISCORD_TOKEN']}}
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
    cfg['mcp']['discord-bot'] = {
        'type': 'local',
        'command': ['/usr/local/bin/mcp-discord'],
        'enabled': True,
        'environment': {'DISCORD_TOKEN': os.environ['DISCORD_TOKEN']},
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

[Service]
Type=forking
User={{.OSUser}}
ExecStart=/usr/bin/tmux new-session -d -s {{.AgentKey}} {{.HomeDir}}/.local/bin/clem-runner.sh
ExecStop=/usr/bin/tmux kill-session -t {{.AgentKey}}
RemainAfterExit=yes
Restart=no

[Install]
WantedBy=multi-user.target
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
	Project      string
	AgentKey     string
	AgentName    string
	Model        string
	Prompt       string
	OSUser       string
	HomeDir      string
	SleepActive  int
	SleepNight   int
	TtydPort     int
	TtydBind     string
	AlertChannel string
	AlertCurl    string
}

// Generate renders the runner.sh content for an agent. Dispatches on the
// agent's runtime (claude-code default, or opencode).
func Generate(cfg *config.Config, agentKey string) string {
	ac := cfg.Agents[agentKey]
	iterDur, _ := ac.IterationDuration() // validated at load time
	iterSec := int(iterDur.Seconds())

	promptText := ac.Prompt
	if ac.Caveman {
		promptText = "/caveman ultra\n" + promptText
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

	p := RunnerParams{
		Project:      cfg.Project,
		AgentKey:     agentKey,
		AgentName:    ac.Name,
		Model:        ac.Model,
		Prompt:       strings.ReplaceAll(promptText, "'", `'\''`),
		OSUser:       cfg.OSUsername(agentKey),
		HomeDir:      fmt.Sprintf("/home/%s", cfg.OSUsername(agentKey)),
		SleepActive:  iterSec,
		SleepNight:   iterSec * 2,
		AlertChannel: alertChannel,
		AlertCurl:    alertCurl,
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
	p := RunnerParams{
		Project:   cfg.Project,
		AgentKey:  agentKey,
		AgentName: ac.Name,
		OSUser:    cfg.OSUsername(agentKey),
		HomeDir:   fmt.Sprintf("/home/%s", cfg.OSUsername(agentKey)),
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
	)
	return r.Replace(tmpl)
}
