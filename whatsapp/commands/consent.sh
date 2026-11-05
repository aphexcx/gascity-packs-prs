#!/bin/sh
# gc whatsapp consent <wa_id> --off|--on
#
# The window-keeper's consent record for one owner's phone. Run --off on
# the owner's word the moment it is given: the keeper then raises no event
# for that conversation (no daily update, no close ladder). The wa_id may
# be typed in any form (+55 11 ...); a phone that has not joined is refused.
set -eu
WA_VERB=consent
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

wa_id=""
on=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --on) on=true; shift ;;
    --off) on=false; shift ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    -*) wa_usage_error "unknown flag $1" ;;
    *) [ -z "$wa_id" ] || wa_usage_error "one wa_id only"; wa_id="$1"; shift ;;
  esac
done
[ -n "$wa_id" ] || wa_usage_error "usage: gc whatsapp consent <wa_id> --off|--on"
[ -n "$on" ] || wa_usage_error "--off or --on is required"

wa_call POST /consent "$(jq -n --arg wa "$wa_id" --argjson on "$on" '{wa_id: $wa, on: $on}')"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
# The conversation the adapter matched, never the input as typed.
last4=$(printf '%s' "$WA_BODY" | jq -r '.wa_id_last4 // empty')
[ -n "$last4" ] || wa_die "adapter answered HTTP $WA_STATUS without the matched conversation; nothing confirmed"
if [ "$on" = true ]; then
  echo "Window-keeper ON for ...$last4 (from the next mark)."
else
  echo "Window-keeper OFF for ...$last4: no keeper event will be raised for it."
fi
