#!/bin/sh
# gc whatsapp inbound resolve <ref|rid> --delivered|--dead
#
# The operator's one path out of a TERMINAL HOLD (gc answered pending with
# terminal true: a pasted draft lost its ownership; gc's word is stop
# polling, do not redeliver). Read the envelope first, then decide:
#   --delivered  the mayor did take it: the adapter acks the gateway.
#   --dead       it was not taken: a dead letter under reason held_terminal
#                with the envelope, then the ack.
# Prints the conversation's last four digits, the receipt id and the time, never
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
# A hold is named by its ref (m-<10 hex>, what status and the doctor show)
# or by its rid from the journal; the ref is the rid's sha256 digest.
case "$rid" in
  m-*) ref="$rid" ;;
  *)
    case "$rid" in whatsapp:dynasty:*) ;; *) rid="whatsapp:dynasty:$rid" ;; esac
    if command -v sha256sum >/dev/null 2>&1; then digest=$(printf '%s' "$rid" | sha256sum); else digest=$(printf '%s' "$rid" | shasum -a 256); fi
    ref="m-$(printf '%s' "$digest" | cut -c1-10)"
    ;;
esac

wa_call GET /status
[ "$WA_STATUS" = 200 ] || wa_die "adapter answered HTTP $WA_STATUS to /status"
held=$(printf '%s' "$WA_BODY" | jq -c --arg ref "$ref" '.inbound.held[] | select(.ref == $ref)')
[ -n "$held" ] || wa_die "$ref is not a terminal hold (gc whatsapp status lists the holds)"
echo "Terminal hold $ref: conversation ...$(printf '%s' "$held" | jq -r '.conversation_last4 // .wa_id_last4'), receipt $(printf '%s' "$held" | jq -r '.receipt_id'), held since $(printf '%s' "$held" | jq -r '.held_at // "?"' )"
echo "Resolving as $as."

# jq 1.6 (Debian bookworm) reads a bare as as a keyword: quote the key.
wa_call POST /inbound/resolve "$(jq -n --arg ref "$ref" --arg how "$as" '{rid: $ref, "as": $how}')"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
if [ "$as" = dead ]; then
  echo "Dead-lettered under held_terminal; the gateway ack is owed until it confirms (gc whatsapp status)."
else
  echo "Marked delivered; the gateway ack is owed until it confirms (gc whatsapp status)."
fi
