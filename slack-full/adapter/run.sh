#!/usr/bin/env bash
#
# run.sh — entrypoint for the gc-slack-adapter. This is the pack's
# [[service]] proxy_process command, and it also works run by hand in a
# dev checkout.
#
# WHY THE SERVICE COMMAND IS A SCRIPT, NOT THE BINARY: the adapter
# binary is a gitignored build artifact, but `gc import install`
# re-materializes the pack cache GIT-ONLY. When the service command
# points straight at the binary, every pack pin bump wipes it and
# strands the service until a human rebuilds by hand. This script is
# checked in, so it survives every materialization — and when the
# binary is missing it rebuilds it from the sources sitting next to it
# before exec'ing (self-heal, loud logs, idempotent: an existing binary
# is exec'd directly with no build).
#
# Reads secrets from a sourced env file. Default location:
#   ~/.config/gc-slack-adapter/env
# Override via GC_SLACK_ADAPTER_ENV.
#
# Required env keys (in the file):
#   SLACK_WORKSPACE_ID      # T... id, find via Slack admin or auth.test API
#   SLACK_BOT_TOKEN         # xoxb-...
#   SLACK_SIGNING_SECRET    # signing secret from Slack app's Basic Information
#   GC_CITY_NAME            # gc city the adapter posts to (matches
#                           # [workspace].name in city.toml). No default —
#                           # adapter exits at startup if unset.
#
# Optional env keys:
#   LISTEN_PUBLIC           # default :8765 (Funnel exposes this; /slack/events)
#   LISTEN_INTERNAL         # default 127.0.0.1:8766 (localhost-only; /publish)
#   INTERNAL_CALLBACK_URL   # default http://127.0.0.1:8766
#   GC_API_BASE_URL         # default http://127.0.0.1:9443
#   ADAPTER_PROVIDER        # default slack
#   REGISTER_ON_START       # default true; set false to skip self-registration

set -euo pipefail

log() { echo "gc-slack-adapter run.sh: $*" >&2; }

bin_dir="$(cd "$(dirname "$0")" && pwd)"

# ${HOME:-} / ${XDG_CONFIG_HOME:-}: supervisor environments may not set
# HOME, and set -u must not abort startup over a default we only need
# when GC_SLACK_ADAPTER_ENV is unset. Honors XDG_CONFIG_HOME like the
# README's manual-sourcing instructions do.
env_file="${GC_SLACK_ADAPTER_ENV:-${XDG_CONFIG_HOME:-${HOME:-}/.config}/gc-slack-adapter/env}"
if [[ -f "$env_file" ]]; then
  set -a
  # shellcheck source=/dev/null
  source "$env_file"
  set +a
else
  # Not fatal: supervised deployments may inject env another way, and
  # the adapter validates its required keys at startup with a precise
  # error.
  log "WARNING: env file not found at $env_file — continuing with the inherited environment (adapter exits at startup if SLACK_WORKSPACE_ID / SLACK_BOT_TOKEN / GC_CITY_NAME are unset)"
fi

adapter_bin="$bin_dir/gc-slack-adapter"

# Fast path (idempotent): a previously built binary — exec, no build.
if [[ -x "$adapter_bin" ]]; then
  exec "$adapter_bin" "$@"
fi

# Self-heal: rebuild the binary from the sources in this directory.
log "binary missing at $adapter_bin — rebuilding from source (pack cache was likely re-materialized git-only by 'gc import install')"

go_bin=""
if command -v go >/dev/null 2>&1; then
  go_bin="$(command -v go)"
  # command -v can return a relative path (relative PATH entry), which
  # would stop resolving once the build cd's into $bin_dir — pin it to
  # an absolute path now.
  if [[ "$go_bin" != /* ]]; then
    go_bin="$(cd "$(dirname "$go_bin")" && pwd)/$(basename "$go_bin")"
  fi
else
  # Supervisor environments (launchd, systemd) often carry a minimal
  # PATH. ${HOME:-} keeps set -u happy when HOME is unset.
  for cand in /opt/homebrew/bin/go /usr/local/go/bin/go /usr/local/bin/go "${HOME:-}/go/bin/go"; do
    if [[ -x "$cand" ]]; then go_bin="$cand"; break; fi
  done
fi
if [[ -z "$go_bin" ]]; then
  log "ERROR: no Go toolchain found (checked PATH, /opt/homebrew/bin, /usr/local/go/bin, /usr/local/bin, ~/go/bin)"
  log "manual fix: cd $bin_dir && go build -o gc-slack-adapter ."
  exit 1
fi

# Build to a PID-suffixed temp file and mv into place: concurrent
# supervisor restarts each build their own copy, the mv is atomic, and
# nothing ever execs a half-written binary.
tmp_bin="$adapter_bin.build.$$"
trap 'rm -f "$tmp_bin"' EXIT
log "building with $go_bin ($("$go_bin" version 2>/dev/null || echo 'version unknown'))"
if ! (cd "$bin_dir" && "$go_bin" build -o "$tmp_bin" .); then
  log "ERROR: go build failed (compiler output above) — service cannot start"
  log "manual fix: cd $bin_dir && go build -o gc-slack-adapter ."
  exit 1
fi
mv -f "$tmp_bin" "$adapter_bin"
trap - EXIT
log "rebuilt $adapter_bin OK — starting adapter"
exec "$adapter_bin" "$@"
