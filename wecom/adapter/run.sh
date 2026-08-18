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

env_file="${GC_WECOM_ADAPTER_ENV:-$HOME/.config/gc-wecom-adapter/env}"
if [[ ! -f "$env_file" ]]; then
  cat <<EOF >&2
gc-wecom-adapter: env file not found at $env_file
Create it with at minimum:

  WECOM_BOT_ID=...
  WECOM_BOT_SECRET=...

(Bot ID + Secret come from the WeCom console: Workspace -> Smart Robot ->
Create Robot -> Manual -> API Mode -> "Use Long Connection".)
EOF
  exit 1
fi

# shellcheck disable=SC1090
set -a; source "$env_file"; set +a

adapter_dir="$(cd "$(dirname "$0")" && pwd)"
cd "$adapter_dir"

# The adapter is Node + the official Tencent SDK rather than a pack-shipped
# static binary: Tencent maintains the long-connection protocol in
# @wecom/aibot-node-sdk, so protocol upkeep stays upstream. Dependencies
# are fetched on first start (and after lockfile changes).
if [[ ! -d node_modules ]]; then
  if command -v pnpm >/dev/null 2>&1; then
    pnpm install --prod --silent
  else
    npm install --omit=dev --silent
  fi
fi

exec node src/index.js
