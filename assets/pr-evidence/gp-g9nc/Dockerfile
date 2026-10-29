FROM golang:1.26.5-bookworm AS go
FROM node:22.23.4-bookworm-slim
COPY --from=go /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash build-essential ca-certificates curl git libicu-dev \
    python3 python3-pytest systemd tmux procps ripgrep jq lsof \
    && rm -rf /var/lib/apt/lists/*
RUN corepack enable pnpm && corepack prepare pnpm@10.18.3 --activate
WORKDIR /proof
