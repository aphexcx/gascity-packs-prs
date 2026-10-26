#!/bin/sh
set -eu

if [ -n "${GC_GCHAT_ADAPTER_ENV:-}" ]; then
    env_file=$GC_GCHAT_ADAPTER_ENV
elif [ -n "${GC_SERVICE_SECRETS_DIR:-}" ]; then
    env_file=$GC_SERVICE_SECRETS_DIR/env
else
    env_file=${HOME:?HOME is not set; set GC_GCHAT_ADAPTER_ENV or GC_SERVICE_SECRETS_DIR}/.config/gc-gchat-adapter/env
fi

if [ ! -f "$env_file" ]; then
    printf '%s\n' "Missing secrets file: $env_file; required keys: GCHAT_PROJECT_ID, GCHAT_PUBSUB_TOPIC, GCHAT_PUBSUB_SUBSCRIPTION, GCHAT_OAUTH_CLIENT_FILE, GC_CITY_NAME, GC_API_BASE_URL" >&2
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

set -a
# shellcheck disable=SC1090
. "$env_file"
set +a

cd "$(dirname "$0")"
if [ ! -x ./gc-gchat-adapter ]; then
    ./build.sh
elif [ -n "$(find . -type f \( -name '*.go' -o -name go.mod -o -name go.sum -o -name build.sh \) -newer ./gc-gchat-adapter -print -quit)" ]; then
    ./build.sh
fi

exec ./gc-gchat-adapter serve
