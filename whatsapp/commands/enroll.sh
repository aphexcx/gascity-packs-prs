#!/bin/sh
# gc whatsapp enroll --code <enrollment code> --name <city name>
#
# Enrolls this city with the Dynasty WhatsApp gateway ONCE, on an
# enrollment code an operator minted for it, and writes the per-city
# credential the gateway returns into the adapter's env file as
# WHATSAPP_GATEWAY_CREDENTIAL (mode 0600). Prints nothing secret. Restart
# the whatsapp service afterwards so the adapter picks the credential up.
set -eu
WA_VERB=enroll
. "$(dirname "$0")/common.sh"
wa_require curl
wa_require jq

code=""
name=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --code) [ "$#" -ge 2 ] || wa_usage_error "--code needs a value"; code="$2"; shift 2 ;;
    --code=*) code="${1#*=}"; shift ;;
    --name) [ "$#" -ge 2 ] || wa_usage_error "--name needs a value"; name="$2"; shift 2 ;;
    --name=*) name="${1#*=}"; shift ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) wa_usage_error "unknown argument $1" ;;
  esac
done
[ -n "$code" ] && [ -n "$name" ] || wa_usage_error "usage: gc whatsapp enroll --code <enrollment code> --name <city name>"

env_file="$(wa_env_file)"
gateway="${WHATSAPP_GATEWAY_URL:-}"
if [ -z "$gateway" ] && [ -f "$env_file" ]; then
  gateway=$(sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}WHATSAPP_GATEWAY_URL=//p' "$env_file" | tail -n 1 | tr -d "\"'")
fi
gateway="${gateway:-https://whatsapp.dynasty.computer}"
gateway="${gateway%/}"

if [ -f "$env_file" ] && grep -q '^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}WHATSAPP_GATEWAY_CREDENTIAL=..*' "$env_file"; then
  wa_die "$env_file already holds WHATSAPP_GATEWAY_CREDENTIAL; this city is enrolled (ask the operator to revoke it before re-enrolling)"
fi

rc=0
response=$(curl -sS -X POST "$gateway/v1/enroll" -H 'Content-Type: application/json' \
  -d "$(jq -n --arg code "$code" --arg name "$name" '{code: $code, name: $name}')" -w '\n%{http_code}') || rc=$?
[ "$rc" -eq 0 ] || wa_die "transport failure reaching $gateway (curl exit $rc)"
status=$(printf '%s' "$response" | tail -n 1)
payload=$(printf '%s' "$response" | sed '$d')
if [ "$status" != 200 ]; then
  wa_die "the gateway answered HTTP $status: $(printf '%s' "$payload" | jq -r '.error // "enrollment refused"' 2>/dev/null)"
fi
credential=$(printf '%s' "$payload" | jq -r '.credential // empty')
city_id=$(printf '%s' "$payload" | jq -r '.city_id // empty')
[ -n "$credential" ] || wa_die "the gateway answered without a credential"

umask 077
mkdir -p "$(dirname "$env_file")"
tmp="$env_file.tmp.$$"
if [ -f "$env_file" ]; then
  grep -v '^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}WHATSAPP_GATEWAY_CREDENTIAL=' "$env_file" > "$tmp" || true
else
  : > "$tmp"
fi
grep -q '^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}WHATSAPP_GATEWAY_URL=' "$tmp" || printf 'WHATSAPP_GATEWAY_URL=%s\n' "$gateway" >> "$tmp"
printf "WHATSAPP_GATEWAY_CREDENTIAL='%s'\n" "$credential" >> "$tmp"
chmod 600 "$tmp"
mv "$tmp" "$env_file"
echo "Enrolled as city $city_id at $gateway; WHATSAPP_GATEWAY_CREDENTIAL written to $env_file (0600)."
echo "Restart the whatsapp service so the adapter reads it."
