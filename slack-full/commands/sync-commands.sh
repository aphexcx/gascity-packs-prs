#!/bin/sh
exec "${GC_PACK_DIR:?gc slack sync-commands: missing Gas City pack context}/commands/_cli.sh" sync-commands "$@"
