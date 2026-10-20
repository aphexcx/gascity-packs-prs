#!/bin/sh
exec "${GC_PACK_DIR:?gc slack post-message: missing Gas City pack context}/commands/_cli.sh" post-message "$@"
