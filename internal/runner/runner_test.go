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

func TestGenerate_SubstitutesPromptPlaceholders(t *testing.T) {
	cfg := baseCfg("workerb", config.AgentConfig{
		Name:      "Solane",
		Role:      "Software Engineer",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "Act as {{agent.name}} ({{agent.role}}) in #{{channels.general}}",
	})

	out := Generate(cfg, "workerb")

	for _, want := range []string{"Act as Solane (Software Engineer)", "#333"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in runner, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{{agent.name}}") || strings.Contains(out, "{{agent.role}}") || strings.Contains(out, "{{channels.general}}") {
		t.Errorf("placeholders left unsubstituted in runner:\n%s", out)
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

// TestGenerate_McpBinResolverPrefersPipx pins the pipx-vs-system fallback
// in the runner's .mcp.json writer. v0.9.5 introduced _mcp_bin to stop
// hardcoding /usr/local/bin paths that desync from system Python state and
// require manual operator edits to .mcp.json every iteration. The helper
// must (a) be defined, (b) prefer /opt/pipx/bin, (c) fall back to
// /usr/local/bin, and (d) be used at every Python-MCP call site.
func TestGenerate_McpBinResolverPrefersPipx(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")

	if !strings.Contains(out, "def _mcp_bin(name):") {
		t.Error("runner must define _mcp_bin in the .mcp.json writer")
	}
	if !strings.Contains(out, "'/opt/pipx/bin/' + name") {
		t.Error("_mcp_bin must check the pipx path /opt/pipx/bin/<name> first")
	}
	if !strings.Contains(out, "'/usr/local/bin/' + name") {
		t.Error("_mcp_bin must fall back to /usr/local/bin/<name>")
	}
	// prefect-mcp was removed (SSH-based MCPs are dropped under agent-vault).
	for _, mcp := range []string{"mcp-discord", "social-mcp", "slack-mcp-server"} {
		want := "_mcp_bin('" + mcp + "')"
		if !strings.Contains(out, want) {
			t.Errorf("runner must resolve %s via _mcp_bin, expected substring %q", mcp, want)
		}
	}
	if strings.Contains(out, "prefect-mcp") {
		t.Error("prefect-mcp should have been removed from the runner template")
	}
	// Hardcoded /usr/local/bin/<mcp> calls outside _mcp_bin would defeat
	// the fallback. Pin them out so a future copy-paste cannot regress.
	for _, banned := range []string{
		"'command': '/usr/local/bin/mcp-discord'",
		"'command': '/usr/local/bin/social-mcp'",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("runner must not hardcode %q (use _mcp_bin)", banned)
		}
	}
}

func TestGenerateService_EgressEnabledLoopbackOnly(t *testing.T) {
	mockHome(t, "/home/test-lead")
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	cfg.Egress.Enabled = true

	out, err := GenerateService(cfg, "lead")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	// Loopback-only block + pipelock/nftables unit ordering, no hardcoded CIDRs.
	for _, want := range []string{
		"IPAddressDeny=any",
		"IPAddressAllow=127.0.0.0/8",
		"After=clem-pipelock-test.service clem-nftables-test.service",
		"Wants=clem-pipelock-test.service",
		"Requires=clem-nftables-test.service", // firewall is fail-closed
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in service unit, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "104.16.0.0/13") || strings.Contains(out, "140.82.112.0/20") {
		t.Errorf("hardcoded CIDR allowlist should be gone, got:\n%s", out)
	}
}

func TestGenerateService_DeprecatedFlagStillEnables(t *testing.T) {
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
	if !strings.Contains(out, "IPAddressDeny=any") {
		t.Errorf("deprecated egress_restriction_experimental should still enable containment, got:\n%s", out)
	}
}

func TestGenerateService_EgressDisabled(t *testing.T) {
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
		t.Fatalf("expected no IPAddressDeny when egress unset, got:\n%s", out)
	}
	if strings.Contains(out, "clem-pipelock") {
		t.Fatalf("expected no pipelock unit deps when egress unset, got:\n%s", out)
	}
}

func TestGenerate_ProxyExportPresentWhenEgressEnabled(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	cfg.Egress = config.EgressConfig{Enabled: true, ProxyPort: 9001}

	out := Generate(cfg, "worker")
	if !strings.Contains(out, "export HTTPS_PROXY=http://127.0.0.1:9001") {
		t.Errorf("expected HTTPS_PROXY export at configured port, got:\n%s", out)
	}
	if !strings.Contains(out, "export NO_PROXY=127.0.0.1,localhost,::1") {
		t.Errorf("expected NO_PROXY export, got:\n%s", out)
	}
}

func TestGenerate_NoProxyExportWhenEgressDisabled(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out := Generate(cfg, "worker")
	if strings.Contains(out, "HTTPS_PROXY") {
		t.Errorf("expected no HTTPS_PROXY export when egress disabled, got:\n%s", out)
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

	// JoinsNamespaceOf is a [Unit]-section directive. If it lands in
	// [Service] systemd silently ignores it and the namespace is not joined
	// (clem #106 follow-up). Anchor on newline to avoid matching the same
	// tokens inside doc comments.
	serviceIdx := strings.Index(out, "\n[Service]")
	joinsIdx := strings.Index(out, "\nJoinsNamespaceOf=")
	if serviceIdx == -1 || joinsIdx == -1 {
		t.Fatalf("missing required section/directive in ttyd unit:\n%s", out)
	}
	if joinsIdx > serviceIdx {
		t.Errorf("JoinsNamespaceOf must live in [Unit] before [Service], got:\n%s", out)
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
		"PrivateTmp=yes",
		"ReadOnlyPaths=-/home/test-lead/CLAUDE.md -/home/test-lead/CLAUDE.local.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in service unit, got:\n%s", want, out)
		}
	}
	// ProtectHome=read-only was dropped in v0.9.3 — see buildHardeningDirectives
	// doc comment for the rationale (cross-agent isolation already comes from
	// 0750 perms on /home/<agent>; ProtectHome added EROFS whack-a-mole without
	// adding security against the threat model). Pin it removed so a future
	// well-meaning re-add cannot silently regress every Claude Code path.
	for _, banned := range []string{
		"ProtectHome=",
		"ReadWritePaths=",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("hardening must not contain %q (cross-agent isolation = Unix perms; per-path RW carveouts cause EROFS regressions). Got:\n%s", banned, out)
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

func TestGenerate_OpencodeRunnerHasClaudeMdGuard(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Runtime:   "opencode",
		Model:     "nemotron-3-nano:4b",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")

	for _, want := range []string{
		"MAX_CLAUDE_MD_BYTES=12288",
		"MAX_LESSONS_MESSAGES=25",
		`if [ -f "$WORKDIR/CLAUDE.local.md" ]`,
		"SIZE > MAX_CLAUDE_MD_BYTES",
		"WARNING: CLAUDE.local.md is ${SIZE} bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("opencode runner missing CLAUDE.local.md guard: expected %q\nfull output:\n%s", want, out)
		}
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
