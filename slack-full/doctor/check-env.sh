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

# Optional model credential path. Inspect literal env-file assignments without
# sourcing the file: doctor must never execute it or print credential contents.
ack_key_file="${ACK_MODEL_API_KEY_FILE:-}"
if [ "${ACK_MODEL_API_KEY_FILE+x}" != x ] && [ -f "$env_file" ]; then
  ack_key_file=$(sed -n 's/^\(export \)\{0,1\}ACK_MODEL_API_KEY_FILE=//p' "$env_file" | tail -n 1)
  case "$ack_key_file" in
    \"*\") ack_key_file=${ack_key_file#\"}; ack_key_file=${ack_key_file%\"} ;;
    \'*\') ack_key_file=${ack_key_file#\'}; ack_key_file=${ack_key_file%\'} ;;
  esac
fi
if [ -n "$ack_key_file" ] && { [ ! -f "$ack_key_file" ] || [ ! -r "$ack_key_file" ]; }; then
  echo "ACK_MODEL_API_KEY_FILE must name an existing readable file"
  exit 2
fi

echo "adapter env vars present"
