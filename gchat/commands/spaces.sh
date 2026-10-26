#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
space='' wake='' wake_set=false
while [ "$#" -gt 0 ]; do
    case $1 in
        --space|--conversation-id) gchat_value "$@"; space=$2; shift 2 ;;
        --wake) gchat_value "$@"; wake=$2; wake_set=true; shift 2 ;;
        -h|--help) printf '%s\n' 'Usage: gc gchat spaces [--space spaces/ID --wake all|mentions|default]'; exit 0 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
if [ -n "$space" ] || [ "$wake_set" = true ]; then
    [ -n "$space" ] && [ "$wake_set" = true ] || gchat_die '--space and --wake must be supplied together'
    case $wake in all|mentions) ;; default) wake= ;; *) gchat_die '--wake must be all, mentions or default';; esac
fi
gchat_init
if [ "$wake_set" = true ]; then
    payload=$(jq -n --arg id "$space" --arg wake "$wake" '{id:$id, wake:$wake}')
    result=$(gchat_post spaces "$payload")
else
    result=$(gchat_get "$gchat_service_url/spaces")
fi
printf '%s\n' "$result" | jq .
