#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
space='' thread='' since='' limit=100
while [ "$#" -gt 0 ]; do
    case $1 in
        --space|--conversation-id) gchat_value "$@"; space=$2; shift 2 ;;
        --thread) gchat_value "$@"; thread=$2; shift 2 ;;
        --since) gchat_value "$@"; since=$2; shift 2 ;;
        --limit) gchat_value "$@"; limit=$2; shift 2 ;;
        -h|--help) printf '%s\n' 'Usage: gc gchat read --space spaces/ID [--thread NAME] [--since RFC3339] [--limit 1..1000]'; exit 0 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
[ -n "$space" ] || gchat_die '--space is required'
case $limit in ''|*[!0-9]*) gchat_die '--limit must be 1..1000';; esac
[ "$limit" -ge 1 ] && [ "$limit" -le 1000 ] || gchat_die '--limit must be 1..1000'
gchat_init
result=$(gchat_get "$gchat_service_url/read?space=$(gchat_uri "$space")&thread=$(gchat_uri "$thread")&since=$(gchat_uri "$since")&limit=$limit")
printf '%s\n' "$result" | jq .
