# clem sample — Ollama + NVIDIA Nemotron 3 Nano 4B

Runs clem agents against a local **NVIDIA Nemotron 3 Nano 4B** via Ollama. No Anthropic token, no cloud calls. Fits 8 GB MacBook Air.

**Why Nemotron 4B:** most small local models (gemma4:e4b, qwen3.5:4b) emit reasoning text instead of structured tool-call blocks — the agent spins but never calls MCP tools. NVIDIA trained Nemotron explicitly for agentic workflows, and even the 4B size emits proper `tool_use` blocks reliably. Works with both `claude-code` and `opencode` runtimes.

See [`../README.md`](../README.md) for the shared build command and tour/runtime modes.

---

## Prerequisites on the host

```bash
# install ollama from https://ollama.com/download
ollama serve &
ollama pull nemotron-3-nano:4b    # ~2.8 GB
```

Verify:

```bash
curl http://127.0.0.1:11434/api/tags | grep nemotron
```

## Build

From the repo root:

```bash
docker build -f samples/Dockerfile --build-arg SAMPLE=ollama-nemotron-4b -t clem-nemotron .
```

## Runtime quickstart

```bash
podman run -d --rm --name clem-nemotron --systemd=always \
  -p 7681:7681 \
  clem-nemotron /sbin/init
podman exec -it clem-nemotron bash
```

Open <http://localhost:7681/> in a browser to watch the agent's tmux session live (read-only ttyd). Port comes from `web_terminal_port: 7681` in `clem.yaml`.

Inside the container:

```bash
clem vault init
clem vault set discord-lead DISCORD_TOKEN="Bot <lead-bot-token>"
clem vault set github       GH_TOKEN="ghp_..."
clem provision
clem up
clem status
```

No `clem login` step — Ollama has no OAuth. Provider env vars (`ANTHROPIC_BASE_URL=http://host.containers.internal:11434`, `ANTHROPIC_MODEL=nemotron-3-nano:4b`) are written into each agent's `.env` by `clem provision` based on `provider: ollama` in `clem.yaml`.

## Alternative models

Edit `clem.yaml` `model:` to swap:

| Model                       | Size   | Tool use     | Notes                                                  |
|-----------------------------|--------|--------------|--------------------------------------------------------|
| `nemotron-3-nano:4b`        | 2.8 GB | Strong       | Default. NVIDIA agentic training.                     |
| `nemotron-3-nano:4b-bf16`   | 8 GB   | Strong       | Same weights, higher precision.                       |
| `nemotron-3-nano:30b`       | 24 GB  | Strong       | Best quality, needs 32 GB+ RAM.                       |
| `qwen3.6:35b-a3b`           | 23 GB  | Strong       | MoE, 3B active. Slower on CPU than Nemotron 4B.       |
| `qwen3.5:4b`                | 2.6 GB | **Weak**     | Often fails to emit tool_use blocks. Not recommended. |
| `gemma4:e4b`                | 8.6 GB | **Weak**     | Same tool-use problem as qwen3.5:4b.                  |

## Notes

- `TOKEN EXPIRES` in `clem status` will read `missing` — harmless.
- If the agent spins without calling tools: check `model:` is one of the "Strong tool use" rows above.
