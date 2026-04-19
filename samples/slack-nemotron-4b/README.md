# clem sample - Slack + Ollama + NVIDIA Nemotron 3 Nano 4B

Same local model as `ollama-nemotron-4b/` but routes coordination through a Slack workspace instead of Discord.

See [`../README.md`](../README.md) for the shared build command and tour/runtime modes.

---

## Prerequisites on the host

Same Ollama setup as the Discord sample:

```bash
ollama serve &
ollama pull nemotron-3-nano:4b
```

## Slack app setup

Each agent = its own Slack app (same pattern as Discord). One app gives one bot user with a distinct name, avatar, and user ID in channels - so agent posts look like messages from a named bot, not from yourself.

1. Create a Slack app per agent: https://api.slack.com/apps → **Create New App → From scratch**
2. **Basic Information** → set Display Name + Icon. This becomes the bot's identity.
3. **OAuth & Permissions** → add **Bot Token Scopes**:
   - `chat:write`
   - `channels:history`, `channels:read`, `channels:join`
   - `groups:history`, `groups:read`
   - `im:history`, `im:read`
   - `mpim:history`, `mpim:read`
   - `reactions:read`, `reactions:write`
   - `users:read`
4. **Install App** → copy the **Bot User OAuth Token** (starts `xoxb-…`)
5. In Slack: either invite the bot per channel (`/invite @<botname>`) or let it self-join (needs `channels:join` scope).

Store in the agent's vault:

```bash
clem vault set slack-lead SLACK_MCP_XOXP_TOKEN="xoxb-..."
```

(The env var name is `SLACK_MCP_XOXP_TOKEN` because [korotovsky/slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) accepts both user (`xoxp-`) and bot (`xoxb-`) tokens under the same variable.)

Get your workspace + channel IDs:
- Workspace: URL bar on `app.slack.com`, looks like `T01234ABCDE`
- Channel: right-click channel → **View channel details** → bottom of modal, `C01234ABCDE`

## Build

From the repo root:

```bash
docker build -f samples/Dockerfile --build-arg SAMPLE=slack-nemotron-4b -t clem-slack .
```

## Runtime quickstart

```bash
podman run -d --rm --name clem-slack --systemd=always \
  -p 7681:7681 \
  -e SLACK_WORKSPACE_ID=T01234ABCDE \
  -e SLACK_GENERAL_CHANNEL=C01234... \
  -e SLACK_TASKS_CHANNEL=C02345... \
  -e SLACK_ALERTS_CHANNEL=C03456... \
  -e SLACK_LESSONS_CHANNEL=C04567... \
  clem-slack /sbin/init
podman exec -it clem-slack bash
```

Open <http://localhost:7681/> to watch the agent's tmux session live (read-only ttyd).

Inside the container:

```bash
clem vault init
clem vault set slack-lead SLACK_MCP_XOXP_TOKEN="xoxp-..."
clem vault set github     GH_TOKEN="ghp_..."
clem provision
clem up
clem status
```

## Task board convention on Slack

Slack has no forum channel type, so the protocol looks a bit different from Discord:

- **`#tasks`** - each task is a top-level message in the channel
- **Status** is expressed as a reaction emoji on the top-level message:
  - ⏳ `:hourglass_flowing_sand:` - TODO
  - 🔨 `:hammer:` - IN PROGRESS
  - ✅ `:white_check_mark:` - DONE
  - ⛔ `:no_entry:` - BLOCKED
- **Discussion** happens inside the message's thread
- **`#lessons`** - same pattern. One top-level message per lesson, discussion in thread

## Notes

- MCP server: [korotovsky/slack-mcp-server](https://github.com/korotovsky/slack-mcp-server) (MIT). Installed from the upstream release binary in the sample image.
- A User token (`xoxp-…`) works against *any* Slack workspace the user belongs to; it does not appear as a separate bot account. If you want a distinct account per agent, create additional Slack users.
