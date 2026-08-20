#!/usr/bin/env bash
#
# run.sh — start the gc-wecom-adapter in foreground.
#
# Reads secrets from a sourced env file, resolved in this order:
#   1. $GC_WECOM_ADAPTER_ENV (explicit override)
#   2. $GC_SERVICE_SECRETS_DIR/env (per-city; supervised mode — use this
#      when more than one city on the host imports the pack)
#   3. ~/.config/gc-wecom-adapter/env (global; standalone/dev)
#
# Required env keys (in the file):
#   WECOM_BOT_ID            # Bot ID from WeCom console (API mode / Long Connection)
#   WECOM_BOT_SECRET        # Bot Secret from the same page
#   GC_CITY_NAME            # gc city the adapter posts to. No default —
#                           # adapter exits at startup if unset.
#   GC_API_BASE_URL         # gc API base, e.g. http://127.0.0.1:8372 —
#                           # required in supervised (proxy_process) mode;
#                           # standalone dev falls back to 127.0.0.1:9443.
#
# Optional env keys:
#   LISTEN_INTERNAL         # default 127.0.0.1:8790 (localhost-only; /publish)
#   WECOM_WELCOME_TEXT      # optional enter_chat welcome message
#   WECOM_WS_URL            # override long-connection endpoint (private deploys)
#   REGISTER_ON_START       # default true; set false to skip self-registration
#   WECOM_MEDIA_DIR         # durable inbound media store; default
#                           # <city>/.gc/wecom-media/inbound (supervised) or
#                           # ~/city/.gc/wecom-media/inbound (standalone)
#   WECOM_MEDIA_MAX_BYTES   # attachment size cap; default 209715200 (200MB)
#   WECOM_MEDIA_DOWNLOAD_TIMEOUT_MS   # wall-clock download deadline; default 120000
#   WECOM_MEDIA_MAX_CONCURRENT_DOWNLOADS  # global admission bound; default 3
#   WECOM_MEDIA_URL_TTL_MS  # URL lifetime from create_time; default 270000
#   WECOM_MEDIA_QUOTA_BYTES # store quota, saves rejected on breach (no
#                           # auto-deletion — append-only); default 10GiB
#   WECOM_MEDIA_MIN_FREE_BYTES  # min free disk to leave after a save; default 5GiB
#   WECOM_TRANSCRIBE_TIMEOUT_MS       # default 180000
#   WECOM_TRANSCRIBE_MAX_CONCURRENT   # concurrent Scribe calls; default 2
#   WECOM_TRANSCRIBE_LANGUAGE         # pin Scribe language_code; default auto
#   ELEVENLABS_API_KEY      # Scribe key for audio-file transcription; falls
#                           # back to ~/.config/elevenlabs/api-key; unset =
#                           # audio files deliver with a transcription-failed
#                           # note (never dropped)
#   WECOM_OUTBOUND_MEDIA_ROOT  # directory outbound media may be read from;
#                           # REQUIRED for --image/--video publishing — the
#                           # adapter refuses media sends (fail closed)
#                           # when unset; symlinks in media paths rejected
#   WECOM_IMAGE_MAX_BYTES   # outbound image cap; default 10485760 (10MB —
#                           # WeCom's own smart-robot limit; jpg/png/gif)
#   WECOM_VIDEO_MAX_BYTES   # outbound video cap; default 10485760 (10MB; mp4)
#   WECOM_UPLOAD_TIMEOUT_MS # wall-clock bound on one whole chunked media
#                           # upload; default 300000
#   WECOM_UPLOAD_MAX_CONCURRENT  # global outbound-upload admission bound;
#                           # default 2 (buffer memory ≤ this × media cap)
#   WECOM_UPLOAD_MAX_QUEUE  # requests allowed to wait for an upload slot;
#                           # default 8 — beyond it /publish-media 429s

set -euo pipefail

deps_only=false
if [[ "${1:-}" == "--deps-only" ]]; then
  deps_only=true
fi

# Env file resolution order: explicit override, then the per-city secrets
# dir gc scaffolds for this service (so two cities on one host can't source
# each other's credentials and cross-wire their bots), then the legacy
# global path for standalone/dev use.
if [[ -n "${GC_WECOM_ADAPTER_ENV:-}" ]]; then
  env_file="$GC_WECOM_ADAPTER_ENV"
elif [[ -n "${GC_SERVICE_SECRETS_DIR:-}" ]]; then
  # Supervised mode: the per-city secrets file is REQUIRED — falling
  # through to the global file here would let a new city silently start
  # with another city's bot credentials on a multi-city host. Fail fast
  # (the missing-file message below says where to put it) instead.
  env_file="$GC_SERVICE_SECRETS_DIR/env"
else
  env_file="$HOME/.config/gc-wecom-adapter/env"
fi
# --deps-only needs no secrets — it must work on a fresh box before the
# env file exists, since it's the documented pre-warm step.
if [[ "$deps_only" != true && ! -f "$env_file" ]]; then
  cat <<EOF >&2
gc-wecom-adapter: env file not found at $env_file
Create it with at minimum:

  WECOM_BOT_ID=...
  WECOM_BOT_SECRET=...
  GC_CITY_NAME=...
  GC_API_BASE_URL=http://127.0.0.1:8372

(Bot ID + Secret come from the WeCom console: Workspace -> Smart Robot ->
Create Robot -> Manual -> API Mode -> "Use Long Connection". GC_CITY_NAME
and GC_API_BASE_URL are NOT controller-injected — check the city's actual
API port with 'gc status'.)
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
if command -v shasum >/dev/null 2>&1; then
  lock_hash="$(shasum -a 256 pnpm-lock.yaml | cut -d' ' -f1)"
else
  lock_hash="$(sha256sum pnpm-lock.yaml | cut -d' ' -f1)"
fi
if [[ ! -f "$deps_marker" || "$(cat "$deps_marker" 2>/dev/null)" != "$lock_hash" ]]; then
  # pnpm only: the committed lockfile is pnpm-lock.yaml, and an npm
  # fallback would re-resolve ranged dependencies fresh — production
  # behavior changing on a dependency release with no repository change.
  if ! command -v pnpm >/dev/null 2>&1; then
    echo "gc-wecom-adapter: pnpm is required to install dependencies from the committed lockfile (brew install pnpm, or corepack enable pnpm)" >&2
    exit 1
  fi
  pnpm install --prod --prefer-offline --frozen-lockfile --silent
  printf '%s' "$lock_hash" > "$deps_marker"
fi

if [[ "$deps_only" == true ]]; then
  echo "gc-wecom-adapter: dependencies ready"
  exit 0
fi

exec node src/index.js
