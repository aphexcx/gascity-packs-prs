#!/bin/sh
# gc whatsapp invite [--proactive-consent] [--hours "Mon-Fri 09:00-18:00"] [--tz <IANA zone>]
#
# Mints a 6-digit invite code at the gateway (24 hours, one live code per
# city) and prints the two lines the owner is told. The three flags are the
# window-keeper's onboarding fields, recorded against the code and bound to
# the phone when it joins; the keeper stays OFF for that phone without
# --proactive-consent.
set -eu
WA_VERB=invite
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

consent=false
hours=""
tz=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --proactive-consent) consent=true; shift ;;
    --hours) [ "$#" -ge 2 ] || wa_usage_error "--hours needs a value"; hours="$2"; shift 2 ;;
    --hours=*) hours="${1#*=}"; shift ;;
    --tz) [ "$#" -ge 2 ] || wa_usage_error "--tz needs a value"; tz="$2"; shift 2 ;;
    --tz=*) tz="${1#*=}"; shift ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) wa_usage_error "unknown argument $1" ;;
  esac
done
if [ -n "$hours" ] && [ -z "$tz" ]; then wa_usage_error "--hours needs --tz <IANA zone>"; fi

body=$(jq -n --argjson consent "$consent" --arg hours "$hours" --arg tz "$tz" \
  '{proactive_consent: $consent} + (if $hours != "" then {hours: $hours} else {} end) + (if $tz != "" then {tz: $tz} else {} end)')
wa_call POST /invite "$body"
if [ "$WA_STATUS" -ge 400 ] 2>/dev/null; then
  wa_die "adapter answered HTTP $WA_STATUS: $(wa_error_of "$WA_BODY")"
fi
code=$(printf '%s' "$WA_BODY" | jq -r '.code')
# The gateway answers epoch seconds (its contract reading 1).
expires=$(printf '%s' "$WA_BODY" | jq -r '.expires_at | if type == "number" then todate else . end')
echo "Invite code: $code (expires $expires)"
echo
echo "Tell the owner:"
echo "  1. Save the Dynasty WhatsApp number as a contact named \"Dynasty\"."
echo "  2. Text this code to it: $code"
echo
if [ "$consent" = true ]; then
  echo "Window-keeper: ON for this phone once it joins${hours:+ (hours $hours, $tz)}."
else
  echo "Window-keeper: OFF for this phone (no --proactive-consent); gc whatsapp consent <wa_id> --on after the owner agrees."
fi
