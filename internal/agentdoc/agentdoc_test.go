package agentdoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jahwag/clem/internal/config"
)

func testCfg() *config.Config {
	return &config.Config{
		Project:          "myteam",
		PrimaryMilestone: "Ship v1 by 2027-01-01",
		Coordination: config.Coordination{
			Backend:  "discord",
			ServerID: "123",
			Channels: map[string]string{
				"general": "g-id",
				"tasks":   "t-id",
				"alerts":  "a-id",
				"lessons": "l-id",
			},
		},
		Agents: map[string]config.AgentConfig{
			"lead": {Name: "Amara", Role: "Product Owner"},
		},
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
}

func TestRender_SplitConcatsAndSubstitutes(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"CLAUDE.shared.md": "project={{project}}, ms={{primary_milestone}}, ch={{channels.general}}\n",
		"CLAUDE.lead.md":   "agent={{agent.name}} ({{agent.role}}, {{agent.key}})\n",
	})

	got, mode, err := Render(testCfg(), "lead", dir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if mode != ModeSplit {
		t.Errorf("mode = %q, want %q", mode, ModeSplit)
	}
	want := "project=myteam, ms=Ship v1 by 2027-01-01, ch=g-id\nagent=Amara (Product Owner, lead)\n"
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRender_SplitOnlySharedNoAppendix(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"CLAUDE.shared.md": "shared-only for {{project}}\n",
	})

	got, mode, err := Render(testCfg(), "lead", dir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if mode != ModeSplit {
		t.Errorf("mode = %q, want %q", mode, ModeSplit)
	}
	if string(got) != "shared-only for myteam\n" {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestRender_SharedWithoutTrailingNewlineGetsSeparator(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"CLAUDE.shared.md": "no-trailing-newline",
		"CLAUDE.lead.md":   "appendix\n",
	})

	got, _, err := Render(testCfg(), "lead", dir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(got), "no-trailing-newline\nappendix") {
		t.Errorf("missing separator newline: %q", got)
	}
}

func TestRender_LegacyFallback(t *testing.T) {
	dir := t.TempDir()
	// Legacy mode: no CLAUDE.shared.md, only the monolithic file. Substitution
	// is intentionally NOT applied — existing users' content is copied verbatim.
	writeFiles(t, dir, map[string]string{
		"CLAUDE.local.md": "legacy content with {{project}} placeholder left as-is\n",
	})

	got, mode, err := Render(testCfg(), "lead", dir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if mode != ModeLegacy {
		t.Errorf("mode = %q, want %q", mode, ModeLegacy)
	}
	if !strings.Contains(string(got), "{{project}}") {
		t.Errorf("legacy mode must not substitute, got: %q", got)
	}
}

func TestRender_NoFilesReturnsNone(t *testing.T) {
	dir := t.TempDir()

	got, mode, err := Render(testCfg(), "lead", dir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != nil {
		t.Errorf("content = %q, want nil", got)
	}
	if mode != ModeNone {
		t.Errorf("mode = %q, want %q", mode, ModeNone)
	}
}

func TestRender_PerAgentAppendixNotSharedAcrossAgents(t *testing.T) {
	dir := t.TempDir()
	cfg := testCfg()
	cfg.Agents["worker"] = config.AgentConfig{Name: "Athena", Role: "Software Engineer"}

	writeFiles(t, dir, map[string]string{
		"CLAUDE.shared.md": "shared\n",
		"CLAUDE.lead.md":   "lead-only\n",
		"CLAUDE.worker.md": "worker-only\n",
	})

	leadOut, _, err := Render(cfg, "lead", dir)
	if err != nil {
		t.Fatalf("Render lead: %v", err)
	}
	workerOut, _, err := Render(cfg, "worker", dir)
	if err != nil {
		t.Fatalf("Render worker: %v", err)
	}

	if strings.Contains(string(leadOut), "worker-only") {
		t.Error("lead output leaked worker appendix")
	}
	if strings.Contains(string(workerOut), "lead-only") {
		t.Error("worker output leaked lead appendix")
	}
}

func TestRender_UnknownSubstitutionLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"CLAUDE.shared.md": "known={{project}} unknown={{not_a_key}} channel={{channels.missing}}\n",
	})

	got, _, err := Render(testCfg(), "lead", dir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Unknown {{keys}} should pass through so stale references are visible,
	// not silently elided.
	if !strings.Contains(string(got), "{{not_a_key}}") {
		t.Errorf("unknown key was mangled: %q", got)
	}
	if !strings.Contains(string(got), "{{channels.missing}}") {
		t.Errorf("unknown channel was mangled: %q", got)
	}
	if !strings.Contains(string(got), "known=myteam") {
		t.Errorf("known key not substituted: %q", got)
	}
}
