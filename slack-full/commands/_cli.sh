#!/bin/sh
# Pack caches are materialized git-only; build the ignored CLI on first use.
set -eu

log() { printf 'gc-slack-cli _cli.sh: %s\n' "$*" >&2; }

cli_dir="${GC_PACK_DIR:?missing Gas City pack context}/cli"
cli_bin="$cli_dir/gc-slack-cli"

# A legacy env-wrapper script at this path is not the compiled CLI.
# Only trust the fast path when reading the signature succeeds.
if [ -x "$cli_bin" ] && magic="$(head -c 2 "$cli_bin")" && [ "$magic" != '#!' ]; then
  exec "$cli_bin" "$@"
fi

log "binary missing at $cli_bin (or a legacy wrapper) — rebuilding from source (pack cache was likely re-materialized git-only by 'gc import install')"
go_bin="$(command -v go || true)"
if [ -z "$go_bin" ]; then
  log "ERROR: no Go toolchain found on PATH"
  log "manual fix: install Go, then cd \"$cli_dir\" && go build -o gc-slack-cli ."
  exit 1
fi
# Resolve a relative PATH entry before changing to the source directory.
case "$go_bin" in
  /*) ;;
  *) go_bin="$PWD/$go_bin" ;;
esac

if ! (cd "$cli_dir" && "$go_bin" build -o gc-slack-cli .); then
  log "ERROR: go build failed (compiler output above) — CLI cannot start"
  log "manual fix: cd \"$cli_dir\" && go build -o gc-slack-cli ."
  exit 1
fi
log "rebuilt $cli_bin OK — starting CLI"
exec "$cli_bin" "$@"
