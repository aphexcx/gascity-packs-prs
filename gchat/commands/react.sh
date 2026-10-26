#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
message='' emoji='' remove=false
while [ "$#" -gt 0 ]; do
    case $1 in
        --message) gchat_value "$@"; message=$2; shift 2 ;;
        --emoji) gchat_value "$@"; emoji=$2; shift 2 ;;
        --remove) remove=true; shift ;;
        -h|--help) printf '%s\n' 'Usage: gc gchat react --message spaces/ID/messages/ID --emoji EMOJI [--remove]'; exit 0 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
[ -n "$message" ] && [ -n "$emoji" ] || gchat_die '--message and --emoji are required'
gchat_init
payload=$(jq -n --arg message "$message" --arg emoji "$emoji" --argjson remove "$remove" '{message:$message, emoji:$emoji, remove:$remove}')
result=$(gchat_post react "$payload")
printf '%s\n' "$result" | jq .
