#!/usr/bin/env bash
#
# run.sh — start the gc-whatsapp-adapter in the foreground.
#
# Reads its env from the file selected by env-file.sh, also used by
# enrollment and diagnostics.
#
# Required env keys (in the file; never a value in the pack):
#   WHATSAPP_GATEWAY_URL         # https://whatsapp.dynasty.computer
#   WHATSAPP_GATEWAY_CREDENTIAL  # the per-city credential `gc whatsapp enroll` writes
#   GC_CITY_NAME                 # the gc city the adapter posts to
#   GC_API_BASE_URL              # gc API base, e.g. http://127.0.0.1:8372
#
# Optional env keys:
#   WHATSAPP_POLL_MS             # gateway inbox poll interval; default 2000
#   WHATSAPP_WATERMARK_CHECK_MS  # the all-watermark restore read; default 60000
#   WHATSAPP_ROUTE_AGENT         # the default route's agent; default mayor
#   WHATSAPP_KEEPER_UPDATE_HOUR  # the keeper's daily update hour; default 10
#   WHATSAPP_CITY_TZ             # the city's IANA zone; default the host's
#   WHATSAPP_UPDATE_LANGUAGE     # the update template's language; default en_US
#   WHATSAPP_MEDIA_DIR           # inbound media store; default
#                                # <city>/.gc/whatsapp-media/inbound
#   WHATSAPP_MEDIA_MAX_BYTES     # attachment size cap; default 104857600 (100MB)
#   WHATSAPP_MEDIA_QUOTA_BYTES   # store quota, saves rejected on breach; default 10GiB
#   WHATSAPP_MEDIA_MIN_FREE_BYTES  # free disk to leave after a save; default 5GiB
#   WHATSAPP_TRANSCRIBE_TIMEOUT_MS # default 180000
#   ELEVENLABS_API_KEY           # Scribe key for voice notes; falls back to
#                                # ~/.config/elevenlabs/api-key; unset =
#                                # "[voice note saved ...; not transcribed]"
#   GC_CITY_PATH                 # the city root; the state lives under
#                                # <city>/.gc/whatsapp (supervised: gc sets it)

set -euo pipefail

deps_only=false
if [[ "${1:-}" == "--deps-only" ]]; then
  deps_only=true
fi

adapter_dir="$(cd "$(dirname "$0")" && pwd)"
env_file="$(sh "$adapter_dir/env-file.sh")"
cd "$adapter_dir"

if ! command -v node >/dev/null 2>&1; then
  echo "gc-whatsapp-adapter: node not found; install Node.js 20 or newer" >&2
  exit 1
fi
if ! node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)'; then
  echo "gc-whatsapp-adapter: Node.js 20 or newer is required (found $(node --version))" >&2
  exit 1
fi

# The adapter has no runtime dependency (plain fetch, node: builtins), so the
# pre-warm step is the runtime check above; --deps-only needs no env file so
# it works on a fresh box before enrollment.
if [[ "$deps_only" == true ]]; then
  echo "gc-whatsapp-adapter: dependencies ready (node $(node --version); no packages to install)"
  exit 0
fi

if [[ ! -f "$env_file" ]]; then
  cat <<EOF >&2
gc-whatsapp-adapter: env file not found at $env_file
Create it with at minimum:

  WHATSAPP_GATEWAY_URL=https://whatsapp.dynasty.computer
  GC_CITY_NAME=...
  GC_API_BASE_URL=http://127.0.0.1:8372

then run: gc whatsapp enroll --code <enrollment code> --name <city name>
(it writes WHATSAPP_GATEWAY_CREDENTIAL into this file, mode 0600).
EOF
  exit 1
fi

# shellcheck disable=SC1090
set -a; source "$env_file"; set +a

exec node src/index.js
