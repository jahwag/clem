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
