#!/bin/sh
set -eu
# gc doctor reserves exit 2 for a failed check (exit 1 is only a warning).
trap 'if [ "$?" -ne 0 ]; then exit 2; fi' 0
: "${GC_PACK_DIR:?gchat doctor: missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=../../commands/runtime.sh
. "$GC_PACK_DIR/commands/runtime.sh"
# shellcheck source-path=SCRIPTDIR source=../../commands/common.sh
. "$GC_PACK_DIR/commands/common.sh"

gchat_load_env
for key in GCHAT_PROJECT_ID GCHAT_PUBSUB_TOPIC GCHAT_PUBSUB_SUBSCRIPTION GCHAT_OAUTH_CLIENT_FILE GC_CITY_NAME GC_API_BASE_URL; do
    value=$(printenv "$key" || :)
    [ -n "$value" ] || gchat_die "secrets $env_file: missing required key $key"
done
gchat_build
# The binary resolves identity.token_file overrides, checks the token exists,
# and forces a refresh even when its cached access token is still valid.
if check=$("$GC_PACK_DIR/adapter/gc-gchat-adapter" auth --check 2>&1); then
    : # Keep successful auth output out of the first-failure headline.
else
    gchat_die "token check failed: $check"
fi
gchat_init
status=$(gchat_get "$gchat_service_url/status") || gchat_die 'could not read subscription state from /status'
state=$(printf '%s' "$status" | jq -r '.subscription.state // "unknown"')
[ "$state" = ACTIVE ] || gchat_die "subscription is $state; expected ACTIVE (see gc gchat status)"
if [ -n "${GC_CITY_PATH:-}" ]; then cd "$GC_CITY_PATH"; fi
configuration=$("${GC_BIN:-gc}" config show --json) || gchat_die 'gc config show failed'
if ! printf '%s' "$configuration" | jq -e '
    (.config // .) |
    [(.ExtMsg.DefaultRoutes // .extmsg.default_route // [])[] |
     select(((.Provider // .provider // "") | ascii_downcase) == "gchat") |
     select((.Agent // .agent // "") != "")] | length > 0' >/dev/null; then
    gchat_die 'missing provider = "gchat" with an agent under extmsg.default_route; paste city-fragment.toml into city.toml'
fi
printf '%s\n' 'gchat: secrets valid, token ready, subscription ACTIVE, default route configured'
