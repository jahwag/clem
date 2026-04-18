# Quick start on Hetzner

End-to-end walkthrough of provisioning a Clementine team on a fresh Hetzner Cloud VPS. For the `clem` tool itself (install, CLI reference, `clem.yaml` schema, etc.) see the [main README](../README.md).

Before starting: set up your Discord server and GitHub tokens (see [Discord setup](../README.md#discord-setup) and [GitHub bot setup](../README.md#github-bot-setup)). You will need them in the secrets step below.

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
    iteration: 10m
    vaults: [github, discord-lead]
    prompt: "Act as Amara per CLAUDE.local.md. Check Discord #tasks for tasks assigned to you. Work on ONE task. When done post results and run: kill $PPID. If no tasks: kill $PPID"

  worker:
    name: "Athena"
    role: "Software Engineer"
    model: "claude-sonnet-4-6"
    iteration: 5m
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

# separate discord bot tokens per agent — raw token, no "Bot " prefix
# (discord.py adds the prefix internally; passing "Bot …" yields 401)
clem vault set discord-lead   DISCORD_TOKEN="your-lead-bot-token"
clem vault set discord-worker DISCORD_TOKEN="your-worker-bot-token"
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
  - golang-go

runcmd:
  - "curl -sSfL https://github.com/getsops/sops/releases/download/v3.12.2/sops-v3.12.2.linux.amd64 -o /usr/local/bin/sops && chmod +x /usr/local/bin/sops"
  - "curl -sSfL https://github.com/mikefarah/yq/releases/download/v4.52.5/yq_linux_amd64 -o /usr/local/bin/yq && chmod +x /usr/local/bin/yq"
  - "git clone https://github.com/jahwag/clem.git /tmp/clem && cd /tmp/clem && go build -o /usr/local/bin/clem . && rm -rf /tmp/clem"
  - "bash -c 'curl -fsSL https://claude.ai/install.sh | bash'"
  - "pip3 install --break-system-packages --ignore-installed git+https://github.com/Bytelope/mcp-discord.git"
  - "curl -sL https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64 -o /usr/local/bin/ttyd && chmod +x /usr/local/bin/ttyd"
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

