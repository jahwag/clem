# clem threat model

This document states what clem defends against, the guarantees it makes, how they're enforced, and — just as importantly — **what it does *not* protect.** Honesty about the boundary is the point: a security tool you can't reason about precisely isn't one.

## The adversary

An autonomous coding agent is an **untrusted workload.** Prompt injection (from a fetched web page, an issue comment, a dependency's README), a poisoned package, or simply a capable model making a bad decision can turn an agent into an exfiltration engine or a lateral-movement foothold. clem assumes the agent process **will** at some point try to read every secret it can reach and connect to every host it can.

The defended-against actor is therefore **the agent's own runtime** (the `claude`/`opencode` process and everything it spawns, all running as the agent's OS user). clem's job is to make that UID's reach as small as possible **without the agent's cooperation** — because a compromised agent will not cooperate.

## Trust principals

clem deliberately splits work across distinct OS principals so the kernel, not the agent, enforces the boundary:

| Principal | Trust | When it runs |
|---|---|---|
| **operator / root** | trusted | provision time only (creates users, writes firewall rules, decrypts sops) |
| **agent UID** (`<project>-<agent>`) | **untrusted** | continuously, at runtime |
| **`clem-proxy`** (egress proxy) | trusted, isolated | continuously, separate UID |
| **`clem-vault`** (credential broker) | trusted, isolated | continuously, separate UID |
| **`clem-mcp`** (credential sidecar) | trusted, isolated | continuously, separate UID |
| **the `clem` binary** | trusted | **provision time only — not in the loop at runtime** |

The agent UID can read none of the trusted principals' files (`0700`/`0600`, foreign ownership) and cannot become them. The sops age key, when present, is root-owned; the agent cannot decrypt the vault even though the ciphertext is on the host.

## The four dispositions

Every credential an agent's session would otherwise hold in plaintext gets exactly one treatment. The design goal: **after provisioning, the agent's `.env` and process environment contain no high-value secret in usable form.**

### 1. broker — the agent's own outbound HTTP calls
A TLS-MITM credential proxy ([Infisical agent-vault](https://github.com/Infisical/agent-vault)) runs as `clem-vault`. The agent makes a normal HTTPS request carrying a **placeholder**; the proxy matches the destination host against per-agent service rules and injects the **real** credential into the auth header on the way out. The agent's code is unchanged; it simply never holds the key.
- The minted per-agent token is **instance-role `no-access` + vault-role `proxy` ONLY** — it can cause injection but cannot read a credential back or mutate vaults/services (verified: a proxy token gets `Member role required` / `not logged in` on every read/write surface).
- sops remains the git-committable source of truth and re-seeds the broker at every provision (lossless rollback).

### 2. sidecar — non-HTTP, gateway-identity, and scoped-internal secrets
For secrets the broker can't rewrite (a chat-gateway WebSocket token, an internal database), a secret-holding **MCP server runs as `clem-mcp`** (a separate UID) and exposes only specific tools over a **loopback HTTP MCP transport** with a per-agent bearer token. The agent calls a tool and gets a result; it never sees the credential, and it gets a **narrow capability** (e.g. read-only) rather than the full API.
> A **stdio** MCP server does NOT provide this — it runs as the agent's own UID and its secret is in the agent's reach. Isolation requires a *different principal*; the sidecar is that principal.

### 3. remove — unused credentials
The cheapest control: if an agent doesn't need a credential/MCP, it isn't provisioned with one. Attack surface that doesn't exist can't be exfiltrated.

### 4. egress firewall — data containment (the outer boundary)
A per-agent **nftables rule keyed on the agent's UID** rejects all egress except to the loopback proxy port(s). Enforced in the kernel `output` chain; backed by a redundant systemd `IPAddressDeny=any` second layer; the agent unit `Requires=` the firewall service, so a firewall-load failure **stops the agent (fail-closed)**, it does not run uncontained.

## Guarantees

1. **Kernel-enforced egress, not cooperative.** A non-root agent cannot flush or rewrite an nftables ruleset it does not own. There is no `--unsandboxed` escape hatch, because containment is not a flag the agent process controls — it is a property of the UID it runs under. This is a class of containment that **does not exist in any in-process or single-shared-container design**, where the sandbox is part of, or reachable by, the very process being sandboxed.
2. **Secret-zero for brokered credentials.** The agent holds a placeholder and a token that cannot read secrets. `cat ~/.env` and `grep` the process environment yield nothing usable for a brokered credential.
3. **DNS/raw-socket exfil closed.** The firewall rejects UDP/TCP 53 and arbitrary outbound TCP; the proxy resolves DNS on the agent's behalf, so the agent needs no outbound DNS at all.
4. **Reverse-shell allowance is scoped.** The only inbound-established exception is the agent's own web-terminal source port — not a blanket `ct state established` rule that a bind/reverse shell could ride.

## What clem does NOT protect (the irreducible boundary)

State these plainly; an acquirer's or auditor's diligence finds them anyway, and owning them is a trust signal.

- **The agent's own model credential.** The agent must authenticate to its LLM (Claude OAuth in `~/.claude/.credentials.json`, or `ANTHROPIC_API_KEY`). You cannot sidecar the brain; this credential lives on the agent. Mitigation is scope (OAuth, refreshed) and the egress firewall (it can only reach the model endpoint).
- **Secrets the agent must *embed* into an artifact it produces.** If an agent writes a key into a deployment it builds, it needs the real value — the broker injects only on the agent's *own* network egress, not when it writes a file. Such secrets must either stay real, or the *deploy action itself* must move behind a sidecar.
- **Arbitrary sensitive *data*, as opposed to secrets.** If an agent legitimately reads sensitive data to do its job, the broker/sidecar don't stop it from sending that *data* somewhere. That is the **egress firewall's** job (host allowlist + audit), not the credential layer's.
- **A loopback daemon on an allowed port is an un-contained egress UID.** `clem-proxy`/`clem-vault`/`clem-mcp` egress freely (that's their function). Anything reachable on an allowed loopback port that can be coerced into outbound requests is an **SSRF pivot**. Only allow ports for services that cannot be so coerced; a dedicated per-UID allowlist for these is tracked hardening.
- **TLS interception requires CA trust.** Phase-1 egress is CONNECT-only (SNI/host allowlist + audit, no CA needed). Broker injection (Phase 2) intercepts TLS and therefore requires distributing the broker's CA to the agent's trust stores; this trades plaintext-broker-side visibility for a broker that, if itself compromised, sees brokered traffic. The broker runs as an isolated UID with its management API unreachable from the agent firewall to bound this.
- **Supply-chain trust of the wrapped binaries.** pipelock and agent-vault are installed from pinned, checksum-verified releases (integrity). Signature/attestation pinning (authenticity) is tracked follow-up; until then this is TOFU on a tagged release.

## Verifying the guarantees

Run as the agent user; these should all hold on a contained fleet:

```sh
# BLOCKED — direct internet, raw IP, DNS exfil, arbitrary TCP:
sudo -u <agent> env -u HTTPS_PROXY curl --connect-timeout 5 https://example.com   # fails (no route + no DNS)
sudo -u <agent> curl https://1.1.1.1                                             # fails (raw IP)
sudo -u <agent> bash -c 'echo > /dev/tcp/93.184.216.34/443'                      # fails
# ALLOWED — through the proxy only:
sudo -u <agent> curl https://api.anthropic.com/v1/models                         # reaches upstream (401 = auth, not blocked)
# SECRET-ZERO — a brokered key is a placeholder, real value absent:
sudo -u <agent> bash -lc 'echo $BROKERED_KEY; grep -E "sk-|ghp_|xox" ~/.env'     # placeholder; no real secret
# Controls:
nft list table inet clem_egress ; systemctl is-active clem-pipelock-<project>
```

The CI `egress-containment` job runs this exact block on every change, so the security claim is re-verified continuously rather than asserted.

## Reporting

Security issues: please open a private advisory on the repository rather than a public issue.
