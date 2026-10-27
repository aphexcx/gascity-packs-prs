#!/bin/sh
set -eu

default_env_file=""
if [ -n "${XDG_CONFIG_HOME:-}" ] || [ -n "${HOME:-}" ]; then
  default_env_file="${XDG_CONFIG_HOME:-${HOME:-}/.config}/gc-slack-adapter/env"
fi
env_file="${GC_SLACK_ADAPTER_ENV:-$default_env_file}"

# Match run.sh: an explicit missing file is an error even when ambient
# credentials exist. Never source it here or print credential values.
if [ -n "${GC_SLACK_ADAPTER_ENV:-}" ] && [ ! -f "$env_file" ]; then
  echo "GC_SLACK_ADAPTER_ENV does not name an existing env file"
  exit 2
fi

missing=""
for var in SLACK_WORKSPACE_ID SLACK_BOT_TOKEN SLACK_SIGNING_SECRET GC_CITY_NAME; do
  if eval "[ -n \"\${$var:-}\" ]"; then
    continue
  fi
  if [ -f "$env_file" ] && grep -q "^\(export \)\{0,1\}${var}=." "$env_file"; then
    continue
  fi
  missing="$missing $var"
done

if [ -n "$missing" ]; then
  echo "missing adapter env vars:$missing"
  if [ -n "$env_file" ]; then
    echo "Export them or add them to $env_file (mode 0600) so the adapter's supervisor inherits them — the adapter Fatals at start without them."
  else
    echo "Export them or set GC_SLACK_ADAPTER_ENV to an env file; no HOME or XDG_CONFIG_HOME is set."
  fi
  exit 2
fi

echo "adapter env vars present"
