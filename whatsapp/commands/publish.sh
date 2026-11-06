#!/bin/sh
# gc whatsapp publish --to <conversation_id> --text "..." | --text-file <path>
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
    --update) update=true; shift ;;
    --idempotency-key) [ "$#" -ge 2 ] || wa_usage_error "--idempotency-key needs a value"; idempotency_key="$2"; shift 2 ;;
    --idempotency-key=*) idempotency_key="${1#*=}"; shift ;;
    --session) [ "$#" -ge 2 ] || wa_usage_error "--session needs a value"; session="$2"; shift 2 ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) wa_usage_error "unknown argument $1" ;;
  esac
done

[ -n "$to" ] || wa_usage_error "--to <conversation_id> is required"
if [ -n "$text" ] && [ -n "$text_file" ]; then wa_usage_error "use --text or --text-file, not both"; fi
if [ -n "$text_file" ]; then
  [ -f "$text_file" ] || wa_usage_error "--text-file $text_file not found"
  text=$(cat "$text_file")
fi
[ -n "$text" ] || wa_usage_error "--text or --text-file is required (and must not be empty)"

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
    '{conversation: {scope_id: $city, provider: "whatsapp", account_id: "dynasty", conversation_id: $to, kind: "dm"},
      text: $text, idempotency_key: $key, metadata: {origin: "cli"}}
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
