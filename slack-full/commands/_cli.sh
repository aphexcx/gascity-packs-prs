#!/bin/sh
# Pack caches are materialized git-only; build the ignored CLI on first use.
set -eu

log() { printf 'gc-slack-cli _cli.sh: %s\n' "$*" >&2; }

pack_dir="$(CDPATH= cd -- "${GC_PACK_DIR:?missing Gas City pack context}" && pwd -P)"
cli_dir="$pack_dir/cli"
cli_bin="$cli_dir/gc-slack-cli"

manual_fix() {
  log "manual fix: (cd \"$cli_dir\" && tmp=\$(mktemp .gc-slack-cli.build.XXXXXX) && trap 'rm -f \"\$tmp\"' 0 && go build -o \"\$tmp\" . && mv -f \"\$tmp\" gc-slack-cli)"
}

# A legacy env-wrapper script at this path is not the compiled CLI.
# Only trust the fast path when reading the signature succeeds.
if [ -x "$cli_bin" ] && magic="$(head -c 2 "$cli_bin")" && [ "$magic" != '#!' ]; then
  exec "$cli_bin" "$@"
fi

log "binary missing at $cli_bin (or a legacy wrapper) — rebuilding from source (pack cache was likely re-materialized git-only by 'gc import install')"
go_bin="$(command -v go || true)"
if [ -z "$go_bin" ]; then
  log "ERROR: no Go toolchain found on PATH — install Go, then run the manual fix"
  manual_fix
  exit 1
fi
# Resolve a relative PATH entry before changing to the source directory.
case "$go_bin" in
  /*) ;;
  *) go_bin="$PWD/$go_bin" ;;
esac

# Go refuses to overwrite a legacy script. Build to a unique sibling so
# failure preserves the old file and success can replace it atomically.
if ! build_tmp="$(mktemp "$cli_dir/.gc-slack-cli.build.XXXXXX")"; then
  log "ERROR: cannot create temporary CLI output in $cli_dir"
  manual_fix
  exit 1
fi
trap 'rm -f "$build_tmp"' 0
trap 'exit 1' HUP INT TERM
if ! (cd "$cli_dir" && "$go_bin" build -o "$build_tmp" .); then
  log "ERROR: go build failed (compiler output above) — CLI cannot start"
  manual_fix
  exit 1
fi
if ! mv -f "$build_tmp" "$cli_bin"; then
  log "ERROR: cannot replace $cli_bin with the built CLI"
  manual_fix
  exit 1
fi
trap - 0 HUP INT TERM
log "rebuilt $cli_bin OK — starting CLI"
exec "$cli_bin" "$@"
