package runner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jahwag/clem/internal/config"
)

// mockHome overrides userHomeLookup for a test, returning testHome for any user.
// Returns a cleanup function that restores the original.
func mockHome(t *testing.T, testHome string) {
	t.Helper()
	orig := userHomeLookup
	userHomeLookup = func(_ string) (string, error) { return testHome, nil }
	t.Cleanup(func() { userHomeLookup = orig })
}

func baseCfg(agentKey string, ac config.AgentConfig) *config.Config {
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
		Agents: map[string]config.AgentConfig{agentKey: ac},
	}
}

func TestGenerate_CavemanInjectsLevel(t *testing.T) {
	for _, level := range []config.CavemanLevel{config.CavemanLite, config.CavemanFull, config.CavemanUltra} {
		cfg := baseCfg("lead", config.AgentConfig{
			Name:      "Lead",
			Model:     "claude-opus-4-7",
			Iteration: "1m",
			Prompt:    "do the thing",
			Caveman:   level,
		})
		out := Generate(cfg, "lead")
		want := "/caveman " + level.Level()
		if !strings.Contains(out, want) {
			t.Errorf("level=%q: expected %q in runner, got:\n%s", level, want, out)
		}
	}
}

func TestGenerate_CavemanOffNoInjection(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")
	if strings.Contains(out, "/caveman") {
		t.Fatalf("expected no /caveman when unset, got:\n%s", out)
	}
}

func TestGenerate_SubagentModelExportPresent(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:          "Lead",
		Model:         "claude-opus-4-7",
		Iteration:     "1m",
		Prompt:        "do the thing",
		SubagentModel: "claude-sonnet-4-6",
	})

	out := Generate(cfg, "lead")

	want := `export CLAUDE_CODE_SUBAGENT_MODEL="claude-sonnet-4-6"`
	if !strings.Contains(out, want) {
		t.Fatalf("expected runner to contain %q, got:\n%s", want, out)
	}
}

func TestGenerate_SubagentModelExportAbsentWhenUnset(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out := Generate(cfg, "lead")

	if strings.Contains(out, "CLAUDE_CODE_SUBAGENT_MODEL") {
		t.Fatalf("expected no subagent export when unset, got:\n%s", out)
	}
}

func TestGenerate_SubagentModelOnOpencodeRuntime(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:          "Lead",
		Runtime:       "opencode",
		Model:         "nemotron-3-nano:4b",
		Iteration:     "1m",
		Prompt:        "do the thing",
		SubagentModel: "claude-sonnet-4-6",
	})

	out := Generate(cfg, "lead")

	want := `export CLAUDE_CODE_SUBAGENT_MODEL="claude-sonnet-4-6"`
	if !strings.Contains(out, want) {
		t.Fatalf("expected opencode runner to contain %q, got:\n%s", want, out)
	}
}

func TestGenerate_AutoAppendsKillPPIDWhenMissing(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing", // no kill $PPID
	})

	out := Generate(cfg, "lead")

	if !strings.Contains(out, "kill $PPID") {
		t.Fatalf("expected auto-appended kill $PPID, got:\n%s", out)
	}
}

func TestGenerate_PreservesUserKillPPID(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing then kill $PPID",
	})

	out := Generate(cfg, "lead")

	if c := strings.Count(out, "kill $PPID"); c != 1 {
		t.Fatalf("expected exactly one kill $PPID, got %d in:\n%s", c, out)
	}
}

func TestGenerate_DisablesClaudeAIConnectorMCPs(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out := Generate(cfg, "lead")

	for _, want := range []string{
		"export ENABLE_CLAUDEAI_MCP_SERVERS=false",
		"export CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected runner to contain %q, got:\n%s", want, out)
		}
		// Must export BEFORE sourcing .env so operators can override per-host.
		exportIdx := strings.Index(out, want)
		sourceIdx := strings.Index(out, `source "$HOME/.env"`)
		if exportIdx < 0 || sourceIdx < 0 || exportIdx > sourceIdx {
			t.Errorf("expected %q to precede .env source (export=%d, source=%d)", want, exportIdx, sourceIdx)
		}
	}
}

func TestGenerateService_EgressRestrictionEnabled(t *testing.T) {
	mockHome(t, "/home/test-lead")
	cfg := baseCfg("lead", config.AgentConfig{
		Name:                          "Lead",
		Model:                         "claude-opus-4-7",
		Iteration:                     "1m",
		Prompt:                        "do the thing",
		EgressRestrictionExperimental: true,
	})

	out, err := GenerateService(cfg, "lead")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	for _, want := range []string{"IPAddressDeny=any", "IPAddressAllow=localhost", "IPAddressAllow=140.82.112.0/20"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in service unit, got:\n%s", want, out)
		}
	}
}

func TestGenerateService_EgressRestrictionDisabled(t *testing.T) {
	mockHome(t, "/home/test-lead")
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out, err := GenerateService(cfg, "lead")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	if strings.Contains(out, "IPAddressDeny") {
		t.Fatalf("expected no IPAddressDeny when egress_restriction unset, got:\n%s", out)
	}
}

func TestGenerate_DiscordWatchChannelsWired(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out := Generate(cfg, "worker")

	// Channels are sorted by name (alerts, general, tasks) -> 111,333,222.
	wantList := "111,333,222"
	if !strings.Contains(out, "DISCORD_WATCH_CHANNELS") {
		t.Fatalf("expected DISCORD_WATCH_CHANNELS substitution, got:\n%s", out)
	}
	if !strings.Contains(out, wantList) {
		t.Fatalf("expected channel list %q in runner, got:\n%s", wantList, out)
	}
	if !strings.Contains(out, "CLEM_TMUX_TARGET") {
		t.Fatalf("expected CLEM_TMUX_TARGET substitution, got:\n%s", out)
	}
	// Tmux target = agent key, since clem starts the tmux session under that name.
	if !strings.Contains(out, "'CLEM_TMUX_TARGET'] = 'worker'") {
		t.Fatalf("expected tmux target = 'worker', got:\n%s", out)
	}
}

func TestGenerate_DiscordWatchEmptyWhenNoChannels(t *testing.T) {
	cfg := &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend:  "discord",
			Channels: map[string]string{},
		},
		Agents: map[string]config.AgentConfig{
			"worker": {
				Name:      "Worker",
				Model:     "claude-opus-4-7",
				Iteration: "1m",
				Prompt:    "do the thing",
			},
		},
	}

	out := Generate(cfg, "worker")

	// _watch resolves to '' so the wrapper if-block stays inert: tokens may be set
	// but neither DISCORD_WATCH_CHANNELS nor CLEM_TMUX_TARGET should be assigned.
	if strings.Contains(out, "_discord_env['DISCORD_WATCH_CHANNELS']") &&
		!strings.Contains(out, "_watch = ''") {
		t.Fatalf("expected empty _watch when no channels configured, got:\n%s", out)
	}
}

func TestGenerate_DiscordWatchSkippedForNonDiscordBackend(t *testing.T) {
	cfg := &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend: "slack",
			Channels: map[string]string{
				"general": "C1234",
			},
		},
		Agents: map[string]config.AgentConfig{
			"worker": {
				Name:      "Worker",
				Model:     "claude-opus-4-7",
				Iteration: "1m",
				Prompt:    "do the thing",
			},
		},
	}

	out := Generate(cfg, "worker")

	// Slack channel IDs must not leak into the Discord-watch env block.
	if strings.Contains(out, "C1234") {
		t.Fatalf("expected slack channel id NOT to appear in discord watcher block, got:\n%s", out)
	}
}

func TestGenerateService_PullsTtydUp(t *testing.T) {
	mockHome(t, "/home/test-worker")
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out, err := GenerateService(cfg, "worker")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	// Wants= ensures starting clem-test-worker also pulls the ttyd sidecar.
	// Without this, BindsTo+PartOf only propagate stops back, leaving the
	// web terminal dead until the next provision.
	want := "Wants=clem-ttyd-test-worker.service"
	if !strings.Contains(out, want) {
		t.Fatalf("expected %q in service unit, got:\n%s", want, out)
	}
}

func TestGenerateTtydService_JoinsAgentPrivateTmp(t *testing.T) {
	mockHome(t, "/home/test-worker")
	cfg := baseCfg("worker", config.AgentConfig{
		Name: "Worker", Model: "claude-opus-4-7", Iteration: "1m", Prompt: "do the thing",
	})

	out := GenerateTtydService(cfg, "worker")

	// The agent unit runs with PrivateTmp=yes; ttyd must opt into the same
	// namespacing AND join the agent's namespace, otherwise tmux attach fails
	// because the socket lives in a /tmp it cannot see (clem #106).
	for _, want := range []string{
		"PrivateTmp=yes",
		"JoinsNamespaceOf=clem-test-worker.service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in ttyd unit, got:\n%s", want, out)
		}
	}
}

func TestGenerateService_HardeningDirectivesPresent(t *testing.T) {
	mockHome(t, "/home/test-lead")
	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Model: "claude-opus-4-7", Iteration: "1m", Prompt: "do the thing",
	})
	out, err := GenerateService(cfg, "lead")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	for _, want := range []string{
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=read-only",
		"PrivateTmp=yes",
		"ReadOnlyPaths=/home/test-lead/CLAUDE.md /home/test-lead/CLAUDE.local.md",
		"ReadWritePaths=/home/test-lead/.claude /home/test-lead/.local/state /home/test-lead/test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in service unit, got:\n%s", want, out)
		}
	}
}

func TestGenerateService_HardeningUsesAbsoluteHomePath(t *testing.T) {
	const customHome = "/data/agents/custom-home"
	mockHome(t, customHome)
	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Model: "claude-opus-4-7", Iteration: "1m", Prompt: "do the thing",
	})
	out, err := GenerateService(cfg, "lead")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	if !strings.Contains(out, customHome) {
		t.Errorf("expected absolute home path %q in service unit, got:\n%s", customHome, out)
	}
	if strings.Contains(out, "%h") {
		t.Errorf("service unit must not contain %%h specifier, got:\n%s", out)
	}
}

func TestGenerateService_MissingUserFails(t *testing.T) {
	orig := userHomeLookup
	userHomeLookup = func(username string) (string, error) {
		return "", fmt.Errorf("user not found: %s", username)
	}
	t.Cleanup(func() { userHomeLookup = orig })

	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Model: "claude-opus-4-7", Iteration: "1m", Prompt: "do the thing",
	})
	_, err := GenerateService(cfg, "lead")
	if err == nil {
		t.Fatal("expected error for missing user, got nil")
	}
}
