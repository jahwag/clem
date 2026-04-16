# Clem Code Review — Findings Report

## Context

User asked for a review of `/home/jahwag/IdeaProjects/clem/` to identify improvement opportunities. No implementation requested — this is a written findings report only, to be acted on later.

## What Clem Is

**Clem** ("Clementine IS Claude Code") orchestrates persistent teams of Claude Code agents on Linux. Each agent runs as a separate OS user in a tmux session managed by systemd, with secrets encrypted via age/sops, Discord coordination, and a watchdog for auto-restart. ~2,100 LOC across 16 files, minimal deps (cobra, yaml.v3). Overall architecture is clean and security-conscious.

## Findings by Priority

### P0 — High impact, small effort

**1. Silent `exec.Run()` error swallowing** — `internal/agent/manager.go:71, 79, 126–127, 228–229, 302`
Multiple subprocess calls (`chown`, `chmod`, `systemctl daemon-reload`) discard return values. File ownership misconfigurations and missed systemd reloads go undetected.
*Fix:* Check every `Run()`. Return or at minimum log stderr.

**2. Zero test coverage** — entire repo
No `*_test.go` files exist. Config parsing, runner template generation, secret merging, and username derivation are all untested.
*Fix:* Start with `internal/config` (validation rules), `internal/vault` (DecryptForAgent merge logic), `internal/runner` (template output stability via golden files).

### P1 — High impact, medium effort

**3. No bash script syntax validation** — `internal/runner/runner.go:10–123`
Generated runner scripts (with embedded Python and escaped prompts) are written to disk without `bash -n` check. A templating bug ships silently until an agent boots.
*Fix:* Pipe output through `bash -n` before `os.WriteFile`.

**4. No `context.Context` propagation** — all `cmd/` and `internal/`
Nothing uses `context.Context`. `exec.Command` is used instead of `exec.CommandContext`. No graceful cancellation, no subprocess timeouts from Go side.
*Fix:* Thread `context.Context` through public APIs; switch to `exec.CommandContext`.

**5. Fragile prompt escaping** — `internal/runner/runner.go:185`
`strings.ReplaceAll(ac.Prompt, "'", "'\\''")` single-quote escape only. Complex prompts with nested constructs risk breakage.
*Fix:* Use heredoc or `base64`-encode the prompt and decode in the runner.

**6. Watchdog restart lacks jitter** — `internal/runner/runner.go:113–121`
Exponential backoff present but no randomization. Multi-agent crash scenarios produce thundering-herd restarts.
*Fix:* Add ±10% jitter to BACKOFF.

### P2 — Medium impact

**7. Hardcoded filesystem paths** — `agent/manager.go`, `runner/runner.go`, `cmd/provision.go`
`/home/%s`, `/etc/systemd/system`, `/usr/local/bin/` assumed everywhere. Non-standard systems break.
*Fix:* Look up via `getent` or accept overrides in `clem.yaml`.

**8. Silent vault override on merge** — `internal/vault/vault.go:226–247`
Later vaults overwrite earlier keys with no warning. Reordering vaults silently changes secrets.
*Fix:* Log warnings on duplicate keys; consider explicit strategy in config.

**9. Duplicate yq-parsing logic** — `internal/vault/vault.go:250–268`
`decryptLegacyAgent()` and `DecryptForAgent()` share identical parsing. Extract `parseYQOutput()`.

**10. `.gitconfig` rewrite is not TOML-safe** — `internal/agent/manager.go:76–80`
Appends if "excludesfile" missing, but raw text append can corrupt structure over repeated runs.
*Fix:* Use a TOML/INI parser.

**11. `ensureSops()` fails before first `vault set`** — `internal/vault/vault.go:330`
UX trap — `secrets.sops.yaml` must exist before any vault op, but the create path is `vault set` itself.
*Fix:* Auto-create empty YAML stub if missing.

**12. Error messages lose subprocess context** — `agent/manager.go`, `vault/vault.go`
e.g. "sops unset: operation failed" without command args or stderr.
*Fix:* Consistently include `cmd.String()` and captured stderr.

**13. Go idiom gaps**
- No `errors.Is`/`errors.As` usage
- No custom error types (`ValidationError`, `VaultNotFoundError`)
- Errors sometimes wrapped multiple times, obscuring origin

### Security red flags (already mitigated but worth noting)

- **SSH `StrictHostKeyChecking=accept-new`** (runner.go, manager.go) — MITM window on first connection. Document pre-populating `known_hosts`.
- **GitHub token in git clone URL** (`internal/remote/remote.go:64`) — correct `oauth2` user, but any stdout logging leaks it. Add explicit "do not log" comment.
- **Discord token via env during curl** (`internal/watchdog/watchdog.go:23`) — visible in `ps aux`. Minor on locked-down VPS; consider `--config` file flag.
- **Wrangler OAuth token on disk** (`manager.go:102–106`) — 0600 mode, not sops-encrypted. Acceptable per design but should be documented.

## Code quality strengths

- Clean package boundaries (config/agent/runner/vault/watchdog/remote)
- Consistent `fmt.Errorf("...: %w", err)` wrapping
- Careful file permissions (0600 for `.env`, 0700 for `.ssh`)
- Minimal dependency surface
- Comprehensive README and Hetzner walkthrough

## Suggested order of attack (when you come back to this)

1. Add tests first (P0 #2) — gives a safety net for everything else
2. Fix silent errors (P0 #1) — one-file sweep
3. `bash -n` validation (P1 #3) — 5 lines, prevents a class of bugs
4. Prompt escaping hardening (P1 #5) — before it bites on a real prompt
5. Context propagation (P1 #4) — larger but unblocks clean shutdown
6. Everything else as incremental polish
