#!/bin/sh
# gc whatsapp hours <wa_id> --hours "Mon-Fri 09:00-18:00" --tz <IANA zone>
#
# The business's hours for the window-keeper (the daily update two hours
# before the close of business; the close ladder shifted inside the hours).
# A span may cross midnight ("18:00-02:00"); several spans are comma-separated.
set -eu
WA_VERB=hours
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

wa_id=""
hours=""
tz=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --hours) [ "$#" -ge 2 ] || wa_usage_error "--hours needs a value"; hours="$2"; shift 2 ;;
    --hours=*) hours="${1#*=}"; shift ;;
    --tz) [ "$#" -ge 2 ] || wa_usage_error "--tz needs a value"; tz="$2"; shift 2 ;;
    --tz=*) tz="${1#*=}"; shift ;;
    -h|--help) sed -n '2,7p' "$0"; exit 0 ;;
    -*) wa_usage_error "unknown flag $1" ;;
    *) [ -z "$wa_id" ] || wa_usage_error "one wa_id only"; wa_id="$1"; shift ;;
  esac
done
[ -n "$wa_id" ] && [ -n "$hours" ] && [ -n "$tz" ] || wa_usage_error "usage: gc whatsapp hours <wa_id> --hours \"Mon-Fri 09:00-18:00\" --tz <IANA zone>"

wa_call POST /hours "$(jq -n --arg wa "$wa_id" --arg hours "$hours" --arg tz "$tz" '{wa_id: $wa, hours: $hours, tz: $tz}')"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
echo "Hours recorded for ...$(printf '%s' "$wa_id" | tr -cd '0-9' | tail -c 4): $hours ($tz)."
