#!/bin/sh
set -eu

cd "$(dirname "$0")"
go build -ldflags "-X main.Version=$(git -C . describe --always --dirty 2>/dev/null || echo dev)" -o gc-gchat-adapter ./cmd/gc-gchat-adapter
