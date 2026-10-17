#!/bin/sh
set -eu
: "${GC_PACK_DIR:?missing Gas City pack context}"
exec python3 "$GC_PACK_DIR/assets/scripts/worker_provenance.py" "$@"
