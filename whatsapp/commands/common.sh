#!/bin/sh
# common.sh — shared by the gc whatsapp verbs (sourced, not a verb itself:
# command discovery walks directories, and this is a file).
#
# The verbs reach the running adapter through gc's /svc/whatsapp reverse
# proxy (the adapter's Unix socket), as wecom's publish.sh does;
# WHATSAPP_ADAPTER_URL overrides the base for local testing.

wa_die() {
  echo "gc whatsapp ${WA_VERB:-}: $*" >&2
  exit "${WA_EXIT:-1}"
}

wa_usage_error() {
  WA_EXIT=2 wa_die "$*"
}

wa_require() {
  command -v "$1" >/dev/null 2>&1 || wa_die "$1 is required"
}

wa_adapter_base() {
  api_base="${GC_API_BASE_URL:-http://127.0.0.1:9443}"
  api_base="${api_base%/}"
  city="${GC_CITY_NAME:-}"
  if [ -z "${WHATSAPP_ADAPTER_URL:-}" ] && [ -z "$city" ]; then
    wa_die "GC_CITY_NAME is not set"
  fi
  printf '%s' "${WHATSAPP_ADAPTER_URL:-${api_base}/v0/city/${city}/svc/whatsapp}"
}

# wa_call METHOD PATH [JSON]: sets WA_STATUS and WA_BODY; never exits on a
# transport failure without saying so.
wa_call() {
  method="$1"
  path="$2"
  data="${3:-}"
  # A jq failure inside "$(...)" yields an empty body without stopping the
  # script: never send a POST without its JSON.
  if [ "$method" = POST ] && [ -z "$data" ]; then
    wa_die "internal error: the request body for $path is empty (jq failed?)"
  fi
  url="$(wa_adapter_base)$path"
  rc=0
  if [ -n "$data" ]; then
    response=$(curl -sS --max-time "${WA_MAX_TIME:-120}" -X "$method" "$url" -H 'Content-Type: application/json' -H 'X-GC-Request: gc-whatsapp' -d "$data" -w '\n%{http_code}') || rc=$?
  else
    response=$(curl -sS --max-time "${WA_MAX_TIME:-120}" -X "$method" "$url" -H 'X-GC-Request: gc-whatsapp' -w '\n%{http_code}') || rc=$?
  fi
  if [ "$rc" -ne 0 ]; then
    wa_die "transport failure (curl exit $rc): is the whatsapp service running? (gc service list)"
  fi
  WA_STATUS=$(printf '%s' "$response" | tail -n 1)
  WA_BODY=$(printf '%s' "$response" | sed '$d')
}

wa_error_of() {
  printf '%s' "$1" | jq -r '.error // empty' 2>/dev/null || true
}

# The env file the adapter sources (run.sh's resolution order), for enroll.
wa_env_file() {
  if [ -n "${GC_WHATSAPP_ADAPTER_ENV:-}" ]; then
    printf '%s' "$GC_WHATSAPP_ADAPTER_ENV"
  elif [ -n "${GC_SERVICE_SECRETS_DIR:-}" ]; then
    printf '%s' "$GC_SERVICE_SECRETS_DIR/env"
  elif [ -n "${GC_CITY_PATH:-}" ]; then
    printf '%s' "$GC_CITY_PATH/.gc/services/whatsapp/secrets/env"
  else
    printf '%s' "$HOME/.config/gc-whatsapp-adapter/env"
  fi
}
