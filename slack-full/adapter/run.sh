#!/usr/bin/env bash
#
# run.sh — entrypoint for the gc-slack-adapter. This is the pack's
# [[service]] proxy_process command, and it also works run by hand in a
# dev checkout.
#
# WHY THE SERVICE COMMAND IS A SCRIPT, NOT THE BINARY (gp-d7l): the
# adapter binary is a gitignored build artifact, but `gc import install`
# re-materializes the pack cache GIT-ONLY. When the service command
# pointed straight at the binary, every pin bump wiped it and stranded
# the service until a human rebuilt by hand (outages 2026-07-30 and
# 2026-08-05). This script is checked in, so it survives every
# materialization — and when the binary is missing it rebuilds it from
# the sources sitting next to it before exec'ing (self-heal, loud logs,
# idempotent: an existing binary is exec'd directly with no build).
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

env_file="${GC_SLACK_ADAPTER_ENV:-$HOME/.config/gc-slack-adapter/env}"
if [[ -f "$env_file" ]]; then
  # shellcheck disable=SC1090
  set -a; source "$env_file"; set +a
else
  # Not fatal: supervised deployments may inject env another way, and the
  # adapter validates its required keys at startup with a precise error.
  log "WARNING: env file not found at $env_file — continuing with the inherited environment (adapter exits at startup if SLACK_WORKSPACE_ID / SLACK_BOT_TOKEN / SLACK_SIGNING_SECRET / GC_CITY_NAME are unset)"
fi

real_bin="$bin_dir/gc-slack-adapter.real"
legacy_bin="$bin_dir/gc-slack-adapter"

# Fast path (idempotent): a previously built binary — exec, no build.
if [[ -x "$real_bin" ]]; then
  exec "$real_bin" "$@"
fi

# Dev checkout: `go build -o gc-slack-adapter .` output. Guard against the
# legacy env-wrapper shim (starts with "#!"), which execs the missing
# .real right back into this failure — fall through and rebuild instead.
if [[ -x "$legacy_bin" && "$(head -c 2 "$legacy_bin" 2>/dev/null)" != "#!" ]]; then
  exec "$legacy_bin" "$@"
fi

# Self-heal: rebuild the binary from the sources in this directory.
log "binary missing at $real_bin — rebuilding from source (pack cache was likely re-materialized git-only by 'gc import install')"

go_bin=""
if command -v go >/dev/null 2>&1; then
  go_bin="$(command -v go)"
else
  # Supervisor environments (launchd) often carry a minimal PATH.
  for cand in /opt/homebrew/bin/go /usr/local/go/bin/go /usr/local/bin/go "$HOME/go/bin/go"; do
    if [[ -x "$cand" ]]; then go_bin="$cand"; break; fi
  done
fi
if [[ -z "$go_bin" ]]; then
  log "ERROR: no Go toolchain found (checked PATH, /opt/homebrew/bin, /usr/local/go/bin, /usr/local/bin, ~/go/bin)"
  log "manual fix: cd $bin_dir && go build -o gc-slack-adapter.real ."
  exit 1
fi

# Build to a PID-suffixed temp file and mv into place: concurrent
# supervisor restarts each build their own copy, the mv is atomic, and
# nothing ever execs a half-written binary.
tmp_bin="$real_bin.build.$$"
trap 'rm -f "$tmp_bin"' EXIT
log "building with $go_bin ($("$go_bin" version 2>/dev/null || echo 'version unknown'))"
if ! (cd "$bin_dir" && "$go_bin" build -o "$tmp_bin" .); then
  log "ERROR: go build failed (compiler output above) — service cannot start"
  log "manual fix: cd $bin_dir && go build -o gc-slack-adapter.real ."
  exit 1
fi
mv -f "$tmp_bin" "$real_bin"
trap - EXIT
log "rebuilt $real_bin OK — starting adapter"
exec "$real_bin" "$@"
