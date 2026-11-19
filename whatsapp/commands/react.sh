#!/bin/sh
# A hand reaction, through the adapter and the city's gateway credential.
set -eu
WA_VERB=react
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

to=""
message=""
emoji=""
emoji_set=false
idempotency_key=""
session="${GC_SESSION_ID:-}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --to) [ "$#" -ge 2 ] || wa_usage_error "--to needs a value"; to="$2"; shift 2 ;;
    --to=*) to="${1#*=}"; shift ;;
    --message) [ "$#" -ge 2 ] || wa_usage_error "--message needs a value"; message="$2"; shift 2 ;;
    --message=*) message="${1#*=}"; shift ;;
    --emoji) [ "$#" -ge 2 ] || wa_usage_error "--emoji needs a value"; emoji="$2"; emoji_set=true; shift 2 ;;
    --emoji=*) emoji="${1#*=}"; emoji_set=true; shift ;;
    --idempotency-key) [ "$#" -ge 2 ] || wa_usage_error "--idempotency-key needs a value"; idempotency_key="$2"; shift 2 ;;
    --idempotency-key=*) idempotency_key="${1#*=}"; shift ;;
    --session) [ "$#" -ge 2 ] || wa_usage_error "--session needs a value"; session="$2"; shift 2 ;;
    -h|--help) cat "$(dirname "$0")/react/help.md"; exit 0 ;;
    *) wa_usage_error "unknown argument $1" ;;
  esac
done
[ -n "$to" ] || wa_usage_error "--to <conversation_id> is required"
[ -n "$message" ] || wa_usage_error "--message <message_id> is required"
[ "$emoji_set" = true ] || wa_usage_error '--emoji is required (an empty value removes the reaction)'
if [ -z "$idempotency_key" ]; then
  if command -v uuidgen >/dev/null 2>&1; then
    idempotency_key="whatsapp-cli-$(uuidgen | tr '[:upper:]' '[:lower:]')"
  else
    idempotency_key="whatsapp-cli-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  fi
fi
echo "gc whatsapp react: idempotency key $idempotency_key (rerun with --idempotency-key $idempotency_key to resume without duplicating)" >&2
body=$(jq -n --arg city "${GC_CITY_NAME:-}" --arg to "$to" --arg message "$message" --arg emoji "$emoji" \
  --arg key "$idempotency_key" --arg session "$session" \
  '{conversation: {scope_id: $city, provider: "whatsapp", account_id: "dynasty", conversation_id: $to, kind: "dm"},
    message_id: $message, emoji: $emoji, idempotency_key: $key, metadata: {origin: "cli"}}
   + (if $session != "" then {session_id: $session} else {} end)')
wa_call POST /react "$body"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  err=$(wa_error_of "$WA_BODY")
  echo "gc whatsapp react: adapter answered HTTP $WA_STATUS${err:+: $err}" >&2
  exit 1
fi
delivered=$(printf '%s' "$WA_BODY" | jq -r '.delivered // false')
if [ "$delivered" != true ]; then
  reason=$(printf '%s' "$WA_BODY" | jq -r '.metadata.reason // .failure_kind // "not delivered"')
  hint=$(printf '%s' "$WA_BODY" | jq -r '.metadata.hint // empty')
  echo "gc whatsapp react: not delivered ($reason)" >&2
  [ -n "$hint" ] && echo "gc whatsapp react: $hint" >&2
  exit 1
fi
printf '%s\n' "$WA_BODY"
