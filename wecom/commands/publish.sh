#!/bin/sh
# gc wecom publish — send a markdown message, an image, or a video to a
# WeCom chat through the running adapter.
#
# The wrapper relays to the wecom adapter through gc's /svc/wecom reverse
# proxy. The adapter holds the bot credentials and pushes over the long
# connection, so no secret is needed in the command environment.
#
# --chat is the WeCom conversation id: the chatid for a group chat, or the
# peer's userid for a DM.
#
# Text goes to the adapter's /publish (markdown, chunked at ~3800 UTF-8
# bytes). --image/--video go to /publish-media: the adapter uploads the
# file over the long connection (WeCom's chunked aibot_upload_media
# protocol), sends it as an image/video message, and best-effort records
# the outbound in the gc extmsg transcript via /extmsg/outbound (needs the
# caller's session to own the conversation's binding — the mayor's replies
# do). WeCom accepts images ≤10MB (jpg/jpeg/png/gif) and videos ≤10MB
# (mp4); anything else is rejected with a clear error — convert/downscale
# before sending. --text alongside --image/--video is sent as a follow-up
# markdown message right after the media (WeCom image/video messages have
# no caption field).
#
# Usage:
#   gc wecom publish --chat <chatid-or-userid> --text "**build** is green"
#   gc wecom publish --chat <chatid-or-userid> --image /abs/path/photo.png [--text "caption"]
#   gc wecom publish --chat <chatid-or-userid> --video /abs/path/demo.mp4 [--text "caption"]
set -eu

chat=""
text=""
text_file=""
image=""
video=""
kind=""
session="${GC_SESSION_ID:-}"
idempotency_key=""

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
    --image)     require_value "$1" "$#"; image="$2"; shift 2 ;;
    --video)     require_value "$1" "$#"; video="$2"; shift 2 ;;
    --kind)      require_value "$1" "$#"; kind="$2"; shift 2 ;;
    --session)   require_value "$1" "$#"; session="$2"; shift 2 ;;
    --idempotency-key) require_value "$1" "$#"; idempotency_key="$2"; shift 2 ;;
    --chat=*)      chat="${1#*=}"; shift ;;
    --text=*)      text="${1#*=}"; shift ;;
    --text-file=*) text_file="${1#*=}"; shift ;;
    --image=*)     image="${1#*=}"; shift ;;
    --video=*)     video="${1#*=}"; shift ;;
    --kind=*)      kind="${1#*=}"; shift ;;
    --session=*)   session="${1#*=}"; shift ;;
    --idempotency-key=*) idempotency_key="${1#*=}"; shift ;;
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
if [ -n "$image" ] && [ -n "$video" ]; then
  echo "gc wecom publish: use --image or --video, not both" >&2
  exit 2
fi
if [ -n "$text" ] && [ -n "$text_file" ]; then
  echo "gc wecom publish: use --text or --text-file, not both" >&2
  exit 2
fi
media="$image"
media_kind="image"
if [ -n "$video" ]; then
  media="$video"
  media_kind="video"
fi
if [ -z "$media" ] && [ -z "$text" ] && [ -z "$text_file" ]; then
  echo "gc wecom publish: --text, --text-file, --image, or --video is required" >&2
  exit 2
fi
if [ -n "$text_file" ] && [ ! -f "$text_file" ]; then
  echo "gc wecom publish: --text-file $text_file not found" >&2
  exit 2
fi
if [ -n "$media" ] && [ ! -f "$media" ]; then
  echo "gc wecom publish: --$media_kind $media not found" >&2
  exit 2
fi
if [ -n "$kind" ] && [ "$kind" != "dm" ] && [ "$kind" != "room" ]; then
  echo "gc wecom publish: --kind must be dm or room" >&2
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
# UDS. WECOM_ADAPTER_URL overrides for local testing (points at /publish;
# the media endpoint is derived from it, mirroring slack-full's
# publish-file derivation).
url="${WECOM_ADAPTER_URL:-${api_base}/v0/city/${city}/svc/wecom/publish}"

# Build the request body with jq so chat/text are correctly JSON-escaped
# (text may contain quotes, newlines, etc.). The text body reuses the
# extmsg publishRequest wire shape the adapter already serves for gc
# callbacks; the media body is the adapter's /publish-media shape.
# --text-file exists because reply text regularly contains characters
# that are unsafe to interpolate into a shell command (apostrophes,
# backticks, code snippets) — agents write the reply to a file instead.
if [ -n "$media" ]; then
  # One idempotency key per LOGICAL invocation, generated here — never per
  # HTTP attempt. The adapter latches every delivery stage under this key,
  # so the transport retries below and operator reruns (pass the echoed
  # key back via --idempotency-key) resume the send instead of showing the
  # user the media twice (codex jg-d0xr finding 2).
  if [ -z "$idempotency_key" ]; then
    if command -v uuidgen >/dev/null 2>&1; then
      idempotency_key="wecom-cli-$(uuidgen | tr '[:upper:]' '[:lower:]')"
    else
      idempotency_key="wecom-cli-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
    fi
  fi
  # The adapter reads the file from its own process, so the path must be
  # absolute (its working directory is not the caller's).
  case "$media" in
    /*) ;;
    *) media="$(pwd)/$media" ;;
  esac
  case "$url" in
    */publish) media_url="${url%/publish}/publish-media" ;;
    *)         media_url="${url%/}/publish-media" ;;
  esac
  if [ -n "$text_file" ]; then
    body=$(jq -n \
      --arg chat "$chat" \
      --arg kind "$kind" \
      --arg file "$media" \
      --arg media_kind "$media_kind" \
      --arg session "$session" \
      --arg ikey "$idempotency_key" \
      --rawfile text "$text_file" \
      '{conversation: ({conversation_id: $chat} + (if $kind != "" then {kind: $kind} else {} end)),
        file_path: $file, media_kind: $media_kind, text: $text, idempotency_key: $ikey}
       + (if $session != "" then {session_id: $session} else {} end)')
  else
    body=$(jq -n \
      --arg chat "$chat" \
      --arg kind "$kind" \
      --arg file "$media" \
      --arg media_kind "$media_kind" \
      --arg session "$session" \
      --arg ikey "$idempotency_key" \
      --arg text "$text" \
      '{conversation: ({conversation_id: $chat} + (if $kind != "" then {kind: $kind} else {} end)),
        file_path: $file, media_kind: $media_kind, idempotency_key: $ikey}
       + (if $text != "" then {text: $text} else {} end)
       + (if $session != "" then {session_id: $session} else {} end)')
  fi
  url="$media_url"
elif [ -n "$text_file" ]; then
  body=$(jq -n \
    --arg chat "$chat" \
    --arg ikey "$idempotency_key" \
    --rawfile text "$text_file" \
    '{conversation: {conversation_id: $chat}, text: $text}
     + (if $ikey != "" then {idempotency_key: $ikey} else {} end)')
else
  body=$(jq -n \
    --arg chat "$chat" \
    --arg ikey "$idempotency_key" \
    --arg text "$text" \
    '{conversation: {conversation_id: $chat}, text: $text}
     + (if $ikey != "" then {idempotency_key: $ikey} else {} end)')
fi

# Transport retries are safe ONLY because media requests carry the
# invocation-stable idempotency key generated above — the adapter answers
# a retried key from its latched state instead of re-sending. Text goes
# through gc's own retrying machinery, so no curl retry there.
retry_args=""
if [ -n "$media" ]; then
  retry_args="--retry 2 --retry-connrefused"
fi

# Capture status and body separately so the adapter's JSON error payload
# reaches the operator instead of being swallowed by `curl -f`.
# shellcheck disable=SC2086  # retry_args is deliberately word-split
response=$(curl -sS $retry_args -X POST "$url" \
  -H 'Content-Type: application/json' \
  -H 'X-GC-Request: gc-wecom' \
  -d "$body" \
  -w '\n%{http_code}')

status=$(printf '%s' "$response" | tail -n 1)
payload=$(printf '%s' "$response" | sed '$d')

if [ "$status" -ge 400 ] 2>/dev/null; then
  echo "gc wecom publish: adapter returned HTTP $status" >&2
  [ -n "$payload" ] && echo "$payload" >&2
  # Failed or ambiguous media sends resume under the SAME key — repeating
  # the command with a fresh key would deliver the media a second time.
  if [ -n "$media" ] && [ "$status" -ge 429 ]; then
    echo "gc wecom publish: retry with --idempotency-key $idempotency_key to resume this send without duplicating it" >&2
  fi
  exit 1
fi

[ -n "$payload" ] && echo "$payload"
# Surface a transcript-recording miss without failing the (delivered)
# publish — the adapter reports it in-band on media sends.
note=$(printf '%s' "$payload" | jq -r '.transcript_note // empty' 2>/dev/null || true)
[ -n "$note" ] && echo "gc wecom publish: note: $note" >&2
exit 0
