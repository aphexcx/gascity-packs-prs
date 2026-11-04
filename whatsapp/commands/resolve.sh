#!/bin/sh
# gc whatsapp inbound resolve <rid> --delivered|--dead
#
# The operator's one path out of a TERMINAL HOLD (gc answered pending with
# terminal true: a pasted draft lost its ownership; gc's word is stop
# polling, do not redeliver). Read the envelope first, then decide:
#   --delivered  the mayor did take it: the adapter acks the gateway.
#   --dead       it was not taken: a dead letter under reason held_terminal
#                with the envelope, then the ack.
# Prints the phone's last four digits, the receipt id and the time, never
# the message text, before it acts.
set -eu
WA_VERB="inbound resolve"
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

rid=""
as=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --delivered) as=delivered; shift ;;
    --dead) as=dead; shift ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    -*) wa_usage_error "unknown flag $1" ;;
    *) [ -z "$rid" ] || wa_usage_error "one rid only"; rid="$1"; shift ;;
  esac
done
[ -n "$rid" ] && [ -n "$as" ] || wa_usage_error "usage: gc whatsapp inbound resolve <rid> --delivered|--dead"
case "$rid" in whatsapp:dynasty:*) ;; *) rid="whatsapp:dynasty:$rid" ;; esac

wa_call GET /status
[ "$WA_STATUS" = 200 ] || wa_die "adapter answered HTTP $WA_STATUS to /status"
held=$(printf '%s' "$WA_BODY" | jq -c --arg rid "$rid" '.inbound.held[] | select(.rid == $rid)')
[ -n "$held" ] || wa_die "$rid is not a terminal hold (gc whatsapp status lists the holds)"
echo "Terminal hold $rid: phone ...$(printf '%s' "$held" | jq -r '.wa_id_last4'), receipt $(printf '%s' "$held" | jq -r '.receipt_id'), held since $(printf '%s' "$held" | jq -r '.held_at // "?"' )"
echo "Resolving as $as."

wa_call POST /inbound/resolve "$(jq -n --arg rid "$rid" --arg as "$as" '{rid: $rid, as: $as}')"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
if [ "$as" = dead ]; then
  echo "Dead-lettered under held_terminal; the gateway ack is owed until it confirms (gc whatsapp status)."
else
  echo "Marked delivered; the gateway ack is owed until it confirms (gc whatsapp status)."
fi
