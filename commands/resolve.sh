#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
id='' action=''
while [ "$#" -gt 0 ]; do
    case $1 in
        --accept|--replay)
            [ -z "$action" ] || gchat_die 'choose exactly one of --accept and --replay'
            action=${1#--}; shift ;;
        -h|--help) printf '%s\n' 'Usage: gc gchat resolve ID --accept|--replay (unresolved deliveries only)'; exit 0 ;;
        -*) gchat_die "unknown argument: $1" ;;
        *) [ -z "$id" ] || gchat_die 'only one delivery ID is accepted'; id=$1; shift ;;
    esac
done
[ -n "$id" ] && [ -n "$action" ] || gchat_die 'a delivery ID and --accept or --replay are required'
gchat_init
payload=$(jq -n --arg id "$id" --arg action "$action" '{id:$id, action:$action}')
result=$(gchat_post resolve "$payload")
printf '%s\n' "$result" | jq .
