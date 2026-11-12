#!/bin/sh
# The shared env-file resolver for enrollment, startup and diagnostics.
# Keep startup's existing precedence: GC_CITY_PATH locates state, not secrets.
# An explicit service path must never fall through to another city's file,
# even when the selected file does not exist yet.
if [ -n "${GC_WHATSAPP_ADAPTER_ENV:-}" ]; then
  env_file="$GC_WHATSAPP_ADAPTER_ENV"
elif [ -n "${GC_SERVICE_SECRETS_DIR:-}" ]; then
  env_file="$GC_SERVICE_SECRETS_DIR/env"
else
  env_file="$HOME/.config/gc-whatsapp-adapter/env"
fi

# Startup has always sourced relative paths after changing to the adapter
# directory. Anchor them here so every caller reads that same existing file.
case "$env_file" in
  /*) printf '%s' "$env_file" ;;
  *) printf '%s/%s' "$(CDPATH= cd "$(dirname "$0")" && pwd)" "$env_file" ;;
esac
