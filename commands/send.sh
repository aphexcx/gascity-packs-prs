#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
mode=$1
shift
session='' conversation='' thread='' body='' body_file='' file='' key=''
body_set=false
while [ "$#" -gt 0 ]; do
    case $1 in
        --session) gchat_value "$@"; session=$2; shift 2 ;;
        --conversation-id|--space) gchat_value "$@"; conversation=$2; shift 2 ;;
        --thread|--reply-to) gchat_value "$@"; thread=$2; shift 2 ;;
        --body) gchat_value "$@"; body=$2; body_set=true; shift 2 ;;
        --body-file) gchat_value "$@"; body_file=$2; shift 2 ;;
        --file) gchat_value "$@"; file=$2; shift 2 ;;
        --idempotency-key) gchat_value "$@"; key=$2; shift 2 ;;
        -h|--help)
            printf '%s\n' "Usage: gc gchat $mode [--session ID] [--conversation-id spaces/ID] [--thread spaces/ID/threads/ID] [--body TEXT | --body-file PATH] [--idempotency-key KEY]" \
                'publish and upload require --conversation-id; upload also requires --file PATH.' \
                'reply-current uses the latest inbound conversation, then its active binding; pass --thread to reply in a thread.'
            exit 0 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
if [ "$body_set" = true ] && [ -n "$body_file" ]; then gchat_die 'pass --body OR --body-file'; fi
if [ "$mode" != upload ] && [ "$body_set" = false ] && [ -z "$body_file" ]; then gchat_die '--body or --body-file is required'; fi
if [ "$mode" = upload ]; then
    [ -n "$file" ] && [ -f "$file" ] || gchat_die 'upload requires --file pointing to a regular file'
    file=$(cd "$(dirname "$file")" && pwd)/$(basename "$file")
elif [ -n "$file" ]; then
    gchat_die '--file is only supported by upload'
fi
if [ "$mode" != reply-current ] && [ -z "$conversation" ]; then gchat_die '--conversation-id is required'; fi
gchat_init
gchat_session
if [ -z "$conversation" ]; then gchat_reply_target; fi
if [ -n "$body_file" ]; then
    [ -f "$body_file" ] || gchat_die "body file not found: $body_file"
    text_json=$(jq -Rs . < "$body_file")
else
    text_json=$(jq -n --arg text "$body" '$text')
fi
if [ -z "$key" ]; then key=gchat-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); fi
# Print before sending: even a lost HTTP response leaves a key for a safe retry.
printf 'Idempotency key: %s\n' "$key" >&2
payload=$(jq -n --arg session "$session" --arg conversation "$conversation" --arg thread "$thread" \
    --argjson text "$text_json" --arg path "$file" --arg key "$key" \
    '{session_id:$session, conversation_id:$conversation, thread:$thread, text:$text, path:$path, idempotency_key:$key}')
route=commands/publish
if [ "$mode" = upload ]; then route=commands/upload; fi
result=$(gchat_post "$route" "$payload")
gchat_send_result "$result"
