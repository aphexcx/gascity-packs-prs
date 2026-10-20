#!/bin/sh
set -eu

pack_dir="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)}"

missing=""
for bin in adapter/gc-slack-adapter cli/gc-slack-cli; do
  if [ ! -x "$pack_dir/$bin" ]; then
    missing="$missing $bin"
  fi
done

if [ -n "$missing" ]; then
  echo "pack binaries not built:$missing"
  echo "The CLI builds itself on the next gc slack CLI verb (e.g. gc slack map-channel with no arguments; --help only renders help). Or build in place: (cd adapter && go build -o gc-slack-adapter) and (cd cli && go build -o gc-slack-cli .) — see CONTRIBUTING.md."
  exit 2
fi

echo "adapter and CLI binaries built"
