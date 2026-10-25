#!/bin/sh
exec "${GC_PACK_DIR:?gc slack enable-room-launch: missing Gas City pack context}/commands/_cli.sh" enable-room-launch "$@"
