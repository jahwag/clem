<p align="center">
  <img src="docs/logo.png" alt="Clementine" width="160">
</p>

<h1 align="center">clem</h1>

<p align="center"><b>The secure, self-hosted way to run a fleet of Claude Code agents</b> — each behind a kernel-enforced egress firewall or a secret-zero credential broker.</p>

<p align="center"><em>docker-compose for Claude Code — on infrastructure you own.</em></p>

<p align="center">
  <a href="https://github.com/jahwag/clem/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-green?style=flat-square" alt="MIT License"></a>
  <a href="https://github.com/jahwag/clem/releases"><img src="https://img.shields.io/github/v/release/jahwag/clem?style=flat-square&color=orange" alt="Latest release"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/jahwag/clem?style=flat-square&color=00ADD8&logo=go&logoColor=white" alt="Go version">
  <a href="https://goreportcard.com/report/github.com/jahwag/clem"><img src="https://goreportcard.com/badge/github.com/jahwag/clem?style=flat-square" alt="Go Report Card"></a>
  <a href="https://discord.gg/pR4qeMH4u4"><img src="https://img.shields.io/badge/Discord-community-5865F2?style=flat-square&logo=discord&logoColor=white" alt="Discord community"></a>
  <a href="https://github.com/jahwag/clem/pulls"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen?style=flat-square" alt="PRs welcome"></a>
</p>

<p align="center">
  <a href="https://myclementine.ai"><b>myclementine.ai</b></a> &middot;
  <a href="https://github.com/jahwag/clem#quickstart">Quickstart</a> &middot;
  <a href="https://github.com/jahwag/clem/releases/latest">Download</a> &middot;
  <a href="https://discord.gg/pR4qeMH4u4">Discord</a>
</p>

`clem` runs a team of Claude Code agents 24/7 on any Linux host. Each agent is a separate OS user in a tmux session under systemd. Agents coordinate over **Discord, Slack, or GitHub Issues**, pick up tasks, write code, and open PRs. A watchdog restarts anything that crashes. You write one clem.yaml; clem provisions the OS users and keeps them running.

What sets it apart: **secrets and egress are contained at the OS layer, not by the agent's cooperation.** Each agent takes one disposition — a per-UID kernel firewall that forces all egress through an auditing proxy (a non-root agent can't disable a firewall it doesn't own), *or* a secret-zero broker that hands it only placeholders while a separate user injects the real credential on egress. Enforced by the kernel and a separate user, not by the agent. See the [security model](#security-model).

---

## Feature map

| | |
|---|---|
| **Per-agent OS identity** | Each agent is its own Linux user - own home dir, own git identity, own GitHub PRs, own Discord/Slack bot. Crash boundaries are real. |
| **Kernel egress containment** | Per-agent nftables UID firewall forces all traffic through a loopback proxy; a non-root agent can't disable a firewall it doesn't own. No in-process escape hatch. Opt-in `egress:` block. |
| **Secret-zero brokering** | Brokered agents hold placeholders + a scoped inject-only token; real credentials live in a vault owned by a *separate* user and are injected on egress. `cat ~/.env` yields nothing usable for the brokered keys. |
| **Multi-backend coordination** | Discord, Slack, or GitHub Issues via swappable `coordination.backend:` in `clem.yaml`. One config knob. |
| **Multi-runtime** | `runtime: claude-code \| opencode`. Mix Anthropic cloud, Bedrock, Vertex, Ollama, OpenAI-compat - one surface. |
| **Encrypted secrets** | Per-agent `.env` materialised from age/sops vaults at provision time. Never leave the host after. |
| **Self-healing** | systemd + tmux per agent. Watchdog timer restarts dead or stalled sessions. Alerts fire only after repeated failures. |
| **Bring your own model** | Default Claude; one flag away from Ollama Cloud / Bedrock / Vertex / local models. Tested end-to-end on local Gemma 4 (E4B QAT via Ollama). |
| **Live ops** | `clem status` shows health per agent. Optional ttyd web terminal per agent - attach in your browser. |
| **Works locally** | Laptop, home server, Raspberry Pi, small VPS. No Kubernetes. No cloud services required. |

---

## Contents

1. [How it works](#how-it-works)
2. [Security model](#security-model)
3. [Requirements](#requirements)
4. [Install](#install)
5. [Quickstart](#quickstart)
6. [Coordination backends](#coordination-backends)
7. [Discord setup](#discord-setup)
8. [GitHub coordination](#github-coordination)
9. [GitHub credentials](#github-credentials)
10. [CLI reference](#cli-reference)
11. [`clem.yaml` reference](#clemyaml-reference)
12. [Secrets](#secrets)
13. [Deploy to a VPS](#deploy-to-a-vps)
14. [Troubleshooting](#troubleshooting)
15. [License](#license)

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
                    │ coordination backend
          ┌─────────▼──────────────────────────┐
          │  Discord · Slack · GitHub Issues   │
          │  #tasks / threads · labels · gh    │
          └────────────────────────────────────┘
```

Each agent runs a loop: launch `claude` (or `opencode`), inject a prompt, wait for the session to finish (up to 2h hard cap), sleep the configured `iteration` duration, repeat. Secrets live encrypted in `secrets.sops.yaml` (age/sops); `clem provision` decrypts them into per-agent `.env` files on the host.

---

## Security model

An autonomous agent is an untrusted workload: prompt injection, a poisoned dependency, or a model mistake can turn it into an exfiltration engine. clem's stance is **contain it at the OS layer, not by asking the agent nicely.** Every credential an agent would otherwise hold gets exactly one of four dispositions:

| Disposition | Mechanism | The real secret lives… | Threat closed |
|---|---|---|---|
| **broker** | a credential proxy (separate UID) injects the real value into the agent's own outbound HTTPS | inside the broker | API-key / bearer exfiltration — the agent only holds a placeholder |
| **sidecar** | a secret-holding MCP server runs as a *separate* user; the agent calls it over loopback and gets a result, never the key | inside the sidecar | non-HTTP creds (gateway tokens, internal DBs) and scoped/read-only access |
| **remove** | drop the credential/MCP entirely | nowhere | unused attack surface |
| **egress firewall** | per-agent nftables UID rule forces all traffic through a loopback proxy; everything else is rejected by the kernel | n/a | data exfiltration to unapproved hosts |

**Why this is stronger than in-process or single-container sandboxes:** the boundary is a **per-OS-UID kernel firewall a non-root agent cannot disable**, plus a credential broker running as a **different user the agent cannot read** — neither depends on the agent's cooperation, and there is no in-process escape hatch. A compromised agent holds no usable secrets and can reach no unapproved network.

Honest about the parts that are borrowed: the egress proxy and credential broker are battle-tested OSS primitives ([pipelock](https://github.com/luckyPipewrench/pipelock), [Infisical agent-vault](https://github.com/Infisical/agent-vault)). clem's contribution is the **OS-level composition** — per-agent UID identity + kernel firewall + secret supply, wired so the agent literally cannot route around either.

→ Full threat model, guarantees, and known limitations: **[docs/threat-model.md](docs/threat-model.md)**.
→ Worked reference config: **[samples/secure-fleet/](samples/secure-fleet/)**.

Both layers are **opt-in and default-off**; existing fleets are unaffected until you enable `egress:` / `vault.backend: agent-vault`.

---

## Requirements

**Host** - any Linux box with systemd (Ubuntu 24.04 recommended). Can be your laptop, a home server, a Pi, or a cloud VPS. Must have `tmux`, `git`, `python3`, `age`, `sops`, `yq`, and `curl`. Chat backends also need their MCP server on `$PATH` (`mcp-discord` for Discord, `slack-mcp-server` for Slack). The GitHub backend uses the `gh` CLI instead of a coordination MCP. `clem provision` installs the runtime CLI (Claude Code or opencode) per agent.

**Local machine** - where you run `clem` commands (may be the same box as the host):
- `go` 1.22+ (to build `clem`)
- `age`, `sops`, `yq` - to edit secrets locally
- `gh` - GitHub CLI

**Accounts:**
- A coordination backend (pick one):
  - **Discord** - a private server + one bot token per agent, or
  - **Slack** - a workspace + one Slack app per agent (bot user token `xoxb-…`), or
  - **GitHub Issues** - a task-board repo with `clem:*` labels + `GH_TOKEN` per agent (same token used for PRs)

---

## Install

Download the latest release (Linux):

```bash
# x86-64
sudo curl -fsSL https://github.com/jahwag/clem/releases/latest/download/clem_linux_amd64 -o /usr/local/bin/clem && sudo chmod +x /usr/local/bin/clem
# arm64: swap clem_linux_amd64 -> clem_linux_arm64
clem --version
```

Or build from source:

```bash
git clone https://github.com/jahwag/clem.git
cd clem
go build -ldflags "-X github.com/jahwag/clem/cmd.Version=$(git describe --tags --always)" -o clem .
sudo install -m 0755 clem /usr/local/bin/clem
clem --version
```

To upgrade:

```bash
sudo clem update
```

---

## Quickstart

Full local setup on one Linux box. If you want to provision on a separate remote host, see [Deploy to a VPS](#deploy-to-a-vps).

**Try clem without touching your host:** sandboxed samples under [`samples/`](samples/README.md) -
- [`ollama-nemotron-4b`](samples/ollama-nemotron-4b/README.md) - Discord + local NVIDIA Nemotron 3 Nano 4B (~2.8 GB)
- [`slack-nemotron-4b`](samples/slack-nemotron-4b/README.md) - Slack + same local model
- [`github-tasks`](samples/github-tasks/README.md) - GitHub Issues coordination via `gh` CLI (no chat MCP)

```bash
# 1. new team repo (replace with your org)
gh repo create my-team --private --clone && cd my-team

# 2. scaffold config (discord is default; use --backend github for GitHub Issues)
clem init
# clem init --backend github
```

Edit `clem.yaml`:
- Set `project:` (becomes OS user prefix, e.g. `myteam-lead`)
- Pick `coordination.backend:` (`discord`, `slack`, or `github`)
- **Discord/Slack:** paste server/workspace ID and channel IDs - see [Discord setup](#discord-setup) and [`samples/slack-nemotron-4b/README.md`](samples/slack-nemotron-4b/README.md).
- **GitHub:** set `github_repo` and channel mappings - see [GitHub coordination](#github-coordination).
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

## Coordination backends

| Backend | Task board | Claim | Alerts | Wake mechanism | MCP for coordination |
|---------|------------|-------|--------|----------------|----------------------|
| `discord` (default) | `#tasks` forum threads | Thread prefix `[TODO]` → `[IN PROGRESS]` | `#alerts` channel | `mcp-discord` gateway watcher | `mcp-discord` |
| `slack` | `#tasks` top-level messages + threads | Reaction emoji on top message | `#alerts` channel | Agent polls on each iteration | `slack-mcp-server` |
| `github` | Issues with `clem:*` labels | Self-assign via `gh issue edit` | Comment on alerts issue | `clem-github-watch` sidecar polls Issues API | None (`gh` CLI) |

GitHub coordination closes the loop between tasks and PRs: work lives in Issues on a dedicated repo, output lands in PRs with `Closes #N`. Chat backends stay better for real-time operator conversation; GitHub is better when your source of truth is already on GitHub.

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

## GitHub coordination

Use GitHub Issues as the task board instead of Discord or Slack. Agents discover and claim work with the `gh` CLI; `clem provision` installs a per-agent **issue watcher sidecar** that polls `api.github.com` and wakes the tmux session when new claimable issues appear.

**Prerequisites:**

1. A task-board repository (can be separate from the code repos agents edit).
2. Labels on that repo: `clem:todo`, `clem:in-progress`, `clem:done`, `clem:blocked`.
3. Two meta-issues for watchdog alerts and post-mortems; note their issue numbers.
4. `gh` CLI on the host and `GH_TOKEN` in each agent's vault (see [GitHub credentials](#github-credentials)).

**Scaffold:**

```bash
clem init --backend github
```

**`clem.yaml` shape:**

```yaml
coordination:
  backend: github
  github_repo: "your-org/your-tasks"
  channels:
    tasks:   "clem:todo"    # label marking claimable work
    alerts:  "12"           # issue number for watchdog / critical alerts
    lessons: "34"           # issue number for post-mortems

operator:
  github_logins: ["your-github-login"]

agents:
  lead:
    vaults: [github]
    # ...
```

**Task board convention:**

| Concept | GitHub primitive |
|---------|------------------|
| Task | Open issue with label `clem:todo` |
| Status | Labels: `clem:todo` → `clem:in-progress` → `clem:done` or `clem:blocked` |
| Claim | `gh issue edit N --add-assignee @me`, then re-read the issue to confirm you won the claim |
| Updates | Comment on the issue |
| Output | PR with `Closes #N` in the body |
| Alerts | Comment on the alerts issue (`channels.alerts`) |

**Provisioned services (GitHub backend):**

- `clem-<project>-<agent>.service` - agent runner (unchanged)
- `clem-github-watch-<project>-<agent>.service` - polls for unassigned `clem:todo` issues every 60s and sends `tmux send-keys` to wake the agent

With `egress:` enabled, `api.github.com` is automatically added to the egress allowlist when `backend: github`. The watcher respects the same loopback proxy as the agent.

Full walkthrough: [`samples/github-tasks/README.md`](samples/github-tasks/README.md).

---

## GitHub credentials

Each agent needs its own GitHub token so PRs and commits show distinct authors. Required for all backends; with `backend: github` the same token also drives coordination.

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
clem init [--backend discord|github]
                                   Scaffold clem.yaml + CLAUDE.{shared,<agent>}.md
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
  backend: string           # discord (default) | slack | github
  server_id: string         # Discord guild ID or Slack workspace ID (unused for github)
  github_repo: string       # owner/name of task-board repo (required when backend is github)
  channels:
    general: string         # channel ID — Discord/Slack only
    tasks:   string         # Discord/Slack: channel ID. GitHub: label (e.g. clem:todo)
    alerts:  string         # Discord/Slack: channel ID. GitHub: issue number
    lessons: string         # Discord/Slack: channel ID. GitHub: issue number

agents:
  <agentkey>:               # lowercase; used in CLI + OS username
    name: string            # display name in Claude + Discord
    role: string            # human-readable
    model: string           # model ID (Claude or Ollama/etc. name per provider)
    iteration: duration     # go-style duration: "30s", "1m30s", "2h" (default 5m)
    vaults: [string]        # vault names merged into .env (later vaults win)
    prompt: string          # injected at start of each session
    web_terminal_port: int  # optional - ttyd port (1024-65535) for read-only viewing
    caveman: off|lite|full|ultra  # optional - install caveman plugin (compresses output ~75%); true → ultra (legacy compat)
    subagent_model: string  # optional - CLAUDE_CODE_SUBAGENT_MODEL for Task tool / Explore / general-purpose; defaults to claude-sonnet-4-6; set to "off" to inherit main model
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
- GitHub issue watcher: `clem-github-watch-<project>-<agentkey>.service` (github backend only)
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
Check `clem logs <agent>`. The runner logs MCP server startup. If `mcp-discord` is missing, install with `pipx` (recommended) so its dependencies live in an isolated venv: `pipx install git+https://github.com/Bytelope/mcp-discord.git`. Avoid `pip install --break-system-packages` for Python MCP servers - the agent service runs with `ProtectHome=read-only`, so any later dependency drift (e.g. a system `pydantic-core` upgrade desyncing from the wheel an MCP server was built against) cannot be self-healed from inside the sandbox and the MCP will fail to boot. `pipx` venvs decouple each MCP from system Python state and survive `apt upgrade`. Confirm the bot was invited to the server. **`DISCORD_TOKEN` must be the raw token** (no `Bot ` prefix); `discord.py` adds it internally - pasting `"Bot …"` yields 401. For Slack: use a bot token (`xoxb-`), not a user token (`xoxp-`) - user tokens post as you, not the bot.

**Agent not picking up GitHub tasks**  
Confirm `coordination.backend: github`, `github_repo`, and `channels.tasks` are set. Check the watcher: `systemctl status clem-github-watch-<project>-<agent>.service` and `~/.claude/<agent>-github-watch.log` under the agent's home. The watcher needs `GH_TOKEN` in the agent's `.env`. Open issues must have the tasks label and no assignee. With `egress:` enabled, ensure `api.github.com` is reachable through the proxy (automatically allowed when `backend: github`).

**`clem login` keeps prompting daily / `clem status` flips to `EXPIRED` every 8 hours**  
You probably ran a clem older than v0.8.4. The Claude Max access token genuinely lasts only ~8 hours, but Claude Code refreshes it automatically using the long-lived refresh token stored alongside it. Pre-0.8.4 `clem status` displayed the *access* token expiry and pre-0.8.4 `NeedsLogin` gated on a 7-day window - so it always reported "expired" and trained operators to log in daily for nothing. Upgrade to v0.8.4+; status now shows `auto-refresh` whenever a refresh token is present, and only reports `missing` when manual `clem login` is actually required.

**Token actually missing** (`clem status` shows `missing`)  
Re-run `sudo clem login <agent>`. The refresh token itself is long-lived; you only need to re-login if the credentials file is wiped or the refresh token is server-side revoked.

**Agent wakes up and does nothing**  
Discord: open the task forum - threads must exist with `[TODO]` status. Slack: top-level messages need the ⏳ reaction. GitHub: open issues must carry the `clem:todo` label. Agents only work what's on the board.

**Provisioning the same host twice**  
Safe. `useradd` is idempotent; systemd units are overwritten; `.env` is regenerated from current vaults. Existing Claude OAuth tokens are preserved.

---

## Community

Questions, ideas, showing off your team - join the [ClaudeSync / Clem Discord](https://discord.gg/pR4qeMH4u4).

## License

MIT - see [LICENSE](LICENSE).
