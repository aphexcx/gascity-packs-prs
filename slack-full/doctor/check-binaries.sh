#!/bin/sh
set -eu

pack_dir="${GC_PACK_DIR:-$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)}"
# shellcheck source-path=SCRIPTDIR source=../commands/_resolve.sh
. "$pack_dir/commands/_resolve.sh"

missing=""
for kind in adapter cli; do
  bin="$kind/gc-slack-$kind"
  if prebuilt=$("slack_${kind}_prebuilt_bin"); then
    if [ -n "$prebuilt" ]; then
      printf '%s: prebuilt %s\n' "$kind" "$prebuilt"
    elif slack_binary_is_native "$pack_dir/$bin"; then
      printf '%s: local\n' "$kind"
    else
      missing="$missing $bin"
    fi
  else
    # An invalid explicit override is an error, never a local fallback.
    missing="$missing $bin"
  fi
done

if [ -n "$missing" ]; then
  echo "pack binaries not built:$missing"
  echo "The CLI builds itself on the next gc slack CLI verb (e.g. gc slack map-channel with no arguments; --help only renders help). Build the adapter in place with (cd adapter && go build -o gc-slack-adapter); see CONTRIBUTING.md for manual CLI build instructions."
  echo "Without Go, install gc-slack-adapter and gc-slack-cli on PATH, or set GC_SLACK_ADAPTER_BIN and GC_SLACK_CLI_BIN to prebuilt executables."
  exit 2
fi

echo "adapter and CLI binaries available"
