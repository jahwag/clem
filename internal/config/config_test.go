package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clem.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing yaml: %v", err)
	}
	return path
}

func TestLoad_PrimaryMilestoneParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
primary_milestone: "Ship v1 by 2027-01-01"
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g", tasks: "t"}
agents:
  lead:
    name: "Amara"
    role: "Lead"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrimaryMilestone != "Ship v1 by 2027-01-01" {
		t.Errorf("PrimaryMilestone = %q", cfg.PrimaryMilestone)
	}
}

func TestLoad_PrimaryMilestoneOptional(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Amara"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrimaryMilestone != "" {
		t.Errorf("PrimaryMilestone = %q, want empty", cfg.PrimaryMilestone)
	}
}
