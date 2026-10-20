#!/bin/sh
exec "${GC_PACK_DIR:?gc slack import-app: missing Gas City pack context}/commands/_cli.sh" import-app "$@"
