# clem sample — GitHub Issues coordination

Uses **GitHub Issues** as the task board instead of Discord or Slack. Agents use the
`gh` CLI directly (no GitHub MCP) to discover, claim, and report on tasks. `clem provision`
installs a per-agent issue watcher that polls `api.github.com` and wakes the tmux session
when new claimable issues appear.

## Prerequisites

- `gh` CLI on the host
- A task-board repository (can be separate from the repos agents edit)
- Labels on that repo: `clem:todo`, `clem:in-progress`, `clem:done`, `clem:blocked`
- Two meta-issues for alerts and post-mortems
- `GH_TOKEN` per agent in the vault (same token used for PRs)

## Setup

1. Create the task repo and add the labels above.
2. Open dedicated issues for alerts and lessons; note their issue numbers.
3. Export env vars (or edit `clem.yaml` directly):

```bash
export GITHUB_TASKS_REPO=your-org/your-tasks
export GITHUB_ALERTS_ISSUE=12
export GITHUB_LESSONS_ISSUE=34
```

4. Initialize agent docs:

```bash
clem init --backend github
```

5. Fill in `github_repo`, issue numbers, operator `github_logins`, and agent prompts in `clem.yaml` / `CLAUDE.*.md`.
6. Provision as usual:

```bash
clem vault init
clem vault set github GH_TOKEN="ghp_..."
sudo clem provision
sudo clem login
sudo clem start
```

## Task board convention

| Concept | GitHub primitive |
|---------|------------------|
| Task | Open issue with label `clem:todo` |
| Status | Labels: `clem:todo` → `clem:in-progress` → `clem:done` or `clem:blocked` |
| Claim | `gh issue edit N --add-assignee @me`, then re-read the issue to confirm you won the claim |
| Updates | Comment on the issue |
| Output | PR with `Closes #N` in the body |
| Alerts | Comment on the alerts issue (`channels.alerts`) |
| Lessons | Comment on the lessons issue (`channels.lessons`) |

## How it differs from Discord/Slack samples

| Aspect | Chat backends | GitHub backend |
|--------|---------------|----------------|
| Task discovery | MCP + gateway watcher (Discord) | Sidecar polls Issues + `gh issue list` on each iteration |
| Claim | Thread prefix / emoji | Self-assign + re-read issue |
| Alerts | Post to `#alerts` channel | Comment on alerts issue |
| MCP | `discord-bot` / `slack-mcp` | None (`gh` CLI only) |
| Extra service | — | `clem-github-watch-<project>-<agent>.service` |

## Egress containment

When `egress:` is enabled, `api.github.com` is automatically added to the egress allowlist for `backend: github`. The issue watcher
uses the same loopback proxy as the agent service.

See also the [GitHub coordination section](../../README.md#github-coordination) in the main README.
