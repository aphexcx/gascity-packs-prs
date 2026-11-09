#!/bin/sh
# gc whatsapp hours <conversation_id> --hours "Mon-Fri 09:00-18:00" --tz <IANA zone>
# gc whatsapp hours <conversation_id> --clear [--tz <IANA zone>]
#
# The business's hours for the window-keeper (the daily update two hours
# before the close of business; the close ladder shifted inside the hours).
# A span may cross midnight ("18:00-02:00"); several spans are comma-separated.
# --clear removes the recorded hours (the update then comes at the fixed
# hour, the ladder at any hour); the recorded zone stays unless --tz is given.
set -eu
WA_VERB=hours
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

conversation_id=""
hours=""
tz=""
tz_given=false
clear=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --hours) [ "$#" -ge 2 ] || wa_usage_error "--hours needs a value"; hours="$2"; shift 2 ;;
    --hours=*) hours="${1#*=}"; shift ;;
    --tz) [ "$#" -ge 2 ] || wa_usage_error "--tz needs a value"; tz="$2"; tz_given=true; shift 2 ;;
    --tz=*) tz="${1#*=}"; tz_given=true; shift ;;
    --clear) clear=true; shift ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    -*) wa_usage_error "unknown flag $1" ;;
    *) [ -z "$conversation_id" ] || wa_usage_error "one conversation id only"; conversation_id="$1"; shift ;;
  esac
done
usage='usage: gc whatsapp hours <conversation_id> --hours "Mon-Fri 09:00-18:00" --tz <IANA zone> | --clear [--tz <IANA zone>]'
[ -n "$conversation_id" ] || wa_usage_error "$usage"
# The id goes under both names: conversation_id for this pack's adapter, wa_id
# for an adapter process still running the PR 74 code until its restart after a
# pack roll (gp-g02t). The answer's last four come under either name too.
if [ "$clear" = true ]; then
  [ -z "$hours" ] || wa_usage_error "--clear and --hours are exclusive"
  if [ "$tz_given" = true ]; then
    body=$(jq -n --arg id "$conversation_id" --arg tz "$tz" '{conversation_id: $id, wa_id: $id, hours: "", tz: $tz}')
  else
    body=$(jq -n --arg id "$conversation_id" '{conversation_id: $id, wa_id: $id, hours: ""}')
  fi
else
  [ -n "$hours" ] && [ -n "$tz" ] || wa_usage_error "$usage"
  body=$(jq -n --arg id "$conversation_id" --arg hours "$hours" --arg tz "$tz" '{conversation_id: $id, wa_id: $id, hours: $hours, tz: $tz}')
fi

wa_call POST /hours "$body"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
# The conversation the adapter matched, never the input as typed.
last4=$(printf '%s' "$WA_BODY" | jq -r '.conversation_last4 // .wa_id_last4 // empty')
[ -n "$last4" ] || wa_die "adapter answered HTTP $WA_STATUS without the matched conversation; nothing confirmed"
if [ "$clear" = true ] && [ "$tz_given" = true ]; then
  echo "Hours cleared for ...$last4 (zone ${tz:-cleared})."
elif [ "$clear" = true ]; then
  echo "Hours cleared for ...$last4 (the recorded zone kept)."
else
  echo "Hours recorded for ...$last4: $hours ($tz)."
fi
