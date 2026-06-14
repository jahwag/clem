package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// generateSharedMDBackend runs clem init for a specific backend in a temp dir
// and returns the generated CLAUDE.shared.md content.
func generateSharedMDBackend(t *testing.T, backend string) string {
	t.Helper()

	tmp := t.TempDir()

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	cmd := &cobra.Command{Use: "init"}
	cmd.Flags().String("backend", "discord", "")
	if err := cmd.Flags().Set("backend", backend); err != nil {
		t.Fatal(err)
	}
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("runInit(%s) failed: %v", backend, err)
	}

	data, err := os.ReadFile("CLAUDE.shared.md")
	if err != nil {
		t.Fatalf("reading CLAUDE.shared.md: %v", err)
	}
	return string(data)
}

// generateSharedMD runs clem init in a temp dir and returns the generated
// CLAUDE.shared.md content. The working directory is restored when the
// test finishes.
func generateSharedMD(t *testing.T) string {
	t.Helper()

	tmp := t.TempDir()

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	if err := runInit(nil, nil); err != nil {
		t.Fatalf("runInit failed: %v", err)
	}

	data, err := os.ReadFile("CLAUDE.shared.md")
	if err != nil {
		t.Fatalf("reading CLAUDE.shared.md: %v", err)
	}
	return string(data)
}

func TestInitTemplateContainsRunnerExitProtocol(t *testing.T) {
	content := generateSharedMD(t)

	checks := []string{
		"kill $PPID",
		"runner exit protocol",
		"How your session ends",
	}
	for _, want := range checks {
		if !strings.Contains(content, want) {
			t.Errorf("CLAUDE.shared.md missing %q", want)
		}
	}
}

func TestInitTemplateUsesDiscordBotToolPrefix(t *testing.T) {
	content := generateSharedMD(t)

	// The runner registers the Discord MCP server under the key
	// "discord-bot", so Claude Code exposes its tools as
	// mcp__discord-bot__<tool>. The bare mcp__discord__ prefix matches
	// no server and the tool calls silently fail.
	for _, want := range []string{
		"mcp__discord-bot__send_message",
		"mcp__discord-bot__read_messages",
		"mcp__discord-bot__create_forum_post",
		"mcp__discord-bot__edit_thread",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("CLAUDE.shared.md missing %q", want)
		}
	}
	if strings.Contains(content, "mcp__discord__") {
		t.Error("CLAUDE.shared.md still references the nonexistent mcp__discord__ tool prefix")
	}
}

// TestInitTemplateContainsOpenPRMaintenance asserts both backend contracts tell
// agents to keep their own open PRs mergeable (conflicts, failing checks,
// operator review feedback) instead of treating "PR opened" as the end of a task.
func TestInitTemplateContainsOpenPRMaintenance(t *testing.T) {
	for _, backend := range []string{"discord", "github"} {
		content := generateSharedMDBackend(t, backend)
		for _, want := range []string{
			"## Your open PRs",
			"gh pr list",
			"--author @me",
			"keep them mergeable",
			"rebase it onto the latest base",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("[%s] CLAUDE.shared.md missing %q", backend, want)
			}
		}
	}
}
