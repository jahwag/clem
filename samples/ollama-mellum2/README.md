# clem sample - Ollama + JetBrains Mellum 2 (code specialist)

Runs a clem code-agent against a local **JetBrains Mellum 2** via Ollama. No Anthropic token, no cloud calls. Apache-2.0, open weights.

**Why Mellum 2:** it's a Mixture-of-Experts model (64 experts, ~2.5B active of 12B total) tuned by JetBrains for real-world development workflows — code, context, and intent — and the **Instruct** GGUF emits proper `tool_use` blocks reliably (verified 5/5 single-shot tool calls + 5/5 round-trips over the Anthropic-compatible `/v1/messages` path claude-code uses). It's the code-specialist counterpart to the general-purpose [`../ollama-gemma4/`](../ollama-gemma4/) sample.

See [`../README.md`](../README.md) for the shared build command and tour/runtime modes.

---

## Prerequisites on the host

> **Ollama ≥ 0.30.x** for current GGUF tool-calling. Mellum 2 is heavier resident (**~15 GB** — the full expert set loads) than the Gemma 4 sizes; budget **16 GB+ RAM**.

> **Raise the context window.** Ollama defaults `num_ctx` to 4096, but claude-code's system prompt + tool schemas are ~30-50k tokens — at 4096 the agent truncates and misbehaves. Set `OLLAMA_CONTEXT_LENGTH=32768` before `ollama serve` (or bake `PARAMETER num_ctx 32768` into a derived model). See the [Gemma 4 sample's Context window section](../ollama-gemma4/README.md#context-window).

```bash
# install / upgrade ollama from https://ollama.com/download
curl -fsSL https://ollama.com/install.sh | sh
ollama --version
export OLLAMA_CONTEXT_LENGTH=32768   # claude-code's prompt needs more than the 4096 default
ollama serve &
ollama pull hf.co/JetBrains/Mellum2-12B-A2.5B-Instruct-GGUF-Q4_K_M   # ~8.1 GB
```

Verify it tool-calls over claude-code's path:

```bash
curl -s http://127.0.0.1:11434/v1/messages -H 'content-type: application/json' -d '{
  "model":"hf.co/JetBrains/Mellum2-12B-A2.5B-Instruct-GGUF-Q4_K_M","max_tokens":200,
  "messages":[{"role":"user","content":"What is the weather in Paris?"}],
  "tools":[{"name":"get_weather","description":"Get weather for a city",
    "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
}' | grep -o '"stop_reason":"[^"]*"'
# expect: "stop_reason":"tool_use"
```

## Build

From the repo root:

```bash
docker build -f samples/Dockerfile --build-arg SAMPLE=ollama-mellum2 -t clem-mellum2 .
```

## Runtime quickstart

```bash
podman run -d --rm --name clem-mellum2 --systemd=always \
  -p 7681:7681 \
  clem-mellum2 /sbin/init
podman exec -it clem-mellum2 bash
```

Open <http://localhost:7681/> to watch the agent's tmux session live (read-only ttyd).

Inside the container:

```bash
clem vault init
clem vault set discord-lead DISCORD_TOKEN="Bot <bot-token>"
clem vault set github       GH_TOKEN="ghp_..."
clem provision
clem up
clem status
```

No `clem login` step - Ollama has no OAuth. `clem provision` writes `ANTHROPIC_BASE_URL` / `ANTHROPIC_MODEL` into each agent's `.env` from `provider: ollama` in `clem.yaml`.

## Variants

| Model                                                          | Disk   | Notes                                              |
|----------------------------------------------------------------|--------|----------------------------------------------------|
| `…/Mellum2-12B-A2.5B-Instruct-GGUF-Q4_K_M`                     | 8.1 GB | Default. Answers directly, no externalised CoT.    |
| `…/Mellum2-12B-A2.5B-Instruct-GGUF-Q8_0`                       | ~14 GB | Effectively lossless (KLD ~0.004). Needs more RAM. |
| `…/Mellum2-12B-A2.5B-Thinking-GGUF-Q4_K_M`                     | 8.1 GB | Emits a chain-of-thought before answering.         |

Prefix each with `hf.co/JetBrains/`.

## Notes

- `TOKEN EXPIRES` in `clem status` will read `missing` - harmless.
- Mellum 2 is **code-tuned**: it shines on coding questions/reviews and is lighter on general chit-chat. For a general-purpose local agent use [`../ollama-gemma4/`](../ollama-gemma4/).
