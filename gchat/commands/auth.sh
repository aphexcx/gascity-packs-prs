#!/bin/sh
set -eu

: "${GC_PACK_DIR:?gc gchat auth: missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=runtime.sh
. "$GC_PACK_DIR/commands/runtime.sh"
gchat_load_env
# With a key holder (GCHAT_TOKEN_SOURCE=keyholder) there is nothing to sign in
# to here: enrollment belongs to the box. boxboxbox-google enroll prints what
# an admin still has to grant and waits for it; --check stays with the binary,
# which asks the key holder for a token.
if [ "${GCHAT_TOKEN_SOURCE:-oauth}" = keyholder ] && [ "${1:-}" != --check ] && command -v boxboxbox-google >/dev/null 2>&1; then
    exec boxboxbox-google enroll "$@"
fi
adapter_bin=$(gchat_adapter_bin)
exec "$adapter_bin" auth "$@"
