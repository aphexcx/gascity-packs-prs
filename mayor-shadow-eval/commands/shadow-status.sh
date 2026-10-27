#!/bin/sh
set -eu
umask 077
PACK_DIR=${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}
exec python3 -I "$PACK_DIR/scripts/shadow_status.py" "$@"
