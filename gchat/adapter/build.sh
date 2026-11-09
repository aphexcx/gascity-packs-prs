#!/usr/bin/env bash
set -eu

cd "$(dirname "$0")"
go_bin=$(type -P go 2>/dev/null || true)
if [ -z "$go_bin" ]; then
    printf '%s\n' 'ERROR: no Go toolchain found on PATH; install gc-gchat-adapter on PATH, set GC_GCHAT_ADAPTER_BIN, or install Go to build from source' >&2
    exit 1
fi
version=$(sed -n 's/^version = "\([^"]*\)".*/\1/p' ../pack.toml)
if [ -z "$version" ]; then
    printf '%s\n' 'ERROR: missing version in gchat/pack.toml' >&2
    exit 1
fi
"$go_bin" build -ldflags "-X main.Version=$version" -o gc-gchat-adapter ./cmd/gc-gchat-adapter
