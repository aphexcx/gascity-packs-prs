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
adapter_bin=${GC_GCHAT_ADAPTER_BIN:-}
if [ -n "$adapter_bin" ]; then
    if [ ! -f "$adapter_bin" ] || [ ! -x "$adapter_bin" ]; then
        printf '%s\n' "ERROR: GC_GCHAT_ADAPTER_BIN=$adapter_bin is not an executable file" >&2
        exit 1
    fi
else
    adapter_bin=$(type -P gc-gchat-adapter 2>/dev/null || true)
fi
if [ -n "$adapter_bin" ]; then
    adapter_bin=$(cd "$(dirname "$adapter_bin")" && pwd)/$(basename "$adapter_bin")
    adapter_version=$("$adapter_bin" version)
    printf '%s\n' "gc-gchat-adapter run.sh: using $adapter_bin (version $adapter_version)" >&2
    exec "$adapter_bin" serve
fi

cd "$(dirname "$0")"
if [ ! -x ./gc-gchat-adapter ]; then
    ./build.sh
elif [ -n "$(find .. -maxdepth 1 -name pack.toml -newer ./gc-gchat-adapter -print -quit)" ] ||
     [ -n "$(find . -type f \( -name '*.go' -o -name go.mod -o -name go.sum -o -name build.sh \) -newer ./gc-gchat-adapter -print -quit)" ]; then
    ./build.sh
fi

exec ./gc-gchat-adapter serve
