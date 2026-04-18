// Package agentdoc composes each agent's CLAUDE.local.md from a shared
// template and an optional per-agent appendix, with {{var}} substitution.
//
// Layout in the team repo:
//
//	CLAUDE.shared.md        — concatenated into every agent's file
//	CLAUDE.<agentkey>.md    — appended after shared (optional)
//	CLAUDE.local.md         — legacy monolithic file, used only if shared is absent
//
// Substitution keys:
//
//	{{project}}              cfg.Project
//	{{primary_milestone}}    cfg.PrimaryMilestone
//	{{agent.key}}            the agent key (e.g. "lead")
//	{{agent.name}}           ac.Name
//	{{agent.role}}           ac.Role
//	{{channels.<name>}}      cfg.Coordination.Channels[<name>]
package agentdoc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jahwag/clem/internal/config"
)

// Mode reports which composition path produced the returned content.
type Mode string

const (
	ModeSplit  Mode = "split"  // CLAUDE.shared.md (+ per-agent appendix)
	ModeLegacy Mode = "legacy" // monolithic CLAUDE.local.md copied verbatim
	ModeNone   Mode = "none"   // nothing found; caller should skip the write
)

// Render composes the final CLAUDE.local.md bytes for an agent.
//
// Precedence: if CLAUDE.shared.md exists in repoDir, split mode is used.
// Otherwise, falls back to legacy mode (copy CLAUDE.local.md as-is).
// If neither file exists, returns (nil, ModeNone, nil).
//
// Substitution is applied only in split mode — legacy files predate the
// substitution convention and are copied verbatim.
func Render(cfg *config.Config, agentKey, repoDir string) ([]byte, Mode, error) {
	sharedPath := filepath.Join(repoDir, "CLAUDE.shared.md")
	perAgentPath := filepath.Join(repoDir, fmt.Sprintf("CLAUDE.%s.md", agentKey))
	legacyPath := filepath.Join(repoDir, "CLAUDE.local.md")

	sharedBytes, err := os.ReadFile(sharedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, ModeNone, fmt.Errorf("reading %s: %w", sharedPath, err)
		}
		legacyBytes, legacyErr := os.ReadFile(legacyPath)
		if legacyErr != nil {
			if os.IsNotExist(legacyErr) {
				return nil, ModeNone, nil
			}
			return nil, ModeNone, fmt.Errorf("reading %s: %w", legacyPath, legacyErr)
		}
		return legacyBytes, ModeLegacy, nil
	}

	var sb strings.Builder
	sb.Write(sharedBytes)

	perAgentBytes, err := os.ReadFile(perAgentPath)
	switch {
	case err == nil:
		if len(sharedBytes) > 0 && sharedBytes[len(sharedBytes)-1] != '\n' {
			sb.WriteByte('\n')
		}
		sb.Write(perAgentBytes)
	case !os.IsNotExist(err):
		return nil, ModeNone, fmt.Errorf("reading %s: %w", perAgentPath, err)
	}

	return []byte(substitute(sb.String(), cfg, agentKey)), ModeSplit, nil
}

func substitute(content string, cfg *config.Config, agentKey string) string {
	ac := cfg.Agents[agentKey]
	pairs := []string{
		"{{project}}", cfg.Project,
		"{{primary_milestone}}", cfg.PrimaryMilestone,
		"{{agent.key}}", agentKey,
		"{{agent.name}}", ac.Name,
		"{{agent.role}}", ac.Role,
	}
	for name, id := range cfg.Coordination.Channels {
		pairs = append(pairs, fmt.Sprintf("{{channels.%s}}", name), id)
	}
	return strings.NewReplacer(pairs...).Replace(content)
}
