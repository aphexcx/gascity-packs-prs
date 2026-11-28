#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
usage='Usage: gc gchat owners | gc gchat owners --remove users/ID --request | gc gchat owners --remove users/ID --yes MESSAGE | gc gchat owners --add'
# Asked to add, with any other flag, it asks nothing and changes nothing:
# a new owner comes by a code from the owner page alone.
for arg do
    case $arg in
        --add)
            gchat_init
            result=$(gchat_post owners '{"add":true}')
            printf '%s\n' "$result" | jq .
            exit 1 ;;
        -h|--help) printf '%s\n' "$usage"; exit 0 ;;
    esac
done
remove='' action='' yes=''
while [ "$#" -gt 0 ]; do
    case $1 in
        --remove)
            [ -z "$remove" ] || gchat_die '--remove must be supplied once'
            gchat_value "$@"; remove=$2; shift 2 ;;
        --request)
            [ -z "$action" ] || gchat_die 'choose one: --request or --yes MESSAGE'
            action=request; shift ;;
        --yes)
            [ -z "$action" ] || gchat_die 'choose one: --request or --yes MESSAGE'
            gchat_value "$@"; action=yes; yes=$2; shift 2 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
if [ -n "$remove" ]; then
    case $remove in users/*) id=${remove#users/} ;; *) gchat_die 'supply one Chat user id as users/ID' ;; esac
    case $id in ''|*[!0-9]*) gchat_die 'supply one Chat user id as users/ID' ;; esac
    [ -n "$action" ] || gchat_die '--remove takes --request or --yes MESSAGE'
else
    [ -z "$action" ] || gchat_die '--request and --yes name a removal: add --remove users/ID'
fi
gchat_init
case $action in
    request) payload=$(jq -n --arg remove "$remove" '{remove:$remove,request:true}') ;;
    yes) payload=$(jq -n --arg remove "$remove" --arg yes "$yes" '{remove:$remove,yes:$yes}') ;;
    *) payload='{}' ;;
esac
result=$(gchat_post owners "$payload")
printf '%s\n' "$result" | jq .
if [ -z "$action" ]; then
    printf '%s' "$result" | jq -e '.result == "owners" or .result == "no_owner" or .result == "settings_owner"' >/dev/null
else
    printf '%s' "$result" | jq -e '.result == "requested" or .result == "removed"' >/dev/null
fi
