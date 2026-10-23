#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
case ${1:-} in
    '') ;;
    --json) [ "$#" -eq 1 ] || gchat_die 'usage: status [--json]' ;;
    -h|--help) printf '%s\n' 'Usage: gc gchat status [--json]'; exit 0 ;;
    *) gchat_die 'usage: status [--json]' ;;
esac
gchat_init
result=$(gchat_get "$gchat_service_url/status")
if [ "${1:-}" = --json ]; then printf '%s\n' "$result" | jq .; exit; fi
printf '%s\n' "$result" | jq -r '
    "Health: \(.health.state) — \(.health.reason // "")",
    "Subscription: \(.subscription.state // "unknown") \(.subscription.name // "")",
    "Expiry: \(.subscription.expiry // "unknown")",
    "Last event: \(.last_event_time // "none")",
    "Backlog: \(.backlog // 0); pending: \(.pending // 0); unresolved: \(.unresolved // 0); unrecorded: \(.unrecorded // 0); quarantine: \(.quarantine // 0)",
    (if .subscription_retry then "Subscription retry: \(.subscription_retry | tojson)" else empty end),
    "Counters:", ((.counters // {}) | to_entries | sort_by(.key)[] | "  \(.key): \(.value)"),
    "Deliveries (only unresolved IDs can be resolved):",
    ((.deliveries // [])[] | "  \(.status) \(.id): \(.reason // "")"),
    "Spaces:", ((.spaces // {}) | to_entries | sort_by(.key)[] |
        "  \(.key): wake=\(.value.wake) backlog=\(.value.backlog_count // ((.value.backlog // []) | length))",
        ((.value.page_token // "" | fromjson?) // {} |
            if (.unavailable // "") != "" then
                "    History held: \(.unavailable); retry at the first inventory refresh after \(.access_retry_after // "one hour"), after an observed rejoin, on restart, or after subscription recovery"
            else empty end)),
    "Retained unavailable records are counted under unavailable:<space>; resolve cannot replay them."
'
