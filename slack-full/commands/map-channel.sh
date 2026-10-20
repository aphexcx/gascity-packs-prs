#!/bin/sh
exec "${GC_PACK_DIR:?gc slack map-channel: missing Gas City pack context}/commands/_cli.sh" map-channel "$@"
