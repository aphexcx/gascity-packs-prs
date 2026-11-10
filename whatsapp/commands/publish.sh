#!/bin/sh
# gc whatsapp publish --to <conversation_id> --text "..." | --text-file <path>
# gc whatsapp publish --to <conversation_id> --file <path> [--caption "..." | --caption-file <path>]
# gc whatsapp publish --update --to <conversation_id> --text "..." | --text-file <path>
#
# Free-form text to an owner's chat inside WhatsApp's 24-hour window, or,
# with --update, the one approved utility template (dynasty_update, "Your
# city has an update: {{1}}") outside it. Through gc's /svc/whatsapp proxy
# to the running adapter, which records the outbound transcript in gc.
set -eu
WA_VERB=publish
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

to=""
text=""
text_file=""
file=""
caption=""
caption_file=""
caption_set=false
update=false
idempotency_key=""
session="${GC_SESSION_ID:-}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --to) [ "$#" -ge 2 ] || wa_usage_error "--to needs a value"; to="$2"; shift 2 ;;
    --to=*) to="${1#*=}"; shift ;;
    --text) [ "$#" -ge 2 ] || wa_usage_error "--text needs a value"; text="$2"; shift 2 ;;
    --text=*) text="${1#*=}"; shift ;;
    --text-file) [ "$#" -ge 2 ] || wa_usage_error "--text-file needs a value"; text_file="$2"; shift 2 ;;
    --text-file=*) text_file="${1#*=}"; shift ;;
    --file) [ "$#" -ge 2 ] || wa_usage_error "--file needs a value"; file="$2"; shift 2 ;;
    --file=*) file="${1#*=}"; shift ;;
    --caption) [ "$#" -ge 2 ] || wa_usage_error "--caption needs a value"; caption="$2"; caption_set=true; shift 2 ;;
    --caption=*) caption="${1#*=}"; caption_set=true; shift ;;
    --caption-file) [ "$#" -ge 2 ] || wa_usage_error "--caption-file needs a value"; caption_file="$2"; shift 2 ;;
    --caption-file=*) caption_file="${1#*=}"; shift ;;
    --update) update=true; shift ;;
    --idempotency-key) [ "$#" -ge 2 ] || wa_usage_error "--idempotency-key needs a value"; idempotency_key="$2"; shift 2 ;;
    --idempotency-key=*) idempotency_key="${1#*=}"; shift ;;
    --session) [ "$#" -ge 2 ] || wa_usage_error "--session needs a value"; session="$2"; shift 2 ;;
    -h|--help) cat "$(dirname "$0")/publish/help.md"; exit 0 ;;
    *) wa_usage_error "unknown argument $1" ;;
  esac
done

[ -n "$to" ] || wa_usage_error "--to <conversation_id> is required"
if [ -n "$file" ]; then
  [ -z "$text_file" ] || wa_usage_error "--text-file cannot accompany --file; use --caption-file"
  [ -z "$text" ] || wa_usage_error "--text cannot accompany --file; use --caption"
  [ "$update" = false ] || wa_usage_error "a file cannot use --update; wait for the owner to write"
  if [ "$caption_set" = true ] && [ -n "$caption_file" ]; then wa_usage_error "use --caption or --caption-file, not both"; fi
  # Pass paths, never bytes. The adapter applies the allow-list to file AND caption.
  case "$file" in /*) ;; *) file="$(pwd -P)/$file" ;; esac
  case "$caption_file" in ""|/*) ;; *) caption_file="$(pwd -P)/$caption_file" ;; esac
elif [ "$caption_set" = true ] || [ -n "$caption_file" ]; then
  wa_usage_error "a caption requires --file"
fi
if [ -n "$text" ] && [ -n "$text_file" ]; then wa_usage_error "use --text or --text-file, not both"; fi
if [ -n "$text_file" ]; then
  [ -f "$text_file" ] || wa_usage_error "--text-file $text_file not found"
  text=$(cat "$text_file")
fi
[ -n "$text" ] || [ -n "$file" ] || wa_usage_error "--text, --text-file or --file is required"

if [ -z "$idempotency_key" ]; then
  if command -v uuidgen >/dev/null 2>&1; then
    idempotency_key="whatsapp-cli-$(uuidgen | tr '[:upper:]' '[:lower:]')"
  else
    idempotency_key="whatsapp-cli-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  fi
fi
# The resume key reaches the operator before any network I/O.
echo "gc whatsapp publish: idempotency key $idempotency_key (rerun with --idempotency-key $idempotency_key to resume without duplicating)" >&2

city="${GC_CITY_NAME:-}"
if [ "$update" = true ]; then
  body=$(jq -n --arg to "$to" --arg text "$text" --arg key "$idempotency_key" --arg session "$session" \
    '{to: $to, text: $text, idempotency_key: $key} + (if $session != "" then {session_id: $session} else {} end)')
  wa_call POST /publish-update "$body"
else
  body=$(jq -n --arg city "$city" --arg to "$to" --arg text "$text" --arg key "$idempotency_key" --arg session "$session" \
    --arg file "$file" --arg caption "$caption" --arg caption_file "$caption_file" \
    '{conversation: {scope_id: $city, provider: "whatsapp", account_id: "dynasty", conversation_id: $to, kind: "dm"},
      idempotency_key: $key, metadata: {origin: "cli"}}
     + (if $file != "" then {file: $file} + (if $caption_file != "" then {caption_file: $caption_file} else {caption: $caption} end) else {text: $text} end)
     + (if $session != "" then {session_id: $session} else {} end)')
  wa_call POST /publish "$body"
fi

if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  err=$(wa_error_of "$WA_BODY")
  echo "gc whatsapp publish: adapter answered HTTP $WA_STATUS${err:+: $err}" >&2
  exit 1
fi
delivered=$(printf '%s' "$WA_BODY" | jq -r 'if has("delivered") then .delivered else true end' 2>/dev/null || echo true)
if [ "$delivered" = "false" ]; then
  reason=$(printf '%s' "$WA_BODY" | jq -r '.metadata.reason // .failure_kind // "not delivered"')
  hint=$(printf '%s' "$WA_BODY" | jq -r '.metadata.hint // empty')
  echo "gc whatsapp publish: not delivered ($reason)" >&2
  [ -n "$hint" ] && echo "gc whatsapp publish: $hint" >&2
  exit 1
fi
printf '%s\n' "$WA_BODY"
