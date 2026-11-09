#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
exec sh "$GC_PACK_DIR/commands/send.sh" upload "$@"
