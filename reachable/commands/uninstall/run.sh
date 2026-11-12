#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
exec node "$GC_PACK_DIR/reachable.cjs" uninstall "$@"
