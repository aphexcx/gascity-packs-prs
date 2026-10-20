#!/bin/sh
exec "${GC_PACK_DIR:?gc slack map-rig: missing Gas City pack context}/commands/_cli.sh" map-rig "$@"
