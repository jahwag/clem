# Sample: the secure fleet

A worked reference for clem's least-privilege security model — the configuration shape behind [`docs/threat-model.md`](../../docs/threat-model.md). Configured HTTP credentials are isolated behind a separate OS user; the contained worker can reach no unapproved network because its UID is pinned to an allowlisting proxy by the kernel. The sample also calls out credentials, such as Discord gateway tokens, that remain real until moved behind a sidecar.

## What it demonstrates

Every credential gets one of four **dispositions**:

| Disposition | In this sample | Mechanism |
|---|---|---|
| **broker** | both agents broker their configured HTTP credentials | `vault.backend: agent-vault` + `vault.services` + per-agent `vault_broker`/`brokered_secrets`. agent-vault (separate UID) injects the real value on the agent's own outbound HTTPS; the agent's `.env` holds placeholders for those configured keys + a scoped inject-only token. |
| **egress firewall** | the `worker` is egress-contained | top-level `egress:` block → per-agent nftables UID firewall forces all egress through a loopback proxy; everything else is kernel-rejected. |
| **sidecar** | recommended for `DISCORD_TOKEN`, but not configured in this minimal sample | the WS gateway token can't be HTTP-brokered and a stdio MCP would leak it (same UID); a privileged sidecar (`clem-mcp` user, loopback HTTP MCP) can hold it instead. |
| **remove** | unused MCPs/creds | simply not provisioned — attack surface that doesn't exist can't be exfiltrated. |

## The one rule to know

**Egress containment builds on brokering.** A contained agent must set `vault_broker: true`: agent-vault is both the credential-injecting proxy and the only egress path allowed by the UID firewall. Here the `lead` is brokered-only (`egress: false`), while the `worker` is brokered and egress-contained.

## Use it

```sh
cp samples/secure-fleet/clem.yaml clem.yaml
clem vault init
# bootstrap agent-vault + the brokered/real secrets — one KEY=value per call
# (clem vault set takes exactly 2 args; see the header comments in clem.yaml):
clem vault set clem-vault AGENT_VAULT_MASTER_PASSWORD=...
clem vault set clem-vault AGENT_VAULT_OWNER_EMAIL=...
clem vault set clem-vault AGENT_VAULT_OWNER_PASSWORD=...
clem vault set anthropic ANTHROPIC_API_KEY=sk-ant-...   # a paid API key — subscription OAuth is single-tool/ToS-limited for fleets
clem vault set discord-lead   DISCORD_TOKEN=...
clem vault set discord-worker DISCORD_TOKEN=...          # the worker references discord-worker
sudo clem provision
```

Verify a brokered agent holds only placeholders, and that injection still reaches upstream — see the verification block in [`docs/threat-model.md`](../../docs/threat-model.md).
