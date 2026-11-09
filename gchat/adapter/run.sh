#!/usr/bin/env bash
set -eu

if [ -n "${GC_GCHAT_ADAPTER_ENV:-}" ]; then
    env_file=$GC_GCHAT_ADAPTER_ENV
elif [ -n "${GC_SERVICE_SECRETS_DIR:-}" ]; then
    env_file=$GC_SERVICE_SECRETS_DIR/env
else
    env_file=${HOME:?HOME is not set; set GC_GCHAT_ADAPTER_ENV or GC_SERVICE_SECRETS_DIR}/.config/gc-gchat-adapter/env
fi

if [ ! -f "$env_file" ]; then
    printf '%s\n' "Missing secrets file: $env_file; required keys: GCHAT_PROJECT_ID, GCHAT_PUBSUB_TOPIC, GCHAT_PUBSUB_SUBSCRIPTION, GC_CITY_NAME, GC_API_BASE_URL, and GCHAT_OAUTH_CLIENT_FILE (or GCHAT_TOKEN_SOURCE=keyholder with GCHAT_KEYHOLDER_SOCKET)" >&2
    exit 1
fi

# Resolve relative paths before switching to the adapter directory. Preserve the
# home fallback so the CLI uses the adjacent standalone config.toml.
env_file=$(cd "$(dirname "$env_file")" && pwd)/$(basename "$env_file")
if [ -n "${GC_GCHAT_ADAPTER_ENV:-}" ]; then
    GC_GCHAT_ADAPTER_ENV=$env_file
    export GC_GCHAT_ADAPTER_ENV
elif [ -n "${GC_SERVICE_SECRETS_DIR:-}" ]; then
    GC_SERVICE_SECRETS_DIR=$(dirname "$env_file")
    export GC_SERVICE_SECRETS_DIR
fi

# Use the same supported override list as the auth and doctor launchers.
# shellcheck source-path=SCRIPTDIR source=../commands/runtime.sh
. "$(dirname "$0")/../commands/runtime.sh"
gchat_source_env "$env_file"

# Resolve before changing directories: PATH and an explicit override may be
# relative to the supervisor's working directory. Ignore shell functions.
GC_PACK_DIR=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd -P)
adapter_bin=$(gchat_adapter_bin)
if [ "$adapter_bin" != "$GC_PACK_DIR/adapter/gc-gchat-adapter" ]; then
    adapter_version=$("$adapter_bin" version)
    printf '%s\n' "gc-gchat-adapter run.sh: using $adapter_bin (version $adapter_version)" >&2
    exec "$adapter_bin" serve
fi

cd "$(dirname "$0")"
exec "$adapter_bin" serve
