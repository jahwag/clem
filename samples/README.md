# clem samples

Self-contained Dockerised setup for trying clem without touching your host.

Samples today:

- [`ollama-nemotron-4b/`](ollama-nemotron-4b/) - **Discord** coordination + local [NVIDIA Nemotron 3 Nano 4B](https://ollama.com/library/nemotron-3-nano) via Ollama. 2.8 GB, fits an 8 GB MacBook Air, emits proper tool_use blocks.
- [`slack-nemotron-4b/`](slack-nemotron-4b/) - **Slack** coordination, same local model. Uses [korotovsky/slack-mcp-server](https://github.com/korotovsky/slack-mcp-server).

## Build

From the repo root:

```bash
docker build -f samples/Dockerfile --build-arg SAMPLE=ollama-nemotron-4b -t clem-nemotron .
```

Substitute `podman build` if that's what you have.

## Run

See the sample's README for full instructions. Both `docker` and `podman` work. Two modes:

- **Tour** - interactive shell; explore `clem init` / `clem vault` without real credentials.
- **Runtime** - systemd-enabled; `clem provision` creates OS users and starts agents. Needs `--privileged` on docker or `--systemd=always` on podman.

## Building your own sample

Any directory under `samples/` with a `clem.yaml` can be built the same way. Drop a new folder in, point `SAMPLE=` at it, rebuild.
