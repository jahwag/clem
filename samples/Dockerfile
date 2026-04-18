# syntax=docker/dockerfile:1.7

# --- build stage ---------------------------------------------------
FROM docker.io/library/golang:1.22-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/clem .

# --- runtime stage -------------------------------------------------
FROM docker.io/library/ubuntu:24.04

ARG SOPS_VERSION=3.12.2
ARG YQ_VERSION=4.52.5

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8

# Core deps: tmux/git/python3 for runner, age for sops, sudo for provisioning,
# systemd so `clem provision` can install + start services inside the container.
RUN apt-get update && apt-get install -y --no-install-recommends \
        age \
        ca-certificates \
        curl \
        git \
        openssh-client \
        python3 \
        python3-pip \
        sudo \
        systemd \
        systemd-sysv \
        tmux \
    && rm -rf /var/lib/apt/lists/* /var/log/* /var/cache/apt/*

# sops (binary release)
RUN curl -fsSL "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.amd64" \
        -o /usr/local/bin/sops \
    && chmod +x /usr/local/bin/sops

# yq (binary release — needed by clem vault decrypt merge)
RUN curl -fsSL "https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/yq_linux_amd64" \
        -o /usr/local/bin/yq \
    && chmod +x /usr/local/bin/yq

# Discord MCP server (the Bytelope fork supports forum channels)
RUN pip3 install --break-system-packages --ignore-installed \
        "git+https://github.com/Bytelope/mcp-discord.git"

# ttyd (read-only web terminal — optional, enabled when web_terminal_port set)
ARG TTYD_VERSION=1.7.7
RUN curl -fsSL "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.x86_64" \
        -o /usr/local/bin/ttyd \
    && chmod +x /usr/local/bin/ttyd

# Slack MCP server (korotovsky/slack-mcp-server — Go binary, MIT)
ARG SLACK_MCP_VERSION=v1.2.3
RUN curl -fsSL "https://github.com/korotovsky/slack-mcp-server/releases/download/${SLACK_MCP_VERSION}/slack-mcp-server-linux-amd64" \
        -o /usr/local/bin/slack-mcp-server \
    && chmod +x /usr/local/bin/slack-mcp-server

# clem binary from the build stage
COPY --from=builder /out/clem /usr/local/bin/clem

# Which sample config to preseed. Override with:
#   docker build --build-arg SAMPLE=ollama-gemma4-e4b ...
ARG SAMPLE=anthropic

# Sample team config (placeholders — replace before `clem provision`)
WORKDIR /srv/team
COPY samples/${SAMPLE}/clem.yaml ./clem.yaml

# Default: interactive shell. To boot systemd instead (so `clem provision`
# can start services), run with `--entrypoint /sbin/init --privileged`.
# See samples/<sample>/README.md for full instructions.
CMD ["/bin/bash"]
