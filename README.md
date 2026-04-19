<p align="center">
  <img src="docs/logo.png" alt="Clementine" width="160">
</p>

<h1 align="center">clem</h1>

<p align="center"><em>Continuously Looping Engineering Machines.</em></p>

<p align="center"><em><b>docker-compose for Claude Code agents.</b></em></p>

<p align="center">
  <a href="https://github.com/jahwag/clem/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-green?style=flat-square" alt="MIT License"></a>
  <a href="https://github.com/jahwag/clem/releases"><img src="https://img.shields.io/github/v/release/jahwag/clem?style=flat-square&color=orange" alt="Latest release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/jahwag/clem?style=flat-square&color=00ADD8&logo=go&logoColor=white" alt="Go version">
  <a href="https://goreportcard.com/report/github.com/jahwag/clem"><img src="https://goreportcard.com/badge/github.com/jahwag/clem?style=flat-square" alt="Go Report Card"></a>
  <a href="https://discord.gg/pR4qeMH4u4"><img src="https://img.shields.io/badge/Discord-community-5865F2?style=flat-square&logo=discord&logoColor=white" alt="Discord community"></a>
  <a href="https://github.com/jahwag/clem/pulls"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen?style=flat-square" alt="PRs welcome"></a>
</p>

`clem` runs a team of Claude Code agents 24/7 on any Linux host. Each agent is a separate OS user in a tmux session under systemd. Agents coordinate over Discord or Slack, pick up tasks, write code, and open PRs. A watchdog restarts anything that crashes. You configure it once and walk away.

---

## Feature map

| | |
|---|---|
| **Per-agent OS identity** | Each agent is its own Linux user - own home dir, own git identity, own GitHub PRs, own Discord/Slack bot. Crash boundaries are real. |
| **Multi-backend coordination** | Discord + Slack today via swappable `coordination.backend:` in `clem.yaml`. One config knob. |
| **Multi-runtime** | `runtime: claude-code \| opencode`. Mix Anthropic cloud, Bedrock, Vertex, Ollama, OpenAI-compat - one surface. |
| **Encrypted secrets** | Per-agent `.env` materialised from age/sops vaults at provision time. Never leave the host after. |
| **Self-healing** | systemd + tmux per agent. Watchdog timer restarts dead or stalled sessions. Alerts fire only after repeated failures. |
| **Bring your own model** | Default Claude; one flag away from Ollama Cloud / Bedrock / Vertex / local models. Tested end-to-end on NVIDIA Nemotron. |
| **Live ops** | `clem status` shows health per agent. Optional ttyd web terminal per agent - attach in your browser. |
| **Works locally** | Laptop, home server, Raspberry Pi, small VPS. No Kubernetes. No cloud services required. |

---

## Contents

1. [How it works](#how-it-works)
2. [Requirements](#requirements)
3. [Install](#install)
4. [Quickstart](#quickstart)
5. [Discord setup](#discord-setup)
6. [GitHub setup](#github-setup)
7. [CLI reference](#cli-reference)
8. [`clem.yaml` reference](#clemyaml-reference)
9. [Secrets](#secrets)
10. [Deploy to a VPS](#deploy-to-a-vps)
11. [Troubleshooting](#troubleshooting)
12. [License](#license)

---

## How it works

```
┌──────────────────────────────────────────────────────┐
│  Linux host  (your laptop · home server · VPS · …)   │
│                                                      │
│  ┌──────────────┐   ┌──────────────┐                 │
│  │ OS user:     │   │ OS user:     │   systemd +     │
│  │ myteam-lead  │   │ myteam-worker│   tmux per user │
│  │  claude loop │   │  claude loop │                 │
│  └──────┬───────┘   └──────┬───────┘                 │
│         └──── MCP (stdio) ─┘                         │
│                     │                                │
│  ┌──────────────────┴──────────────────┐             │
│  │  watchdog timer (every 5 min)       │             │
│  │  restarts dead agents → #alerts     │             │
│  └─────────────────────────────────────┘             │
└───────────────────┬──────────────────────────────────┘
                    │ coordination backend API
          ┌─────────▼──────────┐
          │  Discord or Slack  │
          │  #general #tasks   │
          │  #alerts #lessons  │
          └────────────────────┘
```

Each agent runs a loop: launch `claude` (or `opencode`), inject a prompt, wait for the session to finish (up to 2h hard cap), sleep the configured `iteration` duration, repeat. Secrets live encrypted in `secrets.sops.yaml` (age/sops); `clem provision` decrypts them into per-agent `.env` files on the host.

---

## Requirements

**Host** - any Linux box with systemd (Ubuntu 24.04 recommended). Can be your laptop, a home server, a Pi, or a cloud VPS. Must have `tmux`, `git`, `python3`, `age`, `sops`, `yq`, and `curl`. Plus the MCP server for your chosen coordination backend - `mcp-discord` (Python) or `slack-mcp-server` (binary) - reachable via `$PATH`. `clem provision` installs the runtime CLI (Claude Code or opencode) per agent.

**Local machine** - where you run `clem` commands (may be the same box as the host):
- `go` 1.22+ (to build `clem`)
- `age`, `sops`, `yq` - to edit secrets locally
- `gh` - GitHub CLI

**Accounts:**
- A coordination backend:
  - **Discord** - a private server + one bot token per agent, or
  - **Slack** - a workspace + one Slack app per agent (bot user token `xoxb-…`)
- A GitHub token per agent (fine-grained PAT or App)

---

## Install

Build from source:

```bash
git clone https://github.com/jahwag/clem.git
cd clem
go build -ldflags "-X github.com/jahwag/clem/cmd.Version=$(git describe --tags --always)" -o clem .
sudo install -m 0755 clem /usr/local/bin/clem
clem --version
```

To upgrade later, once releases are published:

```bash
sudo clem update
```

---

## Quickstart

Full local setup on one Linux box. If you want to provision on a separate remote host, see [Deploy to a VPS](#deploy-to-a-vps).

**Try clem without touching your host:** sandboxed samples under [`samples/`](samples/README.md) - 
- [`ollama-nemotron-4b`](samples/ollama-nemotron-4b/README.md) - Discord + local NVIDIA Nemotron 3 Nano 4B (~2.8 GB)
- [`slack-nemotron-4b`](samples/slack-nemotron-4b/README.md) - Slack + same local model

```bash
# 1. new team repo (replace with your org)
gh repo create my-team --private --clone && cd my-team

# 2. scaffold config
clem init
```

Edit `clem.yaml`:
- Set `project:` (becomes OS user prefix, e.g. `myteam-lead`)
- Pick `coordination.backend:` (`discord` or `slack`)
- Paste your server/workspace ID and channel IDs - see [Discord setup](#discord-setup) below. Slack setup lives in [`samples/slack-nemotron-4b/README.md`](samples/slack-nemotron-4b/README.md).
- Adjust agent `name`, `role`, `model`, `iteration` (Go duration: `30s`, `1m30s`, `2h`), `runtime`, `provider`

Edit `CLAUDE.shared.md` - describe your project, fill in tiers T2-T4. Edit each `CLAUDE.<agentkey>.md` with per-agent specifics.

```bash
# 3. generate age keypair + .sops.yaml
clem vault init

# 4. store per-agent secrets (see Discord/GitHub setup below)
clem vault set github        GH_TOKEN="ghp_..."
clem vault set discord-lead  DISCORD_TOKEN="Bot <lead-bot-token>"
clem vault set discord-worker DISCORD_TOKEN="Bot <worker-bot-token>"

# 5. commit config (secrets.sops.yaml is encrypted - safe)
git add clem.yaml CLAUDE.*.md .sops.yaml secrets.sops.yaml
git commit -m "init team config"
git push

# 6. provision - creates OS users, installs services, writes .env
sudo clem provision

# 7. authenticate each agent with Claude (opens browser per agent)
sudo clem login

# 8. start and check
sudo clem up
clem status
```

`clem status` shows systemd state, tmux liveness, token expiry, and last log line per agent. Once `SYSTEMD=active` and `TMUX=yes`, agents are running.

Watch an agent:

```bash
clem logs lead
```

---

## Discord setup

Create a **private** Discord server (not a public one). Discord membership is the access control layer - agents act on instructions from anyone who can post in the channels.

**Channels to create:**

| Name       | Type   | Purpose                                 |
|------------|--------|-----------------------------------------|
| `#general` | Text   | Status updates, operator comms          |
| `#tasks`   | Forum  | Task board - agents claim threads here  |
| `#alerts`  | Text   | Critical issues, watchdog alerts        |
| `#lessons` | Forum  | Post-mortems, learnings                 |

Enable **Developer Mode** (Settings → Advanced), then right-click the server icon and each channel to copy their IDs into `clem.yaml`.

**Bot per agent** - one application per agent gives each a distinct name and avatar in task threads:

1. https://discord.com/developers/applications → **New Application** (name it after the agent)
2. **Bot** tab → **Reset Token** → copy
3. Enable **Server Members Intent** and **Message Content Intent**
4. **OAuth2 → URL Generator**: scopes `bot`; permissions `Send Messages`, `Read Message History`, `Attach Files`, `Manage Threads`, `Create Public Threads`
5. Open the generated URL in a browser, add the bot to your server
6. Save the token: `clem vault set discord-<agentkey> DISCORD_TOKEN="Bot <token>"`

Repeat per agent.

---

## GitHub setup

Each agent needs its own GitHub token so PRs and commits show distinct authors.

**Fine-grained PAT** (simplest, good for personal projects):

1. https://github.com/settings/tokens?type=beta → **Generate new token**
2. Select the target repositories
3. Permissions: `Contents` (RW), `Pull requests` (RW), `Issues` (RW), `Workflows` (RW)
4. `clem vault set github GH_TOKEN="ghp_..."` (or a per-agent vault if you want separate tokens)

**Git identity per agent** - so PRs are authored by the agent's name, not root. Run after `clem provision`:

```bash
sudo -u myteam-lead git config --global user.name  "Amara"
sudo -u myteam-lead git config --global user.email "amara@yourproject.com"
sudo -u myteam-lead git config --global credential.helper store
echo "https://amara:ghp_...@github.com" | \
  sudo -u myteam-lead tee /home/myteam-lead/.git-credentials
```

Repeat per agent.

**GitHub App** (recommended for teams) - create one app per agent, exchange the private key for a short-lived installation token each iteration. See [GitHub App authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app).

---

## CLI reference

```
clem --version                     Print the installed version
clem update                        Download and install the latest release
clem init                          Scaffold clem.yaml + CLAUDE.{shared,<agent>}.md
clem vault init                    Generate age keypair + .sops.yaml
clem vault set <vault> KEY=value   Set a secret in a vault
clem vault get <vault> KEY         Read a decrypted secret
clem vault list                    List all vaults and their keys (values hidden)
clem vault delete <vault> [KEY]    Delete a secret or entire vault
clem provision [--remote HOST]     Create OS users, write .env, install services (root)
clem login [agent...]              Run `claude /login` as each agent (one-time)
clem up [agent...]                 Start agent systemd services (root)
clem down [agent...]               Stop agent systemd services (root)
clem status                        Table: systemd · tmux · token expiry · last log
clem logs <agent>                  Tail an agent's runner log
```

Flags:
- `--config <path>` - override the default `clem.yaml` path
- `--remote <user@host>` on `provision`/`login` - run against a remote host over SSH

---

## `clem.yaml` reference

```yaml
project: string             # OS user and service name prefix
primary_milestone: string   # optional - referenced by CLAUDE.shared.md

coordination:
  backend: string           # discord (default) | slack
  server_id: string         # Discord guild ID or Slack workspace ID
  channels:
    general: string         # channel ID (C... for Slack, numeric for Discord)
    tasks:   string         # task board channel (forum on Discord)
    alerts:  string         # alerts channel
    lessons: string         # lessons channel (forum on Discord)

agents:
  <agentkey>:               # lowercase; used in CLI + OS username
    name: string            # display name in Claude + Discord
    role: string            # human-readable
    model: string           # model ID (Claude or Ollama/etc. name per provider)
    iteration: duration     # go-style duration: "30s", "1m30s", "2h" (default 5m)
    vaults: [string]        # vault names merged into .env (later vaults win)
    prompt: string          # injected at start of each session
    web_terminal_port: int  # optional - ttyd port (1024-65535) for read-only viewing
    caveman: bool           # optional - install caveman plugin (compresses output ~75%)
    subagent_model: string  # optional - CLAUDE_CODE_SUBAGENT_MODEL for Task tool / Explore / general-purpose
    provider: string        # optional - anthropic (default) | bedrock | vertex | ollama | openai-compat
    provider_url: string    # required when provider is ollama or openai-compat
    runtime: string         # optional - claude-code (default) | opencode
```

**Runtimes:**

| `runtime`     | CLI binary                          | Notes                                                                               |
|---------------|-------------------------------------|-------------------------------------------------------------------------------------|
| `claude-code` | `~/.local/bin/claude`               | Default. Anthropic-native wire format. Best for cloud Claude.                       |
| `opencode`    | `~/.opencode/bin/opencode`          | Talks natively to 75+ providers via models.dev. Better tool-use on local models.   |

**Providers:**

| `provider`       | Extra env `clem` writes into `.env`                                          | Notes                                                     |
|------------------|------------------------------------------------------------------------------|-----------------------------------------------------------|
| `anthropic`      | none (default behaviour)                                                     | Uses Claude Code's OAuth or `ANTHROPIC_API_KEY`           |
| `bedrock`        | `CLAUDE_CODE_USE_BEDROCK=1`                                                  | Agent also needs AWS creds in a vault                     |
| `vertex`         | `CLAUDE_CODE_USE_VERTEX=1`                                                   | Agent also needs `GOOGLE_APPLICATION_CREDENTIALS`         |
| `ollama`         | `ANTHROPIC_BASE_URL=<url>` · `ANTHROPIC_MODEL=<model>` · `ANTHROPIC_AUTH_TOKEN=none` | Ollama natively speaks Anthropic API - no proxy needed    |
| `openai-compat`  | same as `ollama`                                                             | Requires you to run an Anthropic-wire translator yourself |

Derived names:
- OS user: `<project>-<agentkey>` (e.g. `myteam-lead`)
- Systemd service: `clem-<project>-<agentkey>.service`
- Web terminal: `clem-ttyd-<project>-<agentkey>.service`

---

## Secrets

Secrets live in `secrets.sops.yaml`, encrypted with age via sops. The file is safe to commit. The age private key (`~/.config/sops/age/keys.txt`) is the only thing you must keep out of git - back it up.

`clem provision` decrypts secrets into per-agent `/home/<user>/.env` (mode 0600). The runner sources this at the start of each iteration. Secrets never leave the host after provisioning.

Each agent's `vaults:` list specifies which vaults to merge, in order. Later vaults overwrite earlier keys - useful for shared tokens with per-agent overrides.

Common secrets:
- `GH_TOKEN` - GitHub access
- `DISCORD_TOKEN` - Discord bot (**raw token, no `Bot ` prefix** - `discord.py` adds it)
- `SLACK_MCP_XOXP_TOKEN` - Slack bot (`xoxb-…`) or user (`xoxp-…`) token
- `SSH_HOST`, `ES_PASSWORD` - optional, enables Prefect MCP server
- `WRANGLER_OAUTH_TOKEN` - optional, enables Cloudflare Workers MCP

---

## Deploy to a VPS

`clem` doesn't require a VPS - any Linux host works. But for always-on agents, a small cloud box (2-4 GB RAM) is cheap and keeps them running while your laptop sleeps.

Remote provisioning flow:

```bash
# on your local machine, inside your team repo
clem provision --remote root@<vps-ip> --gh-token ghp_...
clem login --remote root@<vps-ip>
ssh <vps-ip> "cd my-team && clem up && clem status"
```

See [docs/hetzner.md](docs/hetzner.md) for a Hetzner-specific walkthrough (cloud-init, `hcloud` CLI, SSH config).

---

## Troubleshooting

**`clem provision` fails with `useradd: command not found`**  
Not Linux, or missing core userspace. Use a standard Ubuntu/Debian host.

**`clem status` shows `SYSTEMD=failed`**  
Inspect the service: `systemctl status clem-<project>-<agentkey>.service`. Common causes: `.env` missing (run `clem provision` again after setting vaults), `claude` not installed per agent (provision reinstalls), or MCP server binary missing on PATH.

**Agent not posting to Discord/Slack**  
Check `clem logs <agent>`. The runner logs MCP server startup. If `mcp-discord` is missing, install: `pip3 install --break-system-packages git+https://github.com/Bytelope/mcp-discord.git`. Confirm the bot was invited to the server. **`DISCORD_TOKEN` must be the raw token** (no `Bot ` prefix); `discord.py` adds it internally - pasting `"Bot …"` yields 401. For Slack: use a bot token (`xoxb-`), not a user token (`xoxp-`) - user tokens post as you, not the bot.

**Token expired** (`clem status` shows `EXPIRED`)  
Re-run `sudo clem login <agent>`. OAuth tokens last ~30 days. You can also automate refresh via cron.

**Agent wakes up and does nothing**  
Open the task forum - threads must exist with `[TODO]` status. Agents only work what's on the board.

**Provisioning the same host twice**  
Safe. `useradd` is idempotent; systemd units are overwritten; `.env` is regenerated from current vaults. Existing Claude OAuth tokens are preserved.

---

## Community

Questions, ideas, showing off your team - join the [ClaudeSync / Clem Discord](https://discord.gg/pR4qeMH4u4).

## License

MIT - see [LICENSE](LICENSE).
