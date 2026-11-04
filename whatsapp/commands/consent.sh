#!/bin/sh
# gc whatsapp consent <wa_id> --off|--on
#
# The window-keeper's consent record for one owner's phone. Run --off on
# the owner's word the moment it is given: the keeper then raises no event
# for that conversation (no daily update, no close ladder).
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
    -h|--help) sed -n '2,6p' "$0"; exit 0 ;;
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
last4=$(printf '%s' "$wa_id" | tr -cd '0-9' | tail -c 4)
if [ "$on" = true ]; then
  echo "Window-keeper ON for ...$last4 (from the next mark)."
else
  echo "Window-keeper OFF for ...$last4: no keeper event will be raised for it."
fi
