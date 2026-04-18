package coordination

import "fmt"

// Backend describes a chat platform clem can use for agent coordination.
// Two are supported today: Discord (default) and Slack. Anything agent-facing
// that differs between platforms — MCP server name, channel ID format, alert
// POST URL — lives here.
type Backend struct {
	Name           string // "discord" | "slack"
	MCPName        string // the key used in .mcp.json
	MCPBinary      string // absolute path to the MCP server binary
	TokenEnvVar    string // env-var name the MCP server reads for auth
	AlertTemplate  string // curl template for the watchdog alert (see RenderAlert)
	TaskBoardNotes string // short paragraph injected into CLAUDE.shared.md
}

// Known returns the backend for a config value. Unknown backends return an
// error that surfaces at config load time.
func Known(name string) (Backend, error) {
	switch name {
	case "", "discord":
		return discord, nil
	case "slack":
		return slack, nil
	default:
		return Backend{}, fmt.Errorf("unknown coordination backend %q (valid: discord, slack)", name)
	}
}

var discord = Backend{
	Name:        "discord",
	MCPName:     "discord-bot",
	MCPBinary:   "/usr/local/bin/mcp-discord",
	TokenEnvVar: "DISCORD_TOKEN",
	// Raw bot token (no "Bot " prefix) — clem strips it on vault set.
	AlertTemplate: `curl -s -X POST "https://discord.com/api/v10/channels/%s/messages" \
        -H "Authorization: Bot $DISCORD_TOKEN" -H "Content-Type: application/json" \
        -d "{\"content\":\"%s\"}" > /dev/null 2>&1`,
	TaskBoardNotes: `Task board lives in #tasks (forum channel). Each task = one thread.
Use list_threads (not read_messages) to discover work. Status lives in the
thread's first-message prefix: [TODO] → [IN PROGRESS] → [DONE] or [BLOCKED].`,
}

var slack = Backend{
	Name:        "slack",
	MCPName:     "slack-mcp",
	MCPBinary:   "/usr/local/bin/slack-mcp-server",
	TokenEnvVar: "SLACK_MCP_XOXP_TOKEN",
	AlertTemplate: `curl -s -X POST "https://slack.com/api/chat.postMessage" \
        -H "Authorization: Bearer $SLACK_MCP_XOXP_TOKEN" -H "Content-Type: application/json; charset=utf-8" \
        -d "{\"channel\":\"%s\",\"text\":\"%s\"}" > /dev/null 2>&1`,
	TaskBoardNotes: `Task board lives in #tasks (regular channel). Each task = the top-level
message; updates happen inside its thread. Status lives as a reaction emoji on
the top message: ⏳ (TODO) → 🔨 (IN PROGRESS) → ✅ (DONE) or ⛔ (BLOCKED).
Slack has no forum channel type — threads replace first-class forum posts.`,
}
