package runner

import (
	"encoding/json"
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
{{.ProxyExport}}
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
_backend = '{{.CoordinationBackend}}'
def _mcp_bin(name):
    pipx = '/opt/pipx/bin/' + name
    sysbin = '/usr/local/bin/' + name
    return pipx if os.path.exists(pipx) else sysbin
cfg = {'mcpServers': {}}
# Discord bot. When channel IDs are configured the MCP server also runs a
# gateway watcher that pushes one debounced notification per burst into this
# agent's tmux session — see mcp-discord's CLEM_TMUX_TARGET docs.
# Skipped when coordination backend is github (agents use gh CLI instead).
if _backend != 'github' and os.environ.get('DISCORD_TOKEN'):
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
if _backend != 'github' and os.environ.get('SLACK_MCP_XOXP_TOKEN'):
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
# The Prefect MCP (SSH_HOST/SSH_KEY/ES_PASSWORD) was removed: SSH-based MCPs
# cannot be brokered by agent-vault (SSH is not HTTP) and are dropped under the
# credential-proxy model. Re-add it in a project .mcp.json if a host still needs
# it, with the understanding that its secrets stay in plaintext .env.
# GitHub MCP and context7 are NOT registered here by default — agents use
# the gh CLI directly (more context-efficient per Anthropic's cost docs) and
# can opt in to context7 per-project by checking a .mcp.json into the workdir.
# Social media (Typefully backend — local MCP server)
if os.environ.get('TYPEFULLY_API_KEY'):
    cfg['mcpServers']['social'] = {
        'command': _mcp_bin('social-mcp'),
        'env': {'TYPEFULLY_API_KEY': os.environ['TYPEFULLY_API_KEY']}
    }
# Privileged MCP sidecars: reached over loopback streamable-HTTP (never stdio),
# so the upstream secret stays in the separate-UID mcp-proxy process, never here.
# A kernel nftables rule restricts each port to this agent's UID.
for _name, _port in {{.SidecarServers}}:
    cfg['mcpServers'][_name] = {'type': 'http', 'url': 'http://127.0.0.1:%d/mcp' % _port}
print(json.dumps(cfg, indent=2))
" > "$WORKDIR/.mcp.json"

SLEEP_ACTIVE={{.SleepActive}}
SLEEP_NIGHT={{.SleepNight}}
MAX_CLAUDE_MD_BYTES=12288
MAX_LESSONS_MESSAGES=25

while true; do
    START=$(date +%s)
    PROMPT='{{.Prompt}}'
    RUNNER_WARNINGS=""

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

    {{.SkillsSyncCmd}}

    # Surface a near-expired OAuth token to the agent itself: a dead token
    # mid-session shows up as opaque 401/407 API errors otherwise.
    EXP_MS=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.claude/.credentials.json'))).get('claudeAiOauth',{}).get('expiresAt',0))" 2>/dev/null || echo 0)
    NOW_MS=$(( $(date +%s) * 1000 ))
    if [ "$EXP_MS" -gt 0 ] 2>/dev/null && [ "$EXP_MS" -lt $(( NOW_MS + 3600000 )) ]; then
        log "WARNING: OAuth token expires within 1h (or already expired)"
        RUNNER_WARNINGS="${RUNNER_WARNINGS}[runner] Your OAuth token expires within 1h or is already expired; if you hit 401/407 API errors, escalate to the alerts channel. "
    fi

    # Per-agent quota snapshot (TTL 25m). Agents read this file instead of
    # polling the OAuth usage endpoint every iteration, which rate-limits
    # (429) when multiple agents on one host poll per-session.
    QUOTA_FILE="$HOME/.claude/quota.json"
    QUOTA_AGE=$(( $(date +%s) - $(stat -c %Y "$QUOTA_FILE" 2>/dev/null || echo 0) ))
    if [ "$QUOTA_AGE" -gt 1500 ]; then
        OAUTH_TOKEN=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.claude/.credentials.json')))['claudeAiOauth']['accessToken'])" 2>/dev/null)
        if [ -n "$OAUTH_TOKEN" ]; then
            HTTP_CODE=$(curl -sS -m 15 -o "$QUOTA_FILE.tmp" -w "%{http_code}" \
                -H "Authorization: Bearer $OAUTH_TOKEN" \
                -H "anthropic-beta: oauth-2025-04-20" \
                https://api.anthropic.com/api/oauth/usage 2>>"$LOGFILE")
            if [ "$HTTP_CODE" = "200" ]; then
                mv "$QUOTA_FILE.tmp" "$QUOTA_FILE"
                log "Quota snapshot refreshed"
            else
                rm -f "$QUOTA_FILE.tmp"
                log "Quota snapshot fetch failed (HTTP ${HTTP_CODE}); keeping previous snapshot"
            fi
            unset OAUTH_TOKEN
        fi
    fi

    # Per-iteration effort override: the agent writes low|medium|high|xhigh
    # to ~/.claude/next-effort during an iteration; consumed (and deleted)
    # here. CLAUDE_CODE_EFFORT_LEVEL is session-scoped and outranks settings
    # files, so an absent file simply means settings.json effortLevel
    # applies — no reset bookkeeping, no drift across iterations.
    unset CLAUDE_CODE_EFFORT_LEVEL
    if [ -f "$HOME/.claude/next-effort" ]; then
        NEXT_EFFORT=$(tr -cd 'a-z' < "$HOME/.claude/next-effort" | head -c 16)
        rm -f "$HOME/.claude/next-effort"
        case "$NEXT_EFFORT" in
            low|medium|high|xhigh)
                export CLAUDE_CODE_EFFORT_LEVEL="$NEXT_EFFORT"
                log "Effort override for this session: $NEXT_EFFORT" ;;
            *)
                log "Ignoring invalid next-effort value: $NEXT_EFFORT" ;;
        esac
    fi

    [ -n "$RUNNER_WARNINGS" ] && PROMPT="${RUNNER_WARNINGS}${PROMPT}"

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
{{.ProxyExport}}
[ -f "$HOME/.env" ] && source "$HOME/.env"
{{.SubagentExport}}
# Write opencode.json with Ollama provider + discord-bot MCP (if token is set).
# MCP binary paths come from _mcp_bin (pipx-isolated venv preferred over system
# pip install — see the claude-code runner template above for the rationale).
python3 -c "
import json, os
_backend = '{{.CoordinationBackend}}'
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
if _backend != 'github' and os.environ.get('DISCORD_TOKEN'):
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
if _backend != 'github' and os.environ.get('SLACK_MCP_XOXP_TOKEN'):
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
    RUNNER_WARNINGS=""

    # Guard: CLAUDE.local.md too large (token waste)
    if [ -f "$WORKDIR/CLAUDE.local.md" ]; then
        SIZE=$(stat -c %s "$WORKDIR/CLAUDE.local.md" 2>/dev/null || echo 0)
        if (( SIZE > MAX_CLAUDE_MD_BYTES )); then
            log "WARNING: CLAUDE.local.md is ${SIZE} bytes (max ${MAX_CLAUDE_MD_BYTES}) — alerting"
            source "$HOME/.env" 2>/dev/null
            {{.AlertCurl}}
        fi
    fi

    {{.SkillsSyncCmd}}

    [ -n "$RUNNER_WARNINGS" ] && PROMPT="${RUNNER_WARNINGS}${PROMPT}"

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
{{.GitHubWatchUnitDeps}}
{{.ProxyUnitDeps}}
[Service]
Type=forking
User={{.OSUser}}
ExecStart=/usr/bin/tmux new-session -d -s {{.AgentKey}} {{.HomeDir}}/.local/bin/clem-runner.sh
ExecStop=/usr/bin/tmux kill-session -t {{.AgentKey}}
RemainAfterExit=yes
Restart=no
{{.HardeningDirectives}}{{.EgressDirectives}}{{.ResourceDirectives}}
[Install]
WantedBy=multi-user.target
`

// egressDirectives is the systemd IP-firewall block injected when egress
// containment is enabled for an agent. It is intentionally loopback-only:
// hard enforcement (and the domain allowlist) lives in the clem-nftables UID
// firewall + pipelock proxy. This systemd block is a cheap second kernel layer
// that blocks all direct internet egress even if the nftables ruleset is
// flushed. There are no hardcoded CIDRs to drift — the agent reaches the
// internet only via the loopback pipelock proxy.
const egressDirectives = `# Egress containment (egress: enabled). Hard enforcement + domain allowlist
# live in the clem-nftables UID firewall and the pipelock proxy. This block is
# a second kernel layer blocking direct internet egress.
IPAddressDeny=any
IPAddressAllow=127.0.0.0/8
IPAddressAllow=::1/128
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
	Project             string
	AgentKey            string
	AgentName           string
	Model               string
	SubagentExport      string
	Prompt              string
	OSUser              string
	HomeDir             string
	SleepActive         int
	SleepNight          int
	TtydPort            int
	TtydBind            string
	AlertChannel        string
	AlertCurl           string
	EgressDirectives    string
	HardeningDirectives string
	ResourceDirectives  string
	// ProxyExport is the HTTPS_PROXY/NO_PROXY export block injected into the
	// runner when egress containment is enabled for the agent. Empty otherwise.
	ProxyExport string
	// ProxyUnitDeps is the After=/Wants= block tying the agent service to the
	// pipelock + nftables units when egress containment is enabled.
	ProxyUnitDeps string
	// WatchChannelIDs is the comma-separated list of Discord channel IDs the
	// MCP server's gateway watcher should observe. Empty disables the watcher
	// even when DISCORD_TOKEN is set, preserving the original tool-only mode.
	WatchChannelIDs string
	// SidecarServers is a Python/JSON list literal of [toolName, port] pairs for
	// the privileged MCP sidecars this agent subscribes to. "[]" when none.
	SidecarServers string
	// CoordinationBackend is the coordination.backend value (discord, slack, github).
	CoordinationBackend string
	// GitHubWatchUnitDeps is the Wants= block tying the agent to the GitHub
	// issue watcher sidecar when coordination.backend is github.
	GitHubWatchUnitDeps string
	// SkillsSyncCmd is the shell snippet invoked at the top of every iteration
	// to refresh the agent's ~/.claude/skills/ symlinks from the team skills
	// repo. Empty when cfg.SkillsRepo is unset, in which case no sync runs.
	SkillsSyncCmd string
}

// bashDoubleQuoteEscaper escapes the four characters that stay live inside a
// bash double-quoted string.
var bashDoubleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// escapeForAlert escapes a value for the AlertCurl sink, where it sits inside
// a JSON string that is itself inside a bash double-quoted argument
// (AlertTemplate's -d "{\"content\":\"%s\"}"). Bash dequotes the argument
// first, then the chat API parses the JSON, so the value needs both layers of
// escaping applied innermost-first: JSON string escaping (via json.Marshal,
// which also covers control characters), then bash's \ " $ `. The static
// parts of the alert message rely on that bash layer (${SIZE} expands at
// runtime), which is why the sink cannot simply switch to single quotes.
func escapeForAlert(s string) string {
	j, _ := json.Marshal(s) // marshaling a string never errors
	return bashDoubleQuoteEscaper.Replace(string(j[1 : len(j)-1]))
}

// Generate renders the runner.sh content for an agent. Dispatches on the
// agent's runtime (claude-code default, or opencode).
func Generate(cfg *config.Config, agentKey string) string {
	ac := cfg.Agents[agentKey]
	iterDur, _ := ac.IterationDuration() // validated at load time
	iterSec := int(iterDur.Seconds())
	nightDur, _ := ac.IterationNightDuration() // validated at load time
	nightSec := int(nightDur.Seconds())

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
	alertMsg := fmt.Sprintf(`⚠️ %s: CLAUDE.local.md is ${SIZE} bytes (>${MAX_CLAUDE_MD_BYTES}). Trim it to reduce token waste.`, escapeForAlert(ac.Name))
	alertCurlBody := coordination.RenderAlert(backend, coordination.AlertParams{
		Repo:    cfg.Coordination.GithubRepo,
		Channel: alertChannel,
		Message: alertMsg,
	})
	alertCurl := coordination.AlertCurlGuard(backend, alertChannel, alertCurlBody)

	subagentExport := ""
	if ac.SubagentModel != "" {
		subagentExport = fmt.Sprintf("export CLAUDE_CODE_SUBAGENT_MODEL=%q", ac.SubagentModel)
	}
	skillsSyncCmd := ""
	if cfg.SkillsRepo != "" {
		// PIPESTATUS, not ||: without pipefail, `sync | tee || log` reacts to
		// tee's exit status, so sync failures were silently swallowed (bit a
		// production host for 3 weeks: a dirty clone blocked every pull and
		// the "skills sync failed" branch never fired). The warning is also
		// prepended to the agent's prompt so the agent itself escalates.
		skillsSyncCmd = fmt.Sprintf(
			`clem sync-skills --home "$HOME" --agent-key %q --repo %q 2>&1 | tee -a "$LOGFILE"
    if [ "${PIPESTATUS[0]}" != "0" ]; then
        log "skills sync failed"
        RUNNER_WARNINGS="${RUNNER_WARNINGS}[runner] Skills sync FAILED this iteration; your skills may be stale. Check ~/.cache for a dirty clone or auth failure, fix or escalate to the alerts channel. "
    fi`,
			agentKey, cfg.SkillsRepo,
		)
	}
	p := RunnerParams{
		Project:        cfg.Project,
		AgentKey:       agentKey,
		AgentName:      ac.Name,
		Model:          ac.Model,
		SubagentExport: subagentExport,
		Prompt:         strings.ReplaceAll(promptText, "'", `'\''`),
		OSUser:         cfg.OSUsername(agentKey),
		HomeDir:        fmt.Sprintf("/home/%s", cfg.OSUsername(agentKey)),
		SleepActive: iterSec,
		// Night sleep defaults to the active value; iteration_night overrides.
		// History: a hardcoded 2x night doubler was removed on the belief the
		// prompt-cache TTL was 5 min. Subscription Claude Code actually gets
		// the 1h TTL, refreshed on access (verified against session-log usage
		// fields, 2026-06-13), so night intervals up to ~45m still start warm.
		SleepNight:          nightSec,
		AlertChannel:        alertChannel,
		AlertCurl:           alertCurl,
		WatchChannelIDs:     watchChannelIDs(cfg),
		CoordinationBackend: cfg.Coordination.BackendOrDefault(),
		GitHubWatchUnitDeps: githubWatchUnitDeps(cfg, agentKey),
		ProxyExport:         proxyExportBlock(cfg, agentKey),
		SidecarServers:      sidecarServersLiteral(cfg, agentKey),
		SkillsSyncCmd:       skillsSyncCmd,
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

// proxyExportBlock returns the HTTPS_PROXY/NO_PROXY export injected into the
// runner when egress containment is enabled for the agent. Exported before
// sourcing $HOME/.env so an operator can still override per-host. Empty when
// containment is disabled. NO_PROXY keeps loopback (Ollama, MCP sockets) direct.
func proxyExportBlock(cfg *config.Config, agentKey string) string {
	if !cfg.EgressEnabledFor(agentKey) {
		return ""
	}
	port := cfg.Egress.ProxyPortOrDefault()
	return fmt.Sprintf(`# Egress containment: route HTTP(S) through the pipelock proxy. The nftables
# UID firewall blocks all other egress, so this loopback proxy is the only way
# out. NO_PROXY keeps loopback (Ollama, MCP sockets) direct.
export HTTPS_PROXY=http://127.0.0.1:%d
export HTTP_PROXY=http://127.0.0.1:%d
export NO_PROXY=127.0.0.1,localhost,::1`, port, port)
}

// proxyUnitDeps returns the [Unit] dependency block tying the agent service to
// the egress stack. The nftables firewall is a hard Requires= (fail-CLOSED: if
// the firewall fails to load, the agent must not start unconfined). The
// pipelock proxy is a soft Wants= — losing it costs connectivity, not
// containment. After= orders the agent behind both so the boundary is up first.
func proxyUnitDeps(cfg *config.Config) string {
	return fmt.Sprintf("Requires=%s\nWants=%s\nAfter=%s %s\n",
		cfg.NftablesServiceName(), cfg.PipelockServiceName(),
		cfg.PipelockServiceName(), cfg.NftablesServiceName())
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
	proxyDeps := ""
	if cfg.EgressEnabledFor(agentKey) {
		egress = egressDirectives
		proxyDeps = proxyUnitDeps(cfg)
	}
	p := RunnerParams{
		Project:             cfg.Project,
		AgentKey:            agentKey,
		AgentName:           ac.Name,
		OSUser:              osUser,
		HomeDir:             homeDir,
		EgressDirectives:    egress,
		HardeningDirectives: buildHardeningDirectives(homeDir, cfg.Project),
		ResourceDirectives:  ac.ResourceLimits.Directives(),
		ProxyUnitDeps:       proxyDeps,
		GitHubWatchUnitDeps: githubWatchUnitDeps(cfg, agentKey),
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
		"{{.ResourceDirectives}}", p.ResourceDirectives,
		"{{.WatchChannelIDs}}", p.WatchChannelIDs,
		"{{.ProxyExport}}", p.ProxyExport,
		"{{.ProxyUnitDeps}}", p.ProxyUnitDeps,
		"{{.SidecarServers}}", p.SidecarServers,
		"{{.CoordinationBackend}}", p.CoordinationBackend,
		"{{.GitHubWatchUnitDeps}}", p.GitHubWatchUnitDeps,
		"{{.SkillsSyncCmd}}", p.SkillsSyncCmd,
	)
	return r.Replace(tmpl)
}

// sidecarServersLiteral renders the Python list literal of [toolName, port]
// pairs for the sidecars this agent subscribes to — consumed by the .mcp.json
// generator in the runner template. "[]" when the agent subscribes to none.
func sidecarServersLiteral(cfg *config.Config, agentKey string) string {
	var parts []string
	for _, l := range cfg.SidecarListeners() {
		for _, ak := range l.Subscribers {
			if ak == agentKey {
				parts = append(parts, fmt.Sprintf("[%q, %d]", l.Server.ToolName(), l.Port))
				break
			}
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// watchChannelIDs returns coordination-backend-specific watcher configuration
// injected into the MCP generator. Discord: comma-separated channel IDs for
// mcp-discord's gateway watcher. GitHub and Slack: empty (GitHub uses a
// separate systemd sidecar; Slack has no push watcher yet).
func watchChannelIDs(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	// Compare the resolved backend name, not the raw field: an omitted
	// backend value defaults to discord (#153).
	backend, _ := coordination.Known(cfg.Coordination.Backend) // validated at load time
	if backend.Name != "discord" {
		return ""
	}
	return sortedChannelIDs(cfg.Coordination.Channels)
}

// sortedChannelIDs returns a deterministic comma-separated list of configured
// channel IDs. Sorted by channel name so renders are stable across Go map
// iteration orderings.
func sortedChannelIDs(channels map[string]string) string {
	names := make([]string, 0, len(channels))
	for name := range channels {
		names = append(names, name)
	}
	sort.Strings(names)
	ids := make([]string, 0, len(names))
	for _, name := range names {
		if id := strings.TrimSpace(channels[name]); id != "" {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ",")
}

func githubWatchUnitDeps(cfg *config.Config, agentKey string) string {
	if cfg == nil || !cfg.UsesGitHubCoordination() {
		return ""
	}
	return fmt.Sprintf("Wants=%s\n", cfg.GitHubWatchServiceName(agentKey))
}
