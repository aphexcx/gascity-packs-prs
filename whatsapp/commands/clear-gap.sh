#!/bin/sh
# gc whatsapp inbound clear-gap <wa_id>
#
# A RESTORE GAP stands when the adapter's journal records gc accepting a
# message the gc store no longer holds (a beads store restored from an older
# backup, or rebuilt). While any gap stands, every in-doubt message of every
# conversation takes the marked-redelivery path, never a plain repost.
# Clearing it prints the gap and the store's head, removes the gap, and
# REBASES the conversation's watermark to the store as read now (the
# operator's word is the only hand that lowers a watermark). Clear a gap
# only once you know why the store went back.
set -eu
WA_VERB="inbound clear-gap"
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

wa_id="${1:-}"
case "$wa_id" in
  -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
  ''|-*) wa_usage_error "usage: gc whatsapp inbound clear-gap <wa_id>" ;;
esac

wa_call POST /inbound/clear-gap "$(jq -n --arg wa "$wa_id" '{wa_id: $wa}')"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
printf '%s' "$WA_BODY" | jq -r '
  "Gap cleared for ...\(.gap.conversation_id | tostring | .[-4:]):",
  "  journal watermark: sequence \(.gap.watermark_sequence), accepted \(.gap.watermark_accepted_at // "?")",
  "  gc store head:     sequence \(.gap.head_sequence) at \(.gap.head_created_at // "-") when found",
  (if .head then "  watermark rebased to the store head now: sequence \(.head.sequence) (\(.head.created_at))"
   else "  the store holds no record for the conversation now: watermark removed" end)'
