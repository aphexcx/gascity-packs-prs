#!/usr/bin/env bash
#
# run.sh — start the gc-wecom-adapter in foreground.
#
# Reads secrets from a sourced env file. Default location:
#   ~/.config/gc-wecom-adapter/env
# Override via GC_WECOM_ADAPTER_ENV.
#
# Required env keys (in the file):
#   WECOM_BOT_ID            # Bot ID from WeCom console (API mode / Long Connection)
#   WECOM_BOT_SECRET        # Bot Secret from the same page
#   GC_CITY_NAME            # gc city the adapter posts to. No default —
#                           # adapter exits at startup if unset.
#
# Optional env keys:
#   LISTEN_INTERNAL         # default 127.0.0.1:8790 (localhost-only; /publish)
#   GC_API_BASE_URL         # default http://127.0.0.1:9443
#   ADAPTER_PROVIDER        # default wecom
#   WECOM_INBOUND_TARGET    # default mayor
#   WECOM_WELCOME_TEXT      # optional enter_chat welcome message
#   WECOM_WS_URL            # override long-connection endpoint (private deploys)
#   REGISTER_ON_START       # default true; set false to skip self-registration

set -euo pipefail

deps_only=false
if [[ "${1:-}" == "--deps-only" ]]; then
  deps_only=true
fi

env_file="${GC_WECOM_ADAPTER_ENV:-$HOME/.config/gc-wecom-adapter/env}"
# --deps-only needs no secrets — it must work on a fresh box before the
# env file exists, since it's the documented pre-warm step.
if [[ "$deps_only" != true && ! -f "$env_file" ]]; then
  cat <<EOF >&2
gc-wecom-adapter: env file not found at $env_file
Create it with at minimum:

  WECOM_BOT_ID=...
  WECOM_BOT_SECRET=...
  GC_CITY_NAME=...

(Bot ID + Secret come from the WeCom console: Workspace -> Smart Robot ->
Create Robot -> Manual -> API Mode -> "Use Long Connection". GC_CITY_NAME
is the gc city to bridge into — the controller does not inject it.)
EOF
  exit 1
fi

if [[ "$deps_only" != true ]]; then
  # shellcheck disable=SC1090
  set -a; source "$env_file"; set +a
fi

adapter_dir="$(cd "$(dirname "$0")" && pwd)"
cd "$adapter_dir"

# The adapter is Node + the official Tencent SDK rather than a pack-shipped
# static binary: Tencent maintains the long-connection protocol in
# @wecom/aibot-node-sdk, so protocol upkeep stays upstream.
#
# Dependency install is gated on a marker written only AFTER a successful
# install — a bare node_modules existence check would let an interrupted
# install (e.g. the supervisor's readiness window killing a cold start)
# permanently skip an incomplete tree. Pre-warm with
# `(cd wecom/adapter && ./run.sh --deps-only)` at pack setup so the first
# supervised start never pays install latency.
deps_marker="node_modules/.gc-deps-ok"
lock_hash="$(shasum -a 256 pnpm-lock.yaml | cut -d' ' -f1)"
if [[ ! -f "$deps_marker" || "$(cat "$deps_marker" 2>/dev/null)" != "$lock_hash" ]]; then
  if command -v pnpm >/dev/null 2>&1; then
    pnpm install --prod --prefer-offline --silent
  else
    npm install --omit=dev --silent
  fi
  printf '%s' "$lock_hash" > "$deps_marker"
fi

if [[ "$deps_only" == true ]]; then
  echo "gc-wecom-adapter: dependencies ready"
  exit 0
fi

exec node src/index.js
