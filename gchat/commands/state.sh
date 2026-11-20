#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
action=${1:-}
case $action in
    restore)
        [ "$#" -eq 2 ] && [ -f "$2" ] || gchat_die 'state restore requires a backup file' ;;
    reset) [ "$#" -eq 1 ] || gchat_die 'state reset takes no arguments' ;;
    -h|--help) printf '%s\n' 'Usage: gc gchat state restore FILE | reset' 'Restore reads FILE locally and sends its ledger to the running service in recovery-required. For offline recovery, stop the service and use the adapter binary.'; exit 0 ;;
    *) gchat_die 'usage: state restore FILE | reset' ;;
esac
gchat_init
if [ "$action" = restore ]; then
    result=$(jq -nc --rawfile ledger "$2" '{action:"restore",ledger:$ledger}' | gchat_post_stream state)
else
    result=$(gchat_post state '{"action":"reset"}')
fi
printf '%s\n' "$result" | jq .
