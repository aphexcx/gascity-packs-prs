#!/bin/sh
# HTTP helpers only: this file never reads the service secrets.
set -eu

gchat_die() { printf '%s\n' "gc gchat: $*" >&2; exit 1; }
gchat_value() { [ "$#" -ge 2 ] && [ -n "$2" ] || gchat_die "$1 requires a value"; }
gchat_uri() { printf '%s' "$1" | jq -sRr @uri; }
gchat_init() {
    command -v jq >/dev/null 2>&1 || gchat_die 'jq is required'
    command -v curl >/dev/null 2>&1 || gchat_die 'curl is required'
    [ -n "${GC_CITY_NAME:-}" ] || gchat_die 'GC_CITY_NAME is required; run through gc gchat'
    gchat_api=${GC_API_BASE_URL:-http://127.0.0.1:8372}
    gchat_city_url=${gchat_api%/}/v0/city/$(gchat_uri "$GC_CITY_NAME")
    gchat_service_url=$gchat_city_url/svc/gchat
}
gchat_http() {
    # Keep the server's explanation on errors, and never follow a redirect or
    # mistake its response for a successful mutation. No automatic retries.
    gchat_response=$(curl -sS --connect-timeout 5 --max-time "${gchat_http_timeout:-45}" -w '\n%{http_code}' "$@") || return $?
    gchat_newline='
'
    gchat_code=${gchat_response##*"$gchat_newline"}
    gchat_body=${gchat_response%"$gchat_newline"*}
    case $gchat_code in
        2??) printf '%s\n' "$gchat_body" ;;
        *) gchat_die "HTTP $gchat_code: $gchat_body" ;;
    esac
}
gchat_get() { gchat_http "$1"; }
gchat_post() {
    gchat_http -X POST \
        -H 'Content-Type: application/json' -H 'X-GC-Request: gc-gchat' \
        --data-binary "$2" "$gchat_service_url/$1"
}
gchat_post_stream() {
    gchat_http -X POST \
        -H 'Content-Type: application/json' -H 'X-GC-Request: gc-gchat' \
        --data-binary @- "$gchat_service_url/$1"
}
gchat_send_result() {
    printf '%s\n' "$1" | jq .
    if ! printf '%s' "$1" | jq -e '.delivered == true' >/dev/null; then
        gchat_die 'delivery failed; keep the idempotency key and inspect reason, failure_kind and retry_after above'
    fi
    if ! printf '%s' "$1" | jq -e '.recorded == true' >/dev/null; then
        printf '%s\n' 'Delivered, but not recorded in gc; retry recording with the same idempotency key.' >&2
    fi
}
