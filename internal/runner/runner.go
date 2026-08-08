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

    # Guard: {{.InstructionFile}} too large (token waste)
    if [ -f "$WORKDIR/{{.InstructionFile}}" ]; then
        SIZE=$(stat -c %s "$WORKDIR/{{.InstructionFile}}" 2>/dev/null || echo 0)
        if (( SIZE > MAX_CLAUDE_MD_BYTES )); then
            log "WARNING: {{.InstructionFile}} is ${SIZE} bytes (max ${MAX_CLAUDE_MD_BYTES}) — alerting"
            source "$HOME/.env" 2>/dev/null
            {{.AlertCurl}}
        fi
    fi

    # claude install (bun fetch) can't traverse the egress proxy — it fails
    # "Socket is closed" every iteration on egress-contained hosts. Skip it
    # there; updates for contained agents happen at (re)provision time.
    if [ -n "$HTTPS_PROXY" ]; then
        log "Skipping claude update (egress proxy host)"
    else
        log "Updating claude"
        "$CLAUDE" install 2>&1 | tail -5 | tee -a "$LOGFILE" || log "claude install failed, continuing with current version"
    fi

    {{.SkillsSyncCmd}}

    # Surface a near-expired OAuth token to the agent itself: a dead token
    # mid-session shows up as opaque 401/407 API errors otherwise. Skipped when
    # a non-interactive credential (API key or setup-token) is configured —
    # there is no interactive .credentials.json to expire, so the check would
    # inject a false warning into every prompt.
    if [ -z "$ANTHROPIC_API_KEY" ] && [ -z "$CLAUDE_CODE_OAUTH_TOKEN" ]; then
        EXP_MS=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.claude/.credentials.json'))).get('claudeAiOauth',{}).get('expiresAt',0))" 2>/dev/null || echo 0)
        NOW_MS=$(( $(date +%s) * 1000 ))
        if [ "$EXP_MS" -gt 0 ] 2>/dev/null && [ "$EXP_MS" -lt $(( NOW_MS + 3600000 )) ]; then
            log "WARNING: OAuth token expires within 1h (or already expired)"
            RUNNER_WARNINGS="${RUNNER_WARNINGS}[runner] Your OAuth token expires within 1h or is already expired; if you hit 401/407 API errors, escalate to the alerts channel. "
        fi
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
    # to the harness-neutral ~/.clem/next-effort during an iteration; consumed
    # (and deleted) here. The legacy Claude path remains supported.
    # CLAUDE_CODE_EFFORT_LEVEL is session-scoped and outranks settings
    # files, so an absent file simply means settings.json effortLevel
    # applies — no reset bookkeeping, no drift across iterations.
    unset CLAUDE_CODE_EFFORT_LEVEL
    NEXT_EFFORT_FILE="$HOME/.clem/next-effort"
    [ -f "$NEXT_EFFORT_FILE" ] || NEXT_EFFORT_FILE="$HOME/.claude/next-effort"
    if [ -f "$NEXT_EFFORT_FILE" ]; then
        NEXT_EFFORT=$(tr -cd 'a-z' < "$NEXT_EFFORT_FILE" | head -c 16)
        rm -f "$NEXT_EFFORT_FILE"
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
    # Claude Code debounces large multi-line pastes: a single Enter sent right
    # after the prompt is swallowed as a soft newline, leaving the prompt typed
    # but never submitted (the agent looks "stuck" with its prompt in the box).
    # Retry the Enter a few times over a wider settle window — once the prompt
    # submits the input box is empty and any further Enter is a harmless no-op.
    (sleep 1 && tmux send-keys -t {{.AgentKey}} "" Enter
     sleep 25 && tmux send-keys -l -t {{.AgentKey}} "$PROMPT"
     for _ in 1 2 3 4 5; do sleep 3; tmux send-keys -t {{.AgentKey}} Enter; done) &
    # DISABLE_AUTOUPDATER is scoped to the interactive session only: Claude
    # Code's in-session updater can't reach downloads.claude.ai through the
    # brokered https:// proxy (curl can, its bun fetch closes the socket), so it
    # shows a persistent "Auto-update failed" banner. On non-proxy hosts updates
    # still happen via the explicit "claude install" above; proxy hosts skip it
    # (same bun fetch limitation) and update at provision time.
{{.HeadroomLaunch}}
    DISABLE_AUTOUPDATER=1 timeout 7200 $LAUNCH --dangerously-skip-permissions \
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

    if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 143 ] || [ $ELAPSED -gt $RESET_AFTER ]; then
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
# Merge Clem's runtime-neutral extension MCP manifest.
_managed_mcp = os.path.expanduser('~/.clem/mcp-servers.json')
if os.path.exists(_managed_mcp):
    with open(_managed_mcp) as f:
        for name, entry in json.load(f).items():
            if entry.get('type') == 'stdio':
                cfg['mcp'][name] = {
                    'type': 'local',
                    'command': [entry['command']] + entry.get('args', []),
                    'enabled': True,
                    'environment': entry.get('env', {}),
                }
            else:
                cfg['mcp'][name] = {
                    'type': 'remote',
                    'url': entry['url'],
                    'enabled': True,
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

    # Guard: {{.InstructionFile}} too large (token waste)
    if [ -f "$WORKDIR/{{.InstructionFile}}" ]; then
        SIZE=$(stat -c %s "$WORKDIR/{{.InstructionFile}}" 2>/dev/null || echo 0)
        if (( SIZE > MAX_CLAUDE_MD_BYTES )); then
            log "WARNING: {{.InstructionFile}} is ${SIZE} bytes (max ${MAX_CLAUDE_MD_BYTES}) — alerting"
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

    if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 143 ] || [ $ELAPSED -gt $RESET_AFTER ]; then
        BACKOFF=$SLEEP_BETWEEN
    else
        BACKOFF=$(( BACKOFF * 2 ))
        [ $BACKOFF -gt $MAX_BACKOFF ] && BACKOFF=$MAX_BACKOFF
    fi

    log "Sleeping ${BACKOFF}s"
    sleep $BACKOFF
done
`

// codexRunnerTemplate is the runner loop for agents using OpenAI's codex CLI.
// Codex speaks the OpenAI wire format natively (ChatGPT OAuth or OPENAI_API_KEY),
// so like opencode there is no Anthropic-format translator in the middle. It keeps
// the same interactive-TUI contract as claude-code: a long-lived TUI, tmux
// send-keys prompt injection, and the prompt ending in "kill $PPID" to advance
// the loop. MCP servers are configured via ~/.codex/config.toml (TOML, not JSON).
// Codex supports streamable-HTTP MCP, so privileged sidecars work here too.
const codexRunnerTemplate = `#!/bin/bash
set -m
BACKOFF=10
MAX_BACKOFF=900
RESET_AFTER=300
CODEX="$HOME/.npm-global/bin/codex"
LAUNCH=("$CODEX")
{{.CodexHeadroomLaunch}}
CODEX_UPDATER="$HOME/.local/bin/clem-codex-update"
WORKDIR="$HOME/{{.Project}}"
LOGFILE="$HOME/.claude/{{.AgentKey}}-runner.log"

mkdir -p "$HOME/.claude" "$HOME/.codex"
cd "$WORKDIR" || exit 1

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" | tee -a "$LOGFILE"; }
tail -500 "$LOGFILE" > "$LOGFILE.tmp" 2>/dev/null && mv "$LOGFILE.tmp" "$LOGFILE" 2>/dev/null

# Load secrets (written by clem provision, never committed) before the config
# writer reads them from the environment.
[ -f "$HOME/.env" ] && source "$HOME/.env"

# Write ~/.codex/config.toml from env. Top-level keys MUST precede any [table]
# header in TOML, so the auth/trust keys are emitted before the mcp_servers
# tables. Auth is forced to file-based storage because a systemd OS user has no
# unlocked OS keyring; forced_login_method=chatgpt matches the clem login flow.
python3 -c "
import os
_backend = '{{.CoordinationBackend}}'
def _mcp_bin(name):
    pipx = '/opt/pipx/bin/' + name
    sysbin = '/usr/local/bin/' + name
    return pipx if os.path.exists(pipx) else sysbin
def _s(v):
    return '\"' + str(v).replace('\\\\', '\\\\\\\\').replace('\"', '\\\\\"') + '\"'
lines = []
lines.append('cli_auth_credentials_store = \"file\"')
lines.append('mcp_oauth_credentials_store = \"file\"')
lines.append('forced_login_method = \"chatgpt\"')
{{.CodexEffortConfig}}
lines.append('check_for_update_on_startup = false')
lines.append('')
# Trust the work directory so project-scoped config layers load without a prompt.
lines.append('[projects.' + _s(os.path.expanduser('~/{{.Project}}')) + ']')
lines.append('trust_level = \"trusted\"')
lines.append('')
def _stdio(name, command, args=None, env=None):
    lines.append('[mcp_servers.' + name + ']')
    lines.append('command = ' + _s(command))
    if args:
        lines.append('args = [' + ', '.join(_s(a) for a in args) + ']')
    if env:
        lines.append('env = { ' + ', '.join(k + ' = ' + _s(v) for k, v in env.items()) + ' }')
    lines.append('')
def _http(name, url):
    lines.append('[mcp_servers.' + name + ']')
    lines.append('url = ' + _s(url))
    lines.append('')
# Discord bot (same gateway-watcher behaviour as the claude runner).
if _backend != 'github' and os.environ.get('DISCORD_TOKEN'):
    _denv = {'DISCORD_TOKEN': os.environ['DISCORD_TOKEN']}
    _watch = '{{.WatchChannelIDs}}'
    if _watch:
        _denv['DISCORD_WATCH_CHANNELS'] = _watch
        _denv['CLEM_TMUX_TARGET'] = '{{.AgentKey}}'
    _stdio('discord-bot', _mcp_bin('mcp-discord'), env=_denv)
# Slack (korotovsky/slack-mcp-server), write access on by default.
if _backend != 'github' and os.environ.get('SLACK_MCP_XOXP_TOKEN'):
    _sargs = ['--transport', 'stdio']
    if os.environ.get('SLACK_MCP_ENABLED_TOOLS'):
        _sargs += ['--enabled-tools', os.environ['SLACK_MCP_ENABLED_TOOLS']]
    _stdio('slack-mcp', _mcp_bin('slack-mcp-server'), args=_sargs, env={
        'SLACK_MCP_XOXP_TOKEN': os.environ['SLACK_MCP_XOXP_TOKEN'],
        'SLACK_MCP_ADD_MESSAGE_TOOL': os.environ.get('SLACK_MCP_ADD_MESSAGE_TOOL', 'true'),
    })
# Social media (Typefully backend — local MCP server).
if os.environ.get('TYPEFULLY_API_KEY'):
    _stdio('social', _mcp_bin('social-mcp'), env={'TYPEFULLY_API_KEY': os.environ['TYPEFULLY_API_KEY']})
# Privileged MCP sidecars over loopback streamable-HTTP (codex supports url MCP).
for _name, _port in {{.SidecarServers}}:
    _http(_name, 'http://127.0.0.1:%d/mcp' % _port)
# Merge Clem's runtime-neutral extension MCP manifest.
_managed_mcp = os.path.expanduser('~/.clem/mcp-servers.json')
if os.path.exists(_managed_mcp):
    import json
    with open(_managed_mcp) as f:
        for name, entry in json.load(f).items():
            if entry.get('type') == 'stdio':
                _stdio(name, entry['command'], entry.get('args'), entry.get('env'))
            else:
                _http(name, entry['url'])
open(os.path.expanduser('~/.codex/config.toml'), 'w').write('\n'.join(lines) + '\n')
"

SLEEP_ACTIVE={{.SleepActive}}
SLEEP_NIGHT={{.SleepNight}}
MAX_CLAUDE_MD_BYTES=12288

while true; do
    START=$(date +%s)
    PROMPT='{{.Prompt}}'
    RUNNER_WARNINGS=""

    # Guard: {{.InstructionFile}} too large (token waste)
    if [ -f "$WORKDIR/{{.InstructionFile}}" ]; then
        SIZE=$(stat -c %s "$WORKDIR/{{.InstructionFile}}" 2>/dev/null || echo 0)
        if (( SIZE > MAX_CLAUDE_MD_BYTES )); then
            log "WARNING: {{.InstructionFile}} is ${SIZE} bytes (max ${MAX_CLAUDE_MD_BYTES}) — alerting"
            source "$HOME/.env" 2>/dev/null
            {{.AlertCurl}}
        fi
    fi

    "$CODEX_UPDATER" update 2>&1 | tee -a "$LOGFILE" || log "codex update failed, continuing with current validated version"

    {{.SkillsSyncCmd}}

    [ -n "$RUNNER_WARNINGS" ] && PROMPT="${RUNNER_WARNINGS}${PROMPT}"

    # One-session reasoning override written by the shared effort-control skill.
    # Arrays preserve the TOML string as one -c argument.
    CODEX_EFFORT_ARGS=()
    NEXT_EFFORT_FILE="$HOME/.clem/next-effort"
    if [ -f "$NEXT_EFFORT_FILE" ]; then
        NEXT_EFFORT=$(tr -cd 'a-z' < "$NEXT_EFFORT_FILE" | head -c 16)
        rm -f "$NEXT_EFFORT_FILE"
        case "$NEXT_EFFORT" in
            low|medium|high|xhigh)
                CODEX_EFFORT_ARGS=(-c "model_reasoning_effort=\"$NEXT_EFFORT\"")
                log "Effort override for this session: $NEXT_EFFORT" ;;
            *)
                log "Ignoring invalid next-effort value: $NEXT_EFFORT" ;;
        esac
    fi

    log "Starting {{.AgentName}} (fresh session)"
    # Drive the CLI by pane state, not blind timing. New-version start screens,
    # update dialogs, and menus swallow blind keystrokes as navigation — the
    # session then sits stuck (settings screen open, or prompt typed but never
    # submitted). Read the pane, act on what is actually on screen, verify the
    # submission, then keep a watchdog for stuck states that appear mid-session.
    # All state checks anchor on the last 4 non-empty pane lines (the CLI footer)
    # so agent transcript text can't false-positive the markers.
    pane() { tmux capture-pane -p -t {{.AgentKey}} 2>/dev/null | grep -v "^ *$" | tail -4; }
    (
        # Phase 1: reach the composer. Menus dismiss with Escape; update/trust
        # dialogs advance with Enter; the composer shows a '›' input line.
        READY=""
        for _ in $(seq 45); do
            sleep 2
            if pane | grep -qi "esc to go back"; then
                tmux send-keys -t {{.AgentKey}} Escape
            elif pane | grep -q "›"; then
                READY=1; break
            else
                tmux send-keys -t {{.AgentKey}} "" Enter
            fi
        done
        # Phase 2: type the prompt and verify the CLI actually started working.
        # A literal '$' in shell instructions opens Codex's skill picker. Close
        # any mention picker before attempting submission; Escape is a no-op on
        # a composer that just holds text.
        SUBMITTED=""
        if [ -n "$READY" ]; then
            tmux send-keys -l -t {{.AgentKey}} "$PROMPT"
            sleep 2
            tmux send-keys -t {{.AgentKey}} Escape
            for _ in $(seq 10); do
                tmux send-keys -t {{.AgentKey}} Enter
                sleep 3
                if pane | grep -qE "esc to interrupt|Worked for"; then SUBMITTED=1; break; fi
                pane | grep -qi "esc to go back" && tmux send-keys -t {{.AgentKey}} Escape
            done
        fi
        if [ -z "$SUBMITTED" ]; then
            log "Prompt injection failed pane verification; recycling CLI session"
            pkill -TERM -f "$CODEX" 2>/dev/null
            exit 0
        fi
        log "Prompt submission verified on pane"
        # Phase 3: stuck-state watchdog. A menu screen, or sidecar-injected
        # composer text (starts with '['), parked across two consecutive idle
        # checks gets driven back to work instead of stalling the session.
        PREV=""
        while sleep 60; do
            if pane | grep -q "esc to interrupt"; then PREV=""; continue; fi
            # A dead auth banner means every turn fails until credentials are
            # repaired; recycling keeps retrying cheaply and picks up a fixed
            # login within minutes instead of after the 2h session timeout.
            if pane | grep -q "log out and sign in again"; then
                if [ "$PREV" = auth ]; then
                    log "Auth failure banner persists; recycling CLI session"
                    pkill -TERM -f "$CODEX" 2>/dev/null
                    exit 0
                fi
                PREV=auth; continue
            fi
            if pane | grep -qi "esc to go back"; then
                [ "$PREV" = menu ] && tmux send-keys -t {{.AgentKey}} Escape
                PREV=menu
            elif pane | grep -qE "^ *› *\["; then
                [ "$PREV" = text ] && tmux send-keys -t {{.AgentKey}} Enter
                PREV=text
            else
                PREV=""
            fi
        done
    ) &
    DRIVER_PID=$!
    MODEL_ARG=""
    [ -n '{{.Model}}' ] && MODEL_ARG="--model {{.Model}}"
    PROMOTED_VERSION=$(cat "$HOME/.npm-global/codex/state/promoted" 2>/dev/null || true)
    timeout 7200 "${LAUNCH[@]}" --dangerously-bypass-approvals-and-sandbox \
        $MODEL_ARG \
        "${CODEX_EFFORT_ARGS[@]}" \
        -C "$WORKDIR"

    EXIT_CODE=$?
    kill "$DRIVER_PID" 2>/dev/null
    ELAPSED=$(( $(date +%s) - START ))
    log "Exited $EXIT_CODE after ${ELAPSED}s"

    if [ $EXIT_CODE -ne 0 ] && [ $ELAPSED -lt 25 ] && [ -n "$PROMOTED_VERSION" ]; then
        log "Codex $PROMOTED_VERSION exited before prompt injection; rolling back and quarantining"
        "$CODEX_UPDATER" rollback-early "$PROMOTED_VERSION" 2>&1 | tee -a "$LOGFILE"
    elif [ $ELAPSED -ge 25 ] && [ -n "$PROMOTED_VERSION" ]; then
        "$CODEX_UPDATER" clear-promotion
    fi

    HOUR=$(date +%H)
    if [ "$HOUR" -ge 7 ] && [ "$HOUR" -lt 22 ]; then
        SLEEP_BETWEEN=$SLEEP_ACTIVE
    else
        SLEEP_BETWEEN=$SLEEP_NIGHT
    fi

    if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 143 ] || [ $ELAPSED -gt $RESET_AFTER ]; then
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
// firewall + agent-vault's TLS-MITM proxy. This systemd block is a cheap second
// kernel layer that blocks all direct internet egress even if the nftables
// ruleset is flushed. There are no hardcoded CIDRs to drift — the agent reaches
// the internet only via the loopback agent-vault proxy.
const egressDirectives = `# Egress containment (egress: enabled). Hard enforcement + domain allowlist
# live in the clem-nftables UID firewall and agent-vault's TLS-MITM proxy. This
# block is a second kernel layer blocking direct internet egress.
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
	CodexEffortConfig   string
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
	// ProxyUnitDeps is the After=/Wants= block tying the agent service to the
	// agent-vault + nftables units when egress containment is enabled.
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
	// InstructionFile is the per-agent instruction file the runtime reads from
	// the work dir (CLAUDE.local.md for claude-code, AGENTS.md for opencode/codex).
	// Used by the oversize guard so it checks the file the runtime actually reads.
	InstructionFile string
	// HeadroomLaunch is the bash snippet that sets $LAUNCH to either the
	// plain claude binary or a `headroom wrap claude` invocation when the
	// agent has headroom: true. claude-code template only.
	HeadroomLaunch string
	// CodexHeadroomLaunch optionally replaces the Codex launch array with a
	// Headroom wrapper. It is empty when Headroom is disabled.
	CodexHeadroomLaunch string
}

// headroomLaunchSnippet returns the bash block that sets $LAUNCH for the
// claude-code runner. With headroom enabled, the session is wrapped in
// `headroom wrap claude` — a local context-compression proxy that trims
// tokens before they reach the API, stretching subscription rate limits.
// The proxy port is picked fresh each iteration so multiple agents on one
// host never collide, and a missing binary falls back to a direct launch so
// a failed install never strands the agent. wrap resolves 'claude' via
// PATH, hence the export.
func headroomLaunchSnippet(enabled bool) string {
	if !enabled {
		return `    LAUNCH="$CLAUDE"`
	}
	return `    if [ -x "$HOME/.local/bin/headroom" ]; then
        export PATH="$HOME/.local/bin:$PATH"
        HEADROOM_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
        # wrap's 'claude mcp add' is a no-op when the server already exists,
        # leaving HEADROOM_PROXY_URL pinned to a dead port from a prior
        # iteration; remove it so wrap re-adds with this iteration's port.
        "$CLAUDE" mcp remove headroom >/dev/null 2>&1 || true
        LAUNCH="$HOME/.local/bin/headroom wrap claude -p $HEADROOM_PORT --no-context-tool --no-serena"
    else
        log "headroom enabled but not installed; launching claude directly"
        LAUNCH="$CLAUDE"
    fi`
}

func codexHeadroomLaunchSnippet(enabled bool) string {
	if !enabled {
		return ""
	}
	return `if [ -x "$HOME/.local/bin/headroom" ]; then
    export PATH="$HOME/.npm-global/bin:$HOME/.local/bin:$PATH"
    HEADROOM_PORT=$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
    LAUNCH=("$HOME/.local/bin/headroom" wrap codex -p "$HEADROOM_PORT" --code-memory none)
else
    log "headroom enabled but not installed; launching codex directly"
fi`
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

// coordinationReplayPrompt makes chat notifications a wake-up mechanism,
// never the delivery boundary. A notification can land while the model is
// finishing a turn or while the TUI is being recycled, so every fresh
// iteration must reconcile backend history before doing other work.
func coordinationReplayPrompt(cfg *config.Config) string {
	backend, err := coordination.Known(cfg.Coordination.Backend)
	if err != nil {
		return ""
	}

	switch backend.Name {
	case "discord":
		return "[clem coordination replay] Push notifications are wake-up hints only, not delivery or acknowledgement. Before other work, when a push notice arrives, and again before ending or recycling the iteration, call mcp__discord-bot__read_messages with limit 100 for each configured text channel. Continue with before_message_id when a page is full, and inspect relevant task thread history. Handle every trusted operator message without a later substantive response. Never require the operator to resend a message because it arrived during another turn."
	case "slack":
		return "[clem coordination replay] Polling is message delivery for Slack; terminal activity is not acknowledgement. Before other work, and again before ending or recycling the iteration, call mcp__slack-mcp__conversations_history with limit \"100\" for each configured channel. Follow next_cursor until history overlaps the previous sweep, and call mcp__slack-mcp__conversations_replies for relevant task threads. Handle every trusted operator message without a later substantive response. Never require the operator to resend a message because it arrived during another turn."
	default:
		return ""
	}
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
	if replayPrompt := coordinationReplayPrompt(cfg); replayPrompt != "" {
		promptText = strings.TrimRight(promptText, " \n") + "\n" + replayPrompt
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
	alertMsg := fmt.Sprintf(`⚠️ %s: %s is ${SIZE} bytes (>${MAX_CLAUDE_MD_BYTES}). Trim it to reduce token waste.`, escapeForAlert(ac.Name), ac.InstructionFileName())
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
		Project:           cfg.Project,
		AgentKey:          agentKey,
		AgentName:         ac.Name,
		Model:             ac.Model,
		CodexEffortConfig: codexEffortConfig(ac),
		SubagentExport:    subagentExport,
		Prompt:            strings.ReplaceAll(promptText, "'", `'\''`),
		OSUser:            cfg.OSUsername(agentKey),
		HomeDir:           fmt.Sprintf("/home/%s", cfg.OSUsername(agentKey)),
		SleepActive:       iterSec,
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
		SidecarServers:      sidecarServersLiteral(cfg, agentKey),
		SkillsSyncCmd:       skillsSyncCmd,
		InstructionFile:     ac.InstructionFileName(),
		HeadroomLaunch:      headroomLaunchSnippet(ac.Headroom),
		CodexHeadroomLaunch: codexHeadroomLaunchSnippet(ac.Headroom),
	}
	switch ac.RuntimeKind() {
	case "opencode":
		return renderTemplate(opencodeRunnerTemplate, p)
	case "codex":
		return renderTemplate(codexRunnerTemplate, p)
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
			"ReadOnlyPaths=-%s/CLAUDE.md -%s/CLAUDE.local.md -%s/AGENTS.md\n",
		homeDir, homeDir, homeDir,
	)
}

// proxyUnitDeps returns the [Unit] dependency block tying the agent service to
// the egress stack. The nftables firewall is a hard Requires= (fail-CLOSED: if
// the firewall fails to load, the agent must not start unconfined). agent-vault
// is a soft Wants= — losing it costs connectivity, not containment. After=
// orders the agent behind both so the boundary is up first.
func proxyUnitDeps(cfg *config.Config) string {
	return fmt.Sprintf("Requires=%s\nWants=%s\nAfter=%s %s\n",
		cfg.NftablesServiceName(), cfg.AgentVaultServiceName(),
		cfg.AgentVaultServiceName(), cfg.NftablesServiceName())
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
		"{{.CodexEffortConfig}}", p.CodexEffortConfig,
		"{{.Prompt}}", p.Prompt,
		"{{.HeadroomLaunch}}", p.HeadroomLaunch,
		"{{.CodexHeadroomLaunch}}", p.CodexHeadroomLaunch,
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
		"{{.ProxyUnitDeps}}", p.ProxyUnitDeps,
		"{{.SidecarServers}}", p.SidecarServers,
		"{{.CoordinationBackend}}", p.CoordinationBackend,
		"{{.GitHubWatchUnitDeps}}", p.GitHubWatchUnitDeps,
		"{{.SkillsSyncCmd}}", p.SkillsSyncCmd,
		"{{.InstructionFile}}", p.InstructionFile,
	)
	return r.Replace(tmpl)
}

// codexEffort translates Clem's harness-neutral effort vocabulary to Codex's.
// Claude Code accepts "max" while Codex calls the same top tier "xhigh".
func codexEffort(ac config.AgentConfig) string {
	if ac.RuntimeKind() != "codex" {
		return ""
	}
	if ac.Effort == "max" {
		return "xhigh"
	}
	return ac.Effort
}

func codexEffortConfig(ac config.AgentConfig) string {
	effort := codexEffort(ac)
	if effort == "" {
		return ""
	}
	return fmt.Sprintf(`lines.append('model_reasoning_effort = \"%s\"')`, effort)
}

// sidecarServersLiteral renders the Python list literal of [toolName, port]
// pairs for the sidecars this agent subscribes to — consumed by the .mcp.json
// generator in the runner template. "[]" when the agent subscribes to none.
func sidecarServersLiteral(cfg *config.Config, agentKey string) string {
	var parts []string
	for _, l := range cfg.SidecarListeners() {
		for _, ak := range l.Subscribers {
			if ak == agentKey {
				// Single-quote the tool name: this literal is interpolated into the
				// runner's `python3 -c "..."` block, which is itself double-quoted at
				// the shell level. %q's double quotes would close that shell string,
				// so the name reached python as a bare identifier (NameError, 0-byte
				// .mcp.json). ToolName is validated to validName (no quotes), so
				// single-quoting is safe.
				parts = append(parts, fmt.Sprintf("['%s', %d]", l.Server.ToolName(), l.Port))
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
