#!/bin/sh
# gc wecom publish — send a markdown message to a WeCom chat through the
# running adapter.
#
# The wrapper relays to the wecom adapter through gc's /svc/wecom reverse
# proxy. The adapter holds the bot credentials and pushes over the long
# connection, so no secret is needed in the command environment.
#
# --chat is the WeCom conversation id: the chatid for a group chat, or the
# peer's userid for a DM.
#
# Usage:
#   gc wecom publish --chat <chatid-or-userid> --text "**build** is green"
set -eu

chat=""
text=""
text_file=""

require_value() {
  # $1 = flag name, $2 = arg count remaining (including the flag itself)
  if [ "$2" -lt 2 ]; then
    echo "gc wecom publish: $1 requires a value" >&2
    exit 2
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --chat)      require_value "$1" "$#"; chat="$2"; shift 2 ;;
    --text)      require_value "$1" "$#"; text="$2"; shift 2 ;;
    --text-file) require_value "$1" "$#"; text_file="$2"; shift 2 ;;
    --chat=*)      chat="${1#*=}"; shift ;;
    --text=*)      text="${1#*=}"; shift ;;
    --text-file=*) text_file="${1#*=}"; shift ;;
    -h|--help)
      cat "$(dirname "$0")/publish/help.md"
      exit 0
      ;;
    *)
      echo "gc wecom publish: unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

if [ -z "$chat" ]; then
  echo "gc wecom publish: --chat is required" >&2
  exit 2
fi
if [ -n "$text" ] && [ -n "$text_file" ]; then
  echo "gc wecom publish: use --text or --text-file, not both" >&2
  exit 2
fi
if [ -z "$text" ] && [ -z "$text_file" ]; then
  echo "gc wecom publish: --text or --text-file is required" >&2
  exit 2
fi
if [ -n "$text_file" ] && [ ! -f "$text_file" ]; then
  echo "gc wecom publish: --text-file $text_file not found" >&2
  exit 2
fi

api_base="${GC_API_BASE_URL:-http://127.0.0.1:9443}"
api_base="${api_base%/}"
city="${GC_CITY_NAME:-}"
if [ -z "$city" ]; then
  echo "gc wecom publish: GC_CITY_NAME is not set" >&2
  exit 1
fi

# Resolve the adapter endpoint: gc proxies /svc/wecom/* to the adapter's
# UDS. WECOM_ADAPTER_URL overrides for local testing.
url="${WECOM_ADAPTER_URL:-${api_base}/v0/city/${city}/svc/wecom/publish}"

# Build the request body with jq so chat/text are correctly JSON-escaped
# (text may contain quotes, newlines, etc.). The body reuses the extmsg
# publishRequest wire shape the adapter already serves for gc callbacks.
# --text-file exists because reply text regularly contains characters
# that are unsafe to interpolate into a shell command (apostrophes,
# backticks, code snippets) — agents write the reply to a file instead.
if [ -n "$text_file" ]; then
  body=$(jq -n \
    --arg chat "$chat" \
    --rawfile text "$text_file" \
    '{conversation: {conversation_id: $chat}, text: $text}')
else
  body=$(jq -n \
    --arg chat "$chat" \
    --arg text "$text" \
    '{conversation: {conversation_id: $chat}, text: $text}')
fi

# Capture status and body separately so the adapter's JSON error payload
# reaches the operator instead of being swallowed by `curl -f`.
response=$(curl -sS -X POST "$url" \
  -H 'Content-Type: application/json' \
  -H 'X-GC-Request: gc-wecom' \
  -d "$body" \
  -w '\n%{http_code}')

status=$(printf '%s' "$response" | tail -n 1)
payload=$(printf '%s' "$response" | sed '$d')

if [ "$status" -ge 400 ] 2>/dev/null; then
  echo "gc wecom publish: adapter returned HTTP $status" >&2
  [ -n "$payload" ] && echo "$payload" >&2
  exit 1
fi

[ -n "$payload" ] && echo "$payload"
