#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
space='' action='' yes=''
while [ "$#" -gt 0 ]; do
    case $1 in
        --space)
            [ -z "$space" ] || gchat_die '--space must be supplied once'
            gchat_value "$@"; space=$2; shift 2 ;;
        --request|--check)
            [ -z "$action" ] || gchat_die 'choose one: --request, --yes MESSAGE, or --check'
            action=${1#--}; shift ;;
        --yes)
            [ -z "$action" ] || gchat_die 'choose one: --request, --yes MESSAGE, or --check'
            gchat_value "$@"; action=yes; yes=$2; shift 2 ;;
        -h|--help)
            printf '%s\n' 'Usage: gc gchat join --space spaces/ID --request | --space spaces/ID --yes MESSAGE | --check'
            exit 0 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
if [ "$action" = check ]; then
    [ -z "$space" ] || gchat_die '--check takes no room'
else
    [ -n "$space" ] || gchat_die '--space is required'
    case $space in spaces/*) id=${space#spaces/} ;; *) gchat_die 'supply one exact room id as spaces/ID' ;; esac
    case $id in ''|*[!A-Za-z0-9_-]*) gchat_die 'supply one exact room id as spaces/ID; lists and wildcards are not allowed' ;; esac
fi
gchat_init
case $action in
    check) payload='{"check":true}' ;;
    request) payload=$(jq -n --arg space "$space" '{space:$space,request:true}') ;;
    yes) payload=$(jq -n --arg space "$space" --arg yes "$yes" '{space:$space,yes:$yes}') ;;
    *) payload=$(jq -n --arg space "$space" '{space:$space}') ;;
esac
result=$(gchat_post join "$payload")
printf '%s\n' "$result" | jq .
printf '%s' "$result" | jq -e '.result == "joined" or .result == "already_member" or .result == "requested" or .result == "permission_present"' >/dev/null
