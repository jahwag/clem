package watchdog

import (
	"strings"
	"testing"

	"github.com/jahwag/clem/internal/config"
)

func baseCfg() *config.Config {
	return &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend: "discord",
			Channels: map[string]string{
				"alerts":  "111",
				"tasks":   "222",
				"general": "333",
			},
		},
		Agents: map[string]config.AgentConfig{
			"lead": {Name: "Lead", Model: "claude-opus-4-7", Iteration: "1m", Prompt: "x"},
		},
	}
}

func TestGenerateScript_PostRestartRecheckSuppressesAlert(t *testing.T) {
	s := GenerateScript(baseCfg())
	for _, want := range []string{
		`systemctl restart "$service"`,
		`post_state=$(systemctl show -p ActiveState --value "$service" 2>/dev/null)`,
		`tmux has-session -t "$agent_key"`,
		`if [ "$post_state" = "active" ] && [ "$post_tmux" = "yes" ]; then`,
		`echo 0 > "$fail_count_file"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated script missing post-restart re-check line: %q\n---\n%s", want, s)
		}
	}

	// Alert message must include both post-restart signals so on-call can tell
	// whether systemd was still failed or tmux never came back.
	if !strings.Contains(s, `(systemd=$post_state tmux=$post_tmux)`) {
		t.Errorf("alert should report post_state + post_tmux, got:\n%s", s)
	}

	// Pre-fix behaviour: counter incremented before any post-restart check.
	// Guard against regression by requiring that the increment only appears
	// AFTER the post_state check returns.
	preCheck := strings.Index(s, `post_state=$(systemctl show`)
	inc := strings.Index(s, `fails=$(( $(cat "$fail_count_file"`)
	if preCheck == -1 || inc == -1 || inc < preCheck {
		t.Errorf("fail-count increment must follow post_state check (preCheck=%d inc=%d)", preCheck, inc)
	}
}

func TestGenerateScript_DiscordBackendAlertCurl(t *testing.T) {
	s := GenerateScript(baseCfg())
	for _, want := range []string{
		`if [ -n "$DISCORD_TOKEN" ] && [ -n "111" ]; then`,
		`https://discord.com/api/v10/channels/111/messages`,
		`-H "Authorization: Bot $DISCORD_TOKEN"`,
		`-d "{\"content\":\"$safe_msg\"}"`,
		`safe_msg=$(python3 -c "import json,sys; print(json.dumps(sys.argv[1])[1:-1])" "$msg" 2>/dev/null) || safe_msg=$msg`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("discord-backend script missing %q\n---\n%s", want, s)
		}
	}
	// Slack-only patterns must not leak into the Discord script.
	for _, deny := range []string{
		`SLACK_MCP_XOXP_TOKEN`,
		`slack.com/api/chat.postMessage`,
	} {
		if strings.Contains(s, deny) {
			t.Errorf("discord-backend script must not contain %q\n---\n%s", deny, s)
		}
	}
}

func TestGenerateScript_SlackBackendAlertCurl(t *testing.T) {
	cfg := baseCfg()
	cfg.Coordination.Backend = "slack"
	s := GenerateScript(cfg)
	for _, want := range []string{
		`if [ -n "$SLACK_MCP_XOXP_TOKEN" ] && [ -n "111" ]; then`,
		`https://slack.com/api/chat.postMessage`,
		`-H "Authorization: Bearer $SLACK_MCP_XOXP_TOKEN"`,
		`-d "{\"channel\":\"111\",\"text\":\"$safe_msg\"}"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("slack-backend script missing %q\n---\n%s", want, s)
		}
	}
	// Discord-only patterns must not leak into the Slack script.
	for _, deny := range []string{
		`DISCORD_TOKEN`,
		`discord.com/api/v10`,
	} {
		if strings.Contains(s, deny) {
			t.Errorf("slack-backend script must not contain %q\n---\n%s", deny, s)
		}
	}
}

func TestGenerateScript_StaleThresholdPerAgent(t *testing.T) {
	cfg := &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend: "discord",
			Channels: map[string]string{
				"alerts":  "111",
				"tasks":   "222",
				"general": "333",
			},
		},
		Agents: map[string]config.AgentConfig{
			"lead":     {Name: "Lead", Model: "claude-opus-4-7", Iteration: "45m", Prompt: "x"},
			"follower": {Name: "Follower", Model: "claude-opus-4-7", Iteration: "5m", Prompt: "y"},
		},
	}
	s := GenerateScript(cfg)

	// 45m iteration → 2700 + 300 margin = 3000
	wantLead := `check_agent "lead" "test-lead" "clem-test-lead.service" "3000"`
	if !strings.Contains(s, wantLead) {
		t.Errorf("expected lead invocation with 3000s stale threshold, missing %q\n---\n%s", wantLead, s)
	}
	// 5m iteration → 300 + 300 = 600, floored to 1800: the runner log is
	// silent for the whole claude session, so short-iteration agents keep
	// the historical 30-minute grace instead of being killed mid-task.
	wantFollower := `check_agent "follower" "test-follower" "clem-test-follower.service" "1800"`
	if !strings.Contains(s, wantFollower) {
		t.Errorf("expected follower invocation floored at 1800s stale threshold, missing %q\n---\n%s", wantFollower, s)
	}

	// The hardcoded 30-minute threshold must be gone — the only valid path is
	// the per-agent variable. A literal `> 1800` in the script would mean we
	// regressed back to the old behavior.
	if strings.Contains(s, `(( log_age > 1800 ))`) {
		t.Errorf("hardcoded 1800s threshold must be replaced by stale_threshold var:\n%s", s)
	}
	if !strings.Contains(s, `(( log_age > stale_threshold ))`) {
		t.Errorf("expected stale_threshold variable in stale check, got:\n%s", s)
	}
}

func TestGenerateScript_OOMCheckPresent(t *testing.T) {
	s := GenerateScript(baseCfg())
	for _, want := range []string{
		`check_oom()`,
		`journalctl --since "$since" --no-pager`,
		`killed by the OOM killer`,
		`clem-[a-zA-Z0-9_-]+\.service`,
		`OOM-kill detected`,
		`free -h`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("generated script missing OOM-check fragment: %q", want)
		}
	}

	// check_oom must be invoked after the per-agent loop so a kill
	// in the same tick still alerts before the next iteration.
	defIdx := strings.Index(s, "check_oom()")
	callIdx := strings.LastIndex(s, "check_oom")
	if defIdx == -1 || callIdx == -1 || callIdx <= defIdx {
		t.Errorf("check_oom must be defined and then invoked (def=%d call=%d)", defIdx, callIdx)
	}

	// marker must be written after the journalctl scan to avoid silently
	// dropping events on interruption between the two lines.
	markerWriteIdx := strings.Index(s, `echo "$new_ts" > "$marker"`)
	journalIdx := strings.Index(s, `journalctl --since "$since" --no-pager`)
	if markerWriteIdx == -1 || journalIdx == -1 || markerWriteIdx <= journalIdx {
		t.Errorf("marker write must appear after journalctl scan (markerWrite=%d journalctl=%d)", markerWriteIdx, journalIdx)
	}
}

func TestGenerateScript_EgressCheckPresentWhenEnabled(t *testing.T) {
	cfg := baseCfg()
	cfg.Egress.Enabled = true
	s := GenerateScript(cfg)
	for _, want := range []string{
		"check_egress()",
		"/var/log/clem/pipelock-test-audit.jsonl",
		"blocked/DLP event(s)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in watchdog script:\n%s", want, s)
		}
	}
	// check_egress must be defined before it is invoked.
	defIdx := strings.Index(s, "check_egress()")
	callIdx := strings.LastIndex(s, "check_egress")
	if defIdx == -1 || callIdx <= defIdx {
		t.Errorf("check_egress must be defined then invoked (def=%d call=%d)", defIdx, callIdx)
	}
}

func TestGenerateScript_NoEgressCheckWhenDisabled(t *testing.T) {
	s := GenerateScript(baseCfg())
	if strings.Contains(s, "check_egress") {
		t.Errorf("expected no check_egress when egress disabled:\n%s", s)
	}
}

func TestGenerateScript_AgentVaultHealthWhenBackendActive(t *testing.T) {
	cfg := baseCfg()
	cfg.Vault.Backend = "agent-vault"
	s := GenerateScript(cfg)
	for _, want := range []string{
		"check_agent_vault()",
		"clem-agent-vault-test.service",
		"http://127.0.0.1:14321/health",
		"agent-vault DOWN",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in watchdog script:\n%s", want, s)
		}
	}
	defIdx := strings.Index(s, "check_agent_vault()")
	callIdx := strings.LastIndex(s, "check_agent_vault")
	if defIdx == -1 || callIdx <= defIdx {
		t.Errorf("check_agent_vault must be defined then invoked (def=%d call=%d)", defIdx, callIdx)
	}
}

func TestGenerateScript_NoAgentVaultCheckWhenEnvBackend(t *testing.T) {
	if strings.Contains(GenerateScript(baseCfg()), "check_agent_vault") {
		t.Error("no agent-vault check expected under default env backend")
	}
}

func TestGenerateScript_TranscriptPrune(t *testing.T) {
	cfg := baseCfg()
	out := GenerateScript(cfg)
	for _, want := range []string{
		"prune_transcripts()",
		`-name "*.jsonl" -mtime +30 -delete`,
		"[0-9a-f]{8}-[0-9a-f]{4}",
		"prune_transcripts\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in watchdog script", want)
		}
	}
	if strings.Contains(out, "{{.AgentHomes}}") {
		t.Errorf("AgentHomes placeholder not substituted")
	}
}

func TestGenerateScript_StaleUsesNightDurationWhenLonger(t *testing.T) {
	cfg := baseCfg()
	for key, ac := range cfg.Agents {
		ac.Iteration = "10m"
		ac.IterationNight = "30m"
		cfg.Agents[key] = ac
	}
	out := GenerateScript(cfg)
	// 1800s night + 300s margin = 2100
	if !strings.Contains(out, `"2100"`) {
		t.Errorf("stale threshold should derive from iteration_night (2100), got:\n%s", out)
	}
}
