# clem samples

Self-contained Dockerised setup for trying clem without touching your host.

Samples today:

- ⭐ [`ollama-gemma4/`](ollama-gemma4/) - **Recommended local sample.** **Discord** coordination + local [Google Gemma 4](https://ollama.com/library/gemma4) (unsloth **QAT** build) via Ollama. Default `unsloth/gemma-4-E4B-it-qat-GGUF:UD-Q4_K_XL` (~5.2 GB, runs 100% on a 10 GB GPU). Native function-calling — verified 5/5 tool calls and an end-to-end "write + run a Python script" in ~18 s. **Requires ollama ≥ 0.30.3.** Published as `ghcr.io/jahwag/clem-sample:latest-ollama`.
- [`anthropic/`](anthropic/) - **Discord** coordination + [Anthropic Claude API](https://www.anthropic.com/). Bring your own API key; no local GPU needed — the zero-setup way to try clem. Published as `ghcr.io/jahwag/clem-sample:latest`.
- [`ollama-mellum2/`](ollama-mellum2/) - **Discord** coordination + local [JetBrains Mellum 2](https://www.jetbrains.com/mellum/) (code specialist, Apache-2.0) via Ollama. MoE, native tool_use, ~16 GB RAM. Published as `ghcr.io/jahwag/clem-sample:latest-mellum2`.
- [`secure-fleet/`](secure-fleet/) - security-hardened multi-agent fleet — egress controls, resource limits, vault isolation. A configuration reference, not a model demo.

## Quick start (prebuilt image)

No clone required. Pull and run the latest published image:

```bash
docker run --rm -it --privileged \
  -e DISCORD_TOKEN=... -e DISCORD_SERVER_ID=... \
  -p 7681:7681 \
  ghcr.io/jahwag/clem-sample:latest
```

Images are published to GHCR on every release tag for `linux/amd64` and `linux/arm64`.

## Build

From the repo root (to customise the sample):

```bash
docker build -f samples/Dockerfile --build-arg SAMPLE=ollama-gemma4 -t clem-gemma4 .
```

Substitute `podman build` if that's what you have.

## Run

See the sample's README for full instructions. Both `docker` and `podman` work. Two modes:

- **Tour** - interactive shell; explore `clem init` / `clem vault` without real credentials.
- **Runtime** - systemd-enabled; `clem provision` creates OS users and starts agents. Needs `--privileged` on docker or `--systemd=always` on podman.

## Building your own sample

Any directory under `samples/` with a `clem.yaml` can be built the same way. Drop a new folder in, point `SAMPLE=` at it, rebuild.
