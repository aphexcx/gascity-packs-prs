#!/bin/sh
# gc whatsapp consent <conversation_id> --off|--on
#
# The window-keeper's consent record for one owner's conversation. Run --off
# on the owner's word the moment it is given: the keeper then raises no
# event for that conversation (no daily update, no close ladder). The
# conversation id is the one gc shows (whatsapp/<conversation_id>: the
# owner's WhatsApp user id, given whole, or for an owner bound before user
# ids its phone number, typed in any form); one that has not joined is
# refused.
set -eu
WA_VERB=consent
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

conversation_id=""
on=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --on) on=true; shift ;;
    --off) on=false; shift ;;
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
    -*) wa_usage_error "unknown flag $1" ;;
    *) [ -z "$conversation_id" ] || wa_usage_error "one conversation id only"; conversation_id="$1"; shift ;;
  esac
done
[ -n "$conversation_id" ] || wa_usage_error "usage: gc whatsapp consent <conversation_id> --off|--on"
[ -n "$on" ] || wa_usage_error "--off or --on is required"

# The id goes under both names: conversation_id for this pack's adapter, wa_id
# for an adapter process still running the PR 74 code until its restart after a
# pack roll (gp-g02t). The answer's last four come under either name too.
wa_call POST /consent "$(jq -n --arg id "$conversation_id" --argjson on "$on" '{conversation_id: $id, wa_id: $id, on: $on}')"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
# The conversation the adapter matched, never the input as typed.
last4=$(printf '%s' "$WA_BODY" | jq -r '.conversation_last4 // .wa_id_last4 // empty')
[ -n "$last4" ] || wa_die "adapter answered HTTP $WA_STATUS without the matched conversation; nothing confirmed"
if [ "$on" = true ]; then
  echo "Window-keeper ON for ...$last4 (from the next mark)."
else
  echo "Window-keeper OFF for ...$last4: no keeper event will be raised for it."
fi
