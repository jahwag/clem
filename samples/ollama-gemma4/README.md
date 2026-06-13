# clem sample - Ollama + Google Gemma 4 (unsloth QAT)

Runs clem agents against a local **Google Gemma 4** in unsloth's **QAT** (quantization-aware-training) build via Ollama. No Anthropic token, no cloud calls. Tuned to run **100% on a 10 GB GPU**.

**Default: `unsloth/gemma-4-E4B-it-qat-GGUF:UD-Q4_K_XL`** (~5.2 GB). Verified end-to-end: 5/5 tool calls + 5/5 round-trips, and claude-code wrote *and ran* a Python script in ~18 s. Gemma 4 ships **native function-calling** — every size tool-calls cleanly over the Anthropic-compatible `/v1/messages` endpoint claude-code uses.

See [`../README.md`](../README.md) for the shared build command and tour/runtime modes.

---

## Prerequisites on the host

> **Ollama ≥ 0.30.3 is required.** Gemma 4 introduced the `gemma4` architecture (ollama 0.30.3, 2026-06-03). Older ollama fails with `unknown model architecture: 'gemma4'` — which then misleadingly surfaces as `does not support tools` and manifest `400`s. Check `ollama --version` first.

> **Use the explicit quant tag.** A bare `ollama pull hf.co/unsloth/gemma-4-E4B-it-qat-GGUF` **400s** — the repo holds many GGUFs (MTP drafters, mmproj, Q2/Q4) and ollama can't pick a default. Always append `:UD-Q4_K_XL`.

> **Raise the context window.** Ollama defaults `num_ctx` to 4096, but claude-code's system prompt + tool schemas are ~30-50k tokens; at 4096 it truncates and the agent rambles into the output cap. Set `OLLAMA_CONTEXT_LENGTH=32768` before `ollama serve`. E4B QAT stays 100% on a 10 GB GPU even at `131072`.

```bash
curl -fsSL https://ollama.com/install.sh | sh
ollama --version                    # must be >= 0.30.3
export OLLAMA_CONTEXT_LENGTH=32768  # or 131072 on a 10 GB+ GPU
ollama serve &
ollama pull hf.co/unsloth/gemma-4-E4B-it-qat-GGUF:UD-Q4_K_XL   # ~5.2 GB
```

Verify it tool-calls over claude-code's path:

```bash
curl -s http://127.0.0.1:11434/v1/messages -H 'content-type: application/json' -d '{
  "model":"hf.co/unsloth/gemma-4-E4B-it-qat-GGUF:UD-Q4_K_XL","max_tokens":200,
  "messages":[{"role":"user","content":"What is the weather in Paris?"}],
  "tools":[{"name":"get_weather","description":"Get weather for a city",
    "input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
}' | grep -o '"stop_reason":"[^"]*"'
# expect: "stop_reason":"tool_use"
```

## Build

```bash
docker build -f samples/Dockerfile --build-arg SAMPLE=ollama-gemma4 -t clem-gemma4 .
```

## Runtime quickstart

```bash
podman run -d --rm --name clem-gemma4 --systemd=always -p 7681:7681 clem-gemma4 /sbin/init
podman exec -it clem-gemma4 bash
```

Open <http://localhost:7681/> to watch the agent's tmux session live (read-only ttyd).

Inside the container:

```bash
clem vault init
clem vault set discord-lead DISCORD_TOKEN="Bot <lead-bot-token>"
clem vault set github       GH_TOKEN="ghp_..."
clem provision
clem up
clem status
```

No `clem login` — Ollama has no OAuth. `clem provision` writes `ANTHROPIC_BASE_URL` / `ANTHROPIC_MODEL` into each agent's `.env` from `provider: ollama`.

## Model sizes — measured on an RTX 3080 (10 GB)

All are `hf.co/unsloth/gemma-4-<size>-it-qat-GGUF:UD-Q4_K_XL`. Tool-calling is 5/5 across the board.

| Size  | Disk   | gen tok/s | Placement @ 32k ctx     | Notes                                   |
|-------|--------|-----------|-------------------------|-----------------------------------------|
| E2B   | 3.6 GB | ~128      | 100% GPU                | Smallest/fastest. Fine for chat/ops.    |
| **E4B** | **5.2 GB** | **~104** | **100% GPU (even @128k)** | **Default.** Best quality that still fits.|
| 12B   | 6.9 GB | ~28       | ~17% CPU / 83% GPU      | Spills on a 10 GB card → ~3x slower. Use only with >12 GB VRAM. |

Why QAT: the Q4_0 QAT build is smaller than the regular `gemma4:*` Q4_K_M, so more stays on-GPU. On this card the 12B QAT is **~3x faster than the regular 12B** (28 vs 9 tok/s) purely from less CPU spill.

## Going faster? (you probably can't, here)

- **llama.cpp + MTP** (the unsloth repo's multi-token-prediction drafters): tested — on this GPU it was *slower* (68 vs 90 tok/s) because draft acceptance was only 0.20. Needs llama.cpp directly (`--spec-type draft-mtp`); **ollama can't use MTP**.
- **Vulkan / other backends**: ollama already uses the **CUDA** llama.cpp backend, which beat the prebuilt Vulkan build (104 vs 90 tok/s). CUDA is the fastest path on NVIDIA.
- **Net:** ollama + E4B QAT is already the fastest practical local setup on a 10 GB NVIDIA card. Don't bother with MTP/Vulkan unless you're on different hardware.

For a code-specialised alternative that also tool-calls 5/5, see [`../ollama-mellum2/`](../ollama-mellum2/) (JetBrains Mellum 2).

## Notes

- `TOKEN EXPIRES` in `clem status` reads `missing` — harmless.
- Agent spinning without calling tools? Check `ollama --version` ≥ 0.30.3 and that `OLLAMA_CONTEXT_LENGTH` is set — those are the two usual culprits.
