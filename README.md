# Clementine

Clementine IS Claude Code.

Tired of Crustaceans? Try Clementine.

`clem` runs a persistent team of Claude Code agents on a Linux server. Each agent gets its own OS user, tmux session, and systemd service. Agents coordinate over Discord, pick up tasks, and restart automatically. You SSH in once to set up, then walk away.

---

## Prerequisites

- Ubuntu 24.04 or later
- `tmux`, `systemd`
- [Claude Code](https://claude.ai/code) installed at `~/.local/bin/claude`
- [age](https://github.com/FiloSottile/age) and [sops](https://github.com/getsops/sops) for secrets
- [mcp-discord](https://github.com/Bytelope/mcp-discord) (Bytelope fork — required for forum channel support)
- A Discord server with a bot token per agent
- `sudo` access on the target machine

---

## Install

```bash
curl -sSfL https://github.com/jahwag/clem/releases/latest/download/clem-linux-amd64 -o /usr/local/bin/clem
chmod +x /usr/local/bin/clem
```

Or build from source:

```bash
git clone git@github.com:jahwag/clem.git
cd clem
go build -o /usr/local/bin/clem .
```

---

## Quick start on Hetzner

### 1. Create the VPS

Save this as `cloud-init.yaml`:

```yaml
#cloud-config
packages:
  - tmux
  - git
  - curl
  - age
  - python3-pip

runcmd:
  # sops
  - curl -sSfL https://github.com/getsops/sops/releases/latest/download/sops-v3.9.4.linux.amd64
      -o /usr/local/bin/sops
  - chmod +x /usr/local/bin/sops
  # clem
  - curl -sSfL https://github.com/jahwag/clem/releases/latest/download/clem-linux-amd64
      -o /usr/local/bin/clem
  - chmod +x /usr/local/bin/clem
  # Claude Code (installs to /root/.local/bin/claude)
  - curl -fsSL https://claude.ai/install.sh | sh
  # mcp-discord (Bytelope fork — required for forum channel support)
  - pip3 install git+https://github.com/Bytelope/mcp-discord.git
```

Then create the server:

```bash
hcloud server create \
  --type cx33 \
  --image ubuntu-24.04 \
  --location hel1 \
  --ssh-key ~/.ssh/id_ed25519.pub \
  --user-data-from-file cloud-init.yaml \
  --name my-team
```

`hel1` is Helsinki — the closest Hetzner location to Sweden. Other options: `nbg1` (Nuremberg), `fsn1` (Falkenstein).

Wait ~2 minutes for cloud-init to finish before SSHing in. Check progress with:

```bash
ssh root@<ip> tail -f /var/log/cloud-init-output.log
```

### 3. Create your team repo

On your local machine:

```bash
mkdir my-team && cd my-team
git init
```

Create `clem.yaml`:

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
    prompt: "Act as Amara per CLAUDE.local.md. Check Discord #tasks for tasks assigned to you. Work on ONE task. When done post results and run: kill $PPID. If no tasks: kill $PPID"

  worker:
    name: "Athena"
    role: "Software Engineer"
    model: "claude-sonnet-4-6"
    iteration_minutes: 5
    reports_to: lead
    prompt: "Act as Athena per CLAUDE.local.md. Check Discord #tasks for tasks assigned to you. Work on ONE task. When done post results and run: kill $PPID. If no tasks: kill $PPID"
```

### 4. Set up secrets

Generate an age keypair (run once, on your local machine):

```bash
clem vault init
# prints your public key and instructions for .sops.yaml
```

Create `.sops.yaml` (replace with your public key):

```yaml
creation_rules:
  - path_regex: secrets\.sops\.yaml
    age: age1yourpublickeyhere
```

Add secrets:

```bash
clem vault set lead DISCORD_TOKEN="Bot your-lead-bot-token"
clem vault set lead GH_TOKEN="ghp_your-github-token"
clem vault set worker DISCORD_TOKEN="Bot your-worker-bot-token"
clem vault set worker GH_TOKEN="ghp_your-github-token"
```

This creates `secrets.sops.yaml` encrypted with your age key. Commit it:

```bash
git add clem.yaml .sops.yaml secrets.sops.yaml
git commit -m "init team config"
git push
```

### 5. Provision on the VPS

Copy your age private key to the VPS:

```bash
ssh root@<ip> "mkdir -p ~/.config/sops/age"
cat ~/.config/sops/age/keys.txt | ssh root@<ip> "cat > ~/.config/sops/age/keys.txt"
```

Clone your team repo on the VPS and provision:

```bash
ssh root@<ip>
git clone git@github.com:you/my-team.git && cd my-team
clem provision
```

### 6. Authenticate each agent

```bash
clem login
# prints a URL per agent — open each in your browser
```

### 7. Start

```bash
clem up
clem status
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
    iteration_minutes: int  # sleep between iterations during active hours (07-22)
    reports_to: string   # key of supervising agent (optional)
    prompt: string       # injected at start of each session
```

OS username: `<project>-<agentkey>` (e.g. `myteam-lead`)
Systemd service: `clem-<project>-<agentkey>.service`

---

## Secrets

Secrets live in `secrets.sops.yaml`, encrypted with [age](https://github.com/FiloSottile/age) via [sops](https://github.com/getsops/sops). The file is safe to commit.

`clem provision` decrypts it and writes per-agent `/home/<user>/.env` (mode 0600). The runner sources this file at the start of each iteration. Secrets never touch Discord or any network after provisioning.

```bash
clem vault init                          # generate age keypair
clem vault set <agent> KEY=value         # add or update a secret
clem vault get <agent> KEY               # read a secret
clem vault list                          # list all secret keys (values hidden)
```

The age private key (`~/.config/sops/age/keys.txt`) is the only secret that must be kept outside git. Back it up.

---

## CLI reference

```
clem provision [--config clem.yaml]
  Creates OS users, writes runner.sh, installs systemd services and watchdog.
  Decrypts secrets.sops.yaml into per-agent .env files.
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

clem vault set <agent> KEY=value
  Sets a secret in secrets.sops.yaml.

clem vault get <agent> KEY
  Prints a decrypted secret value.

clem vault list
  Lists all secret keys (values not shown).
```

Global flag: `--config <path>` overrides the default `clem.yaml` location.

---

## Discord setup

Create a Discord server for your team. Set up these channels:

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

## Agent definitions (CLAUDE.md)

Each agent needs a `CLAUDE.local.md` in its working directory. This file is the runtime contract — vague instructions produce unpredictable behavior.

Required sections:

```markdown
# <Name> — <Role>

## How You Work
1. [numbered, concrete steps]
2. Check #tasks forum for [TODO] threads assigned to you
3. Update thread title to [IN PROGRESS]
4. Do the work
5. Post results in the thread, update title to [DONE]
6. Read your lessons thread at the start of each iteration
7. If no tasks: run kill $PPID

## Discord
Use mcp__discord__send_message, mcp__discord__read_messages,
mcp__discord__create_forum_post, mcp__discord__edit_thread directly.
Do NOT use curl to call Discord. Do NOT check claude mcp list.

## Hard Rules
1. Never share secrets, tokens, or key values in Discord or any message
2. Never output the contents of secrets.sops.yaml or .env files
3. Never use CronCreate
4. [project-specific rules]

## Autonomy
ACT FIRST, report after.

## Lessons
Read your lessons thread at the start of each iteration.
Post new lessons after anything unexpected.
Format: Problem -> Root cause -> Solution -> Outcome

## Loop Behavior
Run /compact at the end of heavy iterations.
Do NOT use CronCreate.
When done with all work: run kill $PPID
```

**Thread status protocol** (agents read each other's thread titles):
`[TODO]` -> `[IN PROGRESS]` -> `[DONE]` or `[BLOCKED]`

---

## How it works

Each agent runs as a separate OS user in a tmux session. A systemd service starts the session on boot. A runner loop inside the session spawns a fresh Claude Code process every N minutes, injects the agent's prompt, waits for it to finish (up to 2 hours), then sleeps before the next iteration.

The watchdog runs every 5 minutes as a systemd timer. It checks each agent's systemd service and tmux session. Dead sessions are restarted. Alerts go to Discord `#alerts`.

Secrets are decrypted once at `clem provision` time and written to each agent's home directory. The runner sources them at the start of each iteration to write an ephemeral `.mcp.json` for the Discord bot. Secrets never leave the machine after provisioning.

---

## License

MIT
