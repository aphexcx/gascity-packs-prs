#!/bin/sh
# gc whatsapp status
#
# Enrolled or not, the last poll, the entries in flight, the keeper's last
# good window read (and the error while it fails), the window per bound
# conversation (by its last four digits) with the gateway's close time and the
# adapter's own ledger beside it when they differ, the keeper's state,
# terminal holds by rid, restore gaps by conversation, dead letters. No
# secrets, no message text, no full phone numbers.
set -eu
WA_VERB=status
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

case "${1:-}" in -h|--help) sed -n '2,9p' "$0"; exit 0 ;; esac

wa_call GET /status
[ "$WA_STATUS" = 200 ] || wa_die "adapter answered HTTP $WA_STATUS to /status"
printf '%s' "$WA_BODY" | jq -r '
  def t: if . == null then "-" elif type == "number" then (. / 1000 | floor | todate) else (tostring | sub("\\.[0-9]+Z$"; "Z")) end;
  def last4: tostring | gsub("[^0-9]"; "") | .[-4:];
  "Enrolled:      \(if .health.gateway_auth == "ok" then "yes" else .health.gateway_auth end)",
  "Last poll:     \(.inbound.last_poll_at | t)\(if (.inbound.last_poll_error // "") != "" then " (error: \(.inbound.last_poll_error))" else "" end)",
  "Entries:       \(.inbound.entries | to_entries | map("\(.key) \(.value)") | join(", ") | if . == "" then "none" else . end); acks owed \(.inbound.acks_owed)\(if (.inbound.binds_owed // 0) > 0 then " (\(.inbound.binds_owed) held for a keeper bind)" else "" end)",
  "Dead letters:  \(.inbound.dead_letters)",
  "Terminal holds: \(.inbound.held | length)",
  (.inbound.held[] | "  \(.ref)  conversation ...\(.conversation_last4 // .wa_id_last4)  receipt \(.receipt_id)  since \(.held_at | t)  (gc whatsapp inbound resolve <ref> --delivered|--dead)"),
  "Restore gaps:  \(.inbound.gaps | length)",
  (.inbound.gaps[] | "  \(.conversation_id)  journal watermark \(.watermark_sequence) not in the gc store (store head \(.head_sequence // "unknown"))  (gc whatsapp inbound clear-gap <conversation_id>)"),
  # The keeper reads GET /v1/window once a minute; while a read is failing
  # the ladder and the lapse count wait on a fresh clock.
  ((.keeper.window_read // {}) as $w |
    "Window read:   last good \($w.ok_at | t)\(if ($w.error // "") != "" then "; failing since \($w.error_at | t) (\($w.failures // 0) attempt(s)): \($w.error)" else "" end)"),
  "Windows:",
  # The gateway close time (read by the keeper) is the authority; the
  # ledger of the adapter is shown beside it only when the two differ.
  (.ledger.conversations // {}) as $ledger |
  ((.keeper.conversations // {}) | to_entries[]? | select(.value | type == "object") |
    .key as $wa | ($ledger[$wa].window_closes_at) as $mine |
    (if $mine != null and .value.window_closes_at != null and ((.value.window_closes_at | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) * 1000 | floor) != $mine
     then " (adapter ledger \($mine | t))" else "" end) as $differs |
    "  ...\($wa | last4)  closes \(.value.window_closes_at // "-" | t)\($differs)  keeper \(if .value.consent then "ON" else "OFF" end)  update raised \(.value.update_day // "-")  marks \((.value.marks_fired // []) | join(",") | if . == "" then "-" else . end)  lapses \(.value.lapses_7d // 0) (7 d)")'
