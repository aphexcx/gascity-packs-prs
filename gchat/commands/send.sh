#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=common.sh
. "$GC_PACK_DIR/commands/common.sh"
mode=$1
shift
session='' conversation='' thread='' body='' body_file='' file='' key='' to=''
top_level=false
body_set=false
while [ "$#" -gt 0 ]; do
    case $1 in
        --session) gchat_value "$@"; session=$2; shift 2 ;;
        --to) gchat_value "$@"; to=$2; shift 2 ;;
        --top-level) top_level=true; shift ;;
        --conversation-id|--space) gchat_value "$@"; conversation=$2; shift 2 ;;
        --thread|--reply-to) gchat_value "$@"; thread=$2; shift 2 ;;
        --body) gchat_value "$@"; body=$2; body_set=true; shift 2 ;;
        --body-file) gchat_value "$@"; body_file=$2; shift 2 ;;
        --file) gchat_value "$@"; file=$2; shift 2 ;;
        --idempotency-key) gchat_value "$@"; key=$2; shift 2 ;;
        -h|--help)
            printf '%s\n' "Usage: gc gchat $mode [--to MESSAGE] [--top-level] [--session ID] [--conversation-id spaces/ID] [--thread spaces/ID/threads/ID] [--body TEXT | --body-file PATH] [--idempotency-key KEY]" \
                'Answer with the command printed by the turn: reply-current requires --to MESSAGE.' \
                'Find MESSAGE on its Message id line or in the gchat: item of its first line.' \
                'publish and upload require --to or --conversation-id; both must agree. Upload also requires --file PATH.' \
                'publish with a space starts a new topic and answers nothing. --top-level sends without a thread.'
            exit 0 ;;
        *) gchat_die "unknown argument: $1" ;;
    esac
done
if [ "$body_set" = true ] && [ -n "$body_file" ]; then gchat_die 'pass --body OR --body-file'; fi
if [ "$mode" != upload ] && [ "$body_set" = false ] && [ -z "$body_file" ]; then gchat_die '--body or --body-file is required'; fi
if [ "$mode" = upload ]; then
    [ -n "$file" ] && [ -f "$file" ] || gchat_die 'upload requires --file pointing to a regular file'
    # Physical path: the service checks the file against its upload root
    # without following symlinks, and a home directory may be one.
    file=$(cd "$(dirname "$file")" && pwd -P)/$(basename "$file")
elif [ -n "$file" ]; then
    gchat_die '--file is only supported by upload'
fi
if [ "$mode" = reply-current ] && [ -z "$to" ]; then
    gchat_die 'reply-current requires --to MESSAGE; use the Message id line or the gchat: item in the first line of the turn'
fi
if [ -z "$to" ] && [ -z "$conversation" ]; then gchat_die '--to or --conversation-id is required'; fi
if [ -n "$body_file" ]; then
    [ -f "$body_file" ] && [ -r "$body_file" ] || gchat_die "body file is not readable: $body_file"
fi
gchat_init
if [ -n "$to" ]; then
    # Validate spelling only. The server derives the space and thread.
    printf '%s' "$to" | jq -Rse '
        def valid_update_time:
            if contains("#updated#") then
                (split("#updated#")[1][:19] + "Z") as $utc |
                (try ($utc | fromdateiso8601 | todateiso8601) catch "") == $utc
            else true end;
        test("^(gchat:)?spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_-]+(\\.[A-Za-z0-9_-]+)*(#deleted|#updated#[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?(Z|[+-](0[0-9]|1[0-9]|2[0-3]):[0-5][0-9])|/reactions/[A-Za-z0-9_-]+#(created|deleted))?\\z") and valid_update_time
    ' >/dev/null ||
        gchat_die '--to must name a Chat message: spaces/SPACE/messages/MESSAGE, or its gchat: item or adapter change id'
fi
session=${session:-${GC_SESSION_ID:-}}
if [ -z "$key" ]; then key=gchat-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); fi
# Print before sending: even a lost HTTP response leaves a key for a safe retry.
printf 'Idempotency key: %s\n' "$key" >&2
gchat_payload() {
    jq -Rs --arg session "$session" --arg conversation "$conversation" --arg thread "$thread" \
        --arg path "$file" --arg key "$key" --arg to "$to" --argjson top_level "$top_level" \
        '{session_id:$session, conversation_id:$conversation, thread:$thread, text:., path:$path, idempotency_key:$key, to:$to, top_level:$top_level}'
}
route=commands/publish
if [ "$mode" = upload ]; then
    route=commands/upload
    # Match the server's 256 KiB/s transfer allowance, with 20 seconds beyond
    # its 25-second command overhead for the proxy response.
    gchat_upload_bytes=$(wc -c < "$file")
    gchat_http_timeout=$((45 + (gchat_upload_bytes + 262143) / 262144))
fi
if [ -n "$body_file" ]; then
    result=$(gchat_payload < "$body_file" | gchat_post_stream "$route")
else
    result=$(printf '%s' "$body" | gchat_payload | gchat_post_stream "$route")
fi
gchat_send_result "$result"
