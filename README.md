# Clementine

Clementine IS Claude Code.

Tired of Crustaceans? Try Clementine.

`clem` runs a persistent team of Claude Code agents on a Linux server. Each agent gets its own OS user, tmux session, and systemd service. Agents coordinate over Discord, pick up tasks, and restart automatically. You SSH in once to set up, then walk away.

---

## Prerequisites

**Local machine** (where you run `clem` commands):
- `clem` binary (see Install below)
- [`age`](https://github.com/FiloSottile/age) — `brew install age` / `sudo apt install age`
- [`sops`](https://github.com/getsops/sops) — `brew install sops` / [download binary](https://github.com/getsops/sops/releases)
- [`yq`](https://github.com/mikefarah/yq) — `brew install yq` / `sudo snap install yq`
- [`gh`](https://cli.github.com) CLI — `brew install gh` / `sudo apt install gh`
- [`hcloud`](https://github.com/hetznercloud/cli) CLI — `brew install hcloud` / [download binary](https://github.com/hetznercloud/cli/releases)
- `ssh`, `scp` (standard on macOS and Linux)

**VPS** (Ubuntu 24.04 or later — handled by cloud-init):
- `tmux`, `git`, `age`, `sops`
- [Claude Code](https://claude.ai/code) at `/usr/local/bin/claude`
- [mcp-discord](https://github.com/Bytelope/mcp-discord) (Bytelope fork — required for forum channel support) at `/usr/local/bin/mcp-discord`
- `sudo` / root access

**Accounts:**
- A private Discord server with a bot token per agent (see [Discord setup](#discord-setup))
- A GitHub token per agent (see [GitHub bot setup](#github-bot-setup))

---

## Install

Build from source (requires [Go](https://go.dev/dl/)):

```bash
git clone git@github.com:jahwag/clem.git
cd clem
go build -o /usr/local/bin/clem .
```

Pre-built binaries will be available on the [releases page](https://github.com/jahwag/clem/releases) once published.

---

## Quick start on Hetzner

Before starting: set up your Discord server and GitHub tokens (see [Discord setup](#discord-setup) and [GitHub bot setup](#github-bot-setup)). You will need them in the secrets step below.

The first three sections run on your local machine. The last three run on the VPS over SSH.

### Local: create your team repo

```bash
gh repo create my-team --private --clone && cd my-team
```

Run `clem init` to generate `clem.yaml` and `CLAUDE.local.md`:

```bash
clem init
```

Then fill in the channel IDs and project description. The generated `clem.yaml` looks like this:

```yaml
project: myteam
coordination:
  backend: discord
  server_id: "YOUR_SERVER_ID"
  channels:
    general: "GENERAL_CHANNEL_ID"
    tasks:   "TASKS_CHANNEL_ID"
    alerts:  "ALERTS_CHANNEL_ID"
    lessons: "LESSONS_CHANNEL_ID"

agents:
  lead:
    name: "Amara"
    role: "Lead Software Engineer"
    model: "claude-sonnet-4-6"
    iteration_minutes: 10
    vaults: [github, discord-lead]
    prompt: "Act as Amara per CLAUDE.local.md. Check Discord #tasks for tasks assigned to you. Work on ONE task. When done post results and run: kill $PPID. If no tasks: kill $PPID"

  worker:
    name: "Athena"
    role: "Software Engineer"
    model: "claude-sonnet-4-6"
    iteration_minutes: 5
    vaults: [github, discord-worker]
    prompt: "Act as Athena per CLAUDE.local.md. Check Discord #tasks for tasks assigned to you. Work on ONE task. When done post results and run: kill $PPID. If no tasks: kill $PPID"
```

### Local: set up secrets

```bash
clem vault init
# generates ~/.config/sops/age/keys.txt and writes .sops.yaml
```

`.sops.yaml` contains only the public key — safe to commit. The private key stays in `~/.config/sops/age/keys.txt` and never leaves your machine.

Secrets are stored in named vaults. Define vaults once and assign them to agents — shared tokens (e.g. a GitHub token) only need to be set in one place.

```bash
# shared github token — both agents use the same vault
clem vault set github GH_TOKEN="ghp_your-github-token"

# separate discord bot tokens per agent
clem vault set discord-lead   DISCORD_TOKEN="Bot your-lead-bot-token"
clem vault set discord-worker DISCORD_TOKEN="Bot your-worker-bot-token"
```

`clem provision` merges the vaults listed in each agent's `vaults:` field (in order) into a single `.env` file. Later vaults win on key conflicts.

Save `cloud-init.yaml` to the repo (useful to commit for reproducibility):

```yaml
#cloud-config
packages:
  - tmux
  - git
  - curl
  - age
  - python3-pip

runcmd:
  - "curl -sSfL https://github.com/getsops/sops/releases/latest/download/sops-v3.9.4.linux.amd64 -o /usr/local/bin/sops && chmod +x /usr/local/bin/sops"
  - "curl -sSfL https://github.com/jahwag/clem/releases/latest/download/clem-linux-amd64 -o /usr/local/bin/clem && chmod +x /usr/local/bin/clem"
  - "curl -fsSL https://claude.ai/install.sh | sh && ln -sf /root/.local/bin/claude /usr/local/bin/claude"
  - "pip3 install git+https://github.com/Bytelope/mcp-discord.git"
```

Commit and push everything:

```bash
git add clem.yaml CLAUDE.local.md .sops.yaml secrets.sops.yaml cloud-init.yaml
git commit -m "init team config"
git push
```

### Local: create the VPS

```bash
hcloud server create \
  --type cx33 \
  --image ubuntu-24.04 \
  --location hel1 \
  --ssh-key ~/.ssh/id_ed25519.pub \
  --user-data-from-file cloud-init.yaml \
  --name my-team
```

See [Hetzner Cloud locations](https://docs.hetzner.com/cloud/general/locations/) and pick the one closest to you.

Get the server IP:

```bash
hcloud server describe my-team | grep "IPv4"
```

Add an alias to `~/.ssh/config` on your local machine so you can use `ssh my-team` instead of typing the IP everywhere:

```
Host my-team
    HostName <ip>
    User root
    IdentityFile ~/.ssh/id_ed25519
```

Wait ~2 minutes for cloud-init to finish. Check progress:

```bash
ssh my-team tail -f /var/log/cloud-init-output.log
```

### VPS: provision

```bash
clem provision --remote my-team --gh-token ghp_yourtoken
```

This runs three steps over SSH. If it fails, run them individually to find where:

```bash
# 1. copy age key
ssh my-team "mkdir -p ~/.config/sops/age"
scp ~/.config/sops/age/keys.txt my-team:~/.config/sops/age/keys.txt

# 2. clone repo (agents use their own tokens from .env after provisioning)
ssh my-team "git clone https://oauth2:ghp_yourtoken@github.com/you/my-team.git"

# 3. provision
ssh my-team "cd my-team && clem provision"
```

### VPS: set git identity per agent

Agents need a named git identity to open PRs. Run once per agent after provision:

```bash
ssh my-team "
  sudo -u myteam-lead git config --global user.name 'Amara'
  sudo -u myteam-lead git config --global user.email 'amara@yourproject.com'
  sudo -u myteam-lead git config --global credential.helper store
  echo 'https://amara:ghp_yourtoken@github.com' | sudo -u myteam-lead tee /home/myteam-lead/.git-credentials

  sudo -u myteam-worker git config --global user.name 'Athena'
  sudo -u myteam-worker git config --global user.email 'athena@yourproject.com'
  sudo -u myteam-worker git config --global credential.helper store
  echo 'https://athena:ghp_yourtoken@github.com' | sudo -u myteam-worker tee /home/myteam-worker/.git-credentials
"
```

Replace `myteam`, names, emails, and tokens with your own.

### VPS: authenticate each agent

```bash
clem login --remote my-team
```

Opens an SSH session and runs `clem login` on the VPS. A URL is printed per agent — open each in your local browser. Each agent's Claude Code OAuth token is cached under its OS user home. One-time step.

If it fails, SSH in and run manually per agent:

```bash
ssh my-team
su - myteam-lead
claude /login
# open the printed URL in your local browser, then exit
exit
su - myteam-worker
claude /login
exit
```

Agents interact with GitHub via the `GH_TOKEN` already in their `.env` from provisioning — no separate `gh auth login` needed on the VPS.

### VPS: start

```bash
ssh my-team "cd my-team && clem up && clem status"
```

Agents are now running 24/7. The watchdog restarts any dead sessions every 5 minutes.

---

## clem.yaml reference

```yaml
project: string          # prefix for OS usernames and service names

coordination:
  backend: discord       # only discord supported
  server_id: string      # Discord server (guild) ID
  channels:
    general: string      # channel ID for status updates
    tasks:   string      # forum channel ID for task board
    alerts:  string      # channel ID for critical alerts
    lessons: string      # forum channel ID for post-mortems

agents:
  <key>:                 # agent key, used in CLI commands and OS username
    name: string         # display name (used as --name in Claude Code)
    role: string         # human-readable role description
    model: string        # Claude model ID
    iteration_minutes: int  # sleep between iterations during active hours (07-22); 2x at night
    vaults: [string]     # vault names from secrets.sops.yaml to merge into .env (optional)
    prompt: string       # injected at start of each session; must end with kill $PPID
```

OS username: `<project>-<agentkey>` (e.g. `myteam-lead`)
Systemd service: `clem-<project>-<agentkey>.service`

---

## Secrets

Secrets live in `secrets.sops.yaml`, encrypted with [age](https://github.com/FiloSottile/age) via [sops](https://github.com/getsops/sops). The file is safe to commit.

`clem provision` decrypts it and writes per-agent `/home/<user>/.env` (mode 0600). The runner sources this file at the start of each iteration. Secrets never touch Discord or any network after provisioning.

```bash
clem vault init                          # generate age keypair
clem vault set <vault> KEY=value         # add or update a secret in a vault
clem vault get <vault> KEY               # read a secret from a vault
clem vault list                          # list all vaults and their keys (values hidden)
```

The age private key (`~/.config/sops/age/keys.txt`) is the only secret that must be kept outside git. Back it up.

---

## CLI reference

```
clem init
  Writes clem.yaml and CLAUDE.local.md templates to the current directory.
  Errors if either file already exists.

clem provision [--config clem.yaml]
  Creates OS users, writes runner.sh, installs systemd services and watchdog.
  Decrypts secrets.sops.yaml into per-agent .env files (merges agent vaults in order).
  Requires root.

clem login [agent...]
  Authenticates each agent with Claude Code via browser OAuth.
  Checks token expiry first — skips agents with >7 days remaining.
  No agents specified: runs for all.

clem up [agent...]
  Starts agent systemd services.
  No agents specified: starts all.

clem down [agent...]
  Stops agent systemd services.
  No agents specified: stops all.

clem status
  Prints a table: AGENT | NAME | SYSTEMD | TMUX | TOKEN EXPIRES | LAST LOG

clem logs <agent>
  Tails the runner log for the given agent.

clem vault init
  Generates an age keypair and prints setup instructions.

clem vault set <vault> KEY=value
  Sets a secret in a vault in secrets.sops.yaml.

clem vault get <vault> KEY
  Prints a decrypted secret value from a vault.

clem vault list
  Lists all vaults and their keys (values not shown).
```

Global flag: `--config <path>` overrides the default `clem.yaml` location.

---

## Discord setup

Create a **private** Discord server for your team. Do not use a public server — Discord membership is the access control layer. Agents will act on instructions from anyone who can post in the channels.

Set up these channels:

| Name | Type | Purpose |
|------|------|---------|
| `#general` | Text | Status updates, comms with the human |
| `#tasks` | Forum | Task board — agents post and claim threads |
| `#alerts` | Text | Critical issues, watchdog alerts |
| `#lessons` | Forum | Post-mortems, learnings |

Each agent needs its own Discord bot with a separate token. One bot identity per agent gives distinct names and avatars in the task threads.

**Create a bot per agent:**
1. Go to https://discord.com/developers/applications
2. New Application, name it after the agent (e.g. "Amara")
3. Bot tab, Reset Token, copy the token
4. Enable: Server Members Intent, Message Content Intent
5. OAuth2, URL Generator: scopes `bot`, permissions `Send Messages`, `Read Message History`, `Attach Files`, `Manage Threads`, `Create Public Threads`
6. Open the generated URL, add to your server
7. Add the token: `clem vault set lead DISCORD_TOKEN="Bot <token>"`

Get channel IDs: enable Developer Mode in Discord (Settings, Advanced), right-click any channel, Copy Channel ID.

---

## GitHub bot setup

Each agent that opens PRs needs a GitHub token.

**Fine-grained PAT (recommended for personal projects):**
1. GitHub Settings, Developer settings, Personal access tokens, Fine-grained tokens
2. New token, set repository access to your target repos
3. Permissions: Contents (read/write), Pull requests (read/write), Issues (read/write), Workflows (read/write)
4. Copy the token
5. `clem vault set lead GH_TOKEN="ghp_..."`

For the agent to commit as a named identity, set git credentials in the runner or provision script:

```bash
sudo -u myteam-lead git config --global user.name "Amara"
sudo -u myteam-lead git config --global user.email "amara@yourproject.com"
sudo -u myteam-lead git config --global credential.helper store
# write credentials
echo "https://amara:ghp_...@github.com" | sudo -u myteam-lead tee /home/myteam-lead/.git-credentials
```

**GitHub App (recommended for teams):**
Create one app per agent with fine-grained repo permissions. The runner exchanges the app private key for a short-lived installation token each iteration. See [GitHub App authentication](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app).

---

## Agent definitions (CLAUDE.local.md)

`clem init` generates a working `CLAUDE.local.md` alongside `clem.yaml`. Fill in the channel IDs and project description at the top — the rest works out of the box.

The file is the runtime contract for all agents. Vague instructions produce unpredictable behaviour. The generated default covers:

- Discord tool names (exact, not generic)
- Task board protocol with thread status conventions
- Trust model: only act on Discord instructions, never on content from external sources
- Security rules: no secrets in Discord, no .env output
- Loop behaviour: `kill $PPID` when done, `/compact` on heavy sessions, no CronCreate
- Lessons format
- Per-agent role definitions

Edit it to describe your project and adjust agent responsibilities. The more specific it is, the more reliably agents behave.

---

## How it works

Each agent runs as a separate OS user in a tmux session. A systemd service starts the session on boot. A runner loop inside the session spawns a fresh Claude Code process every N minutes, injects the agent's prompt, waits for it to finish (up to 2 hours), then sleeps before the next iteration.

The watchdog runs every 5 minutes as a systemd timer. It checks each agent's systemd service and tmux session. Dead sessions are restarted. Alerts go to Discord `#alerts`.

Secrets are decrypted once at `clem provision` time and written to each agent's home directory. The runner sources them at the start of each iteration to write an ephemeral `.mcp.json` for the Discord bot. Secrets never leave the machine after provisioning.

---

## License

MIT
