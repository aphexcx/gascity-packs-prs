#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
action=${1:-}
file=
case $action in
    restore)
        [ "$#" -eq 2 ] && [ -f "$2" ] || gchat_die 'state restore requires a backup file'
        file=$(cd "$(dirname "$2")" && pwd)/$(basename "$2") ;;
    reset) [ "$#" -eq 1 ] || gchat_die 'state reset takes no arguments' ;;
    -h|--help) printf '%s\n' 'Usage: gc gchat state restore FILE | reset' 'Calls the running service in recovery-required; offline recovery uses the adapter binary after stopping the service.'; exit 0 ;;
    *) gchat_die 'usage: state restore FILE | reset' ;;
esac
gchat_init
payload=$(jq -n --arg action "$action" --arg file "$file" '{action:$action, file:$file}')
result=$(gchat_post state "$payload")
printf '%s\n' "$result" | jq .
