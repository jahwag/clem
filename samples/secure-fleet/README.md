# Sample: the secure fleet

A worked reference for clem's least-privilege security model — the configuration shape behind [`docs/threat-model.md`](../../docs/threat-model.md). A compromised agent here holds **no usable secrets** and can reach **no unapproved network**, both enforced by the kernel and separate OS users rather than the agent's cooperation.

## What it demonstrates

Every credential gets one of four **dispositions**:

| Disposition | In this sample | Mechanism |
|---|---|---|
| **broker** | the `lead` brokers `ANTHROPIC_API_KEY`, `GATEWAY_API_KEY`, `GH_TOKEN` | `vault.backend: agent-vault` + `vault.services` + per-agent `vault_broker`/`brokered_secrets`. agent-vault (separate UID) injects the real value on the agent's own outbound HTTPS; the agent's `.env` holds only placeholders + a scoped inject-only token. |
| **egress firewall** | the `worker` is egress-contained | top-level `egress:` block → per-agent nftables UID firewall forces all egress through a loopback proxy; everything else is kernel-rejected. |
| **sidecar** | noted for `DISCORD_TOKEN` (coming) | the WS gateway token can't be brokered and a stdio MCP would leak it (same UID); a privileged sidecar (`clem-mcp` user, loopback HTTP MCP) will hold it. |
| **remove** | unused MCPs/creds | simply not provisioned — attack surface that doesn't exist can't be exfiltrated. |

## The one rule to know

**Brokering and egress containment are mutually exclusive *per agent*** in this release (a brokered agent's proxy points at agent-vault, which doesn't chain through the egress proxy). So you pick per agent — here the `lead` is brokered (`egress: false`) and the `worker` is egress-contained. Choose the disposition that fits each agent's job.

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
