#!/bin/sh
set -eu

: "${GC_PACK_DIR:?gc gchat auth: missing Gas City pack context}"
# shellcheck source-path=SCRIPTDIR source=runtime.sh
. "$GC_PACK_DIR/commands/runtime.sh"
gchat_load_env
gchat_build
exec "$GC_PACK_DIR/adapter/gc-gchat-adapter" auth "$@"
