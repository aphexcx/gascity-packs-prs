#!/bin/sh
# Shared by the service, auth and doctor. Other commands never load credentials.
set -eu

# Preserve only settings that the Go loader also accepts from the process.
# Authentication mode and credential keys belong to the env file instead.
gchat_source_env() {
    gchat_override_keys='GCHAT_CHAT_API_BASE GCHAT_EVENTS_API_BASE GCHAT_PUBSUB_ENDPOINT
GCHAT_OAUTH_AUTH_URL GCHAT_OAUTH_TOKEN_URL GCHAT_PEOPLE_API_BASE
GCHAT_MEDIA_DIR GCHAT_UPLOAD_ROOT GCHAT_STATE_FILE
GC_GCHAT_ADAPTER_ENV GC_SERVICE_SECRETS_DIR GC_CITY_PATH GC_SERVICE_SOCKET GC_SERVICE_URL_PREFIX HOME'
    for gchat_key in $gchat_override_keys; do
        # The key is from the fixed list above. Values expand in assignment
        # context, so quotes, newlines and literal $() are never re-evaluated.
        eval "gchat_saved_$gchat_key=\${$gchat_key-}"
    done
    # An absent mode means oauth in Go, regardless of an inherited value.
    unset GCHAT_TOKEN_SOURCE
    set -a
    # shellcheck disable=SC1090
    . "$1"
    set +a
    for gchat_key in $gchat_override_keys; do
        eval "gchat_saved_value=\${gchat_saved_$gchat_key}"
        if [ -n "$gchat_saved_value" ]; then
            export "$gchat_key=$gchat_saved_value"
        fi
        unset "gchat_saved_$gchat_key"
    done
    unset gchat_saved_value gchat_key gchat_override_keys
}

gchat_load_env() {
    # Pack commands get GC_CITY_PATH, while a supervised service gets its
    # secrets directory directly. Resolve both to the same per-city file.
    if [ -z "${GC_GCHAT_ADAPTER_ENV:-}" ] && [ -z "${GC_SERVICE_SECRETS_DIR:-}" ] && [ -n "${GC_CITY_PATH:-}" ]; then
        GC_SERVICE_SECRETS_DIR=$GC_CITY_PATH/.gc/services/gchat/secrets
        export GC_SERVICE_SECRETS_DIR
    fi
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
    env_file=$(cd "$(dirname "$env_file")" && pwd)/$(basename "$env_file")
    if [ -n "${GC_GCHAT_ADAPTER_ENV:-}" ]; then
        GC_GCHAT_ADAPTER_ENV=$env_file
        export GC_GCHAT_ADAPTER_ENV
    elif [ -n "${GC_SERVICE_SECRETS_DIR:-}" ]; then
        GC_SERVICE_SECRETS_DIR=$(dirname "$env_file")
        export GC_SERVICE_SECRETS_DIR
    fi
    gchat_source_env "$env_file"
}

# Print only the selected absolute path; build diagnostics belong on stderr.
# A subshell keeps PATH splitting and glob settings out of the caller.
gchat_adapter_bin() (
    if [ -n "${GC_GCHAT_ADAPTER_BIN:-}" ]; then
        gchat_bin=$GC_GCHAT_ADAPTER_BIN
        if [ ! -f "$gchat_bin" ] || [ ! -r "$gchat_bin" ] || [ ! -x "$gchat_bin" ]; then
            printf '%s\n' "ERROR: GC_GCHAT_ADAPTER_BIN=$gchat_bin is not an executable file" >&2
            return 1
        fi
    else
        gchat_bin=
        # The extra delimiter preserves a final empty entry (current directory).
        gchat_search_path=${PATH-}:
        IFS=:
        set -f
        for gchat_path_dir in $gchat_search_path; do
            gchat_candidate=${gchat_path_dir:-.}/gc-gchat-adapter
            if [ -f "$gchat_candidate" ] && [ -x "$gchat_candidate" ]; then
                gchat_bin=$gchat_candidate
                break
            fi
        done
        if [ -z "$gchat_bin" ]; then
            gchat_build >&2 || return
            gchat_bin=$GC_PACK_DIR/adapter/gc-gchat-adapter
        fi
    fi
    case "$gchat_bin" in
        /*) printf '%s\n' "$gchat_bin" ;;
        *) printf '%s/%s\n' "$(pwd -P)" "$gchat_bin" ;;
    esac
)

gchat_build() {
    (
        cd "$GC_PACK_DIR/adapter" || return
        if [ ! -x ./gc-gchat-adapter ]; then
            ./build.sh
        elif [ -n "$(find .. -maxdepth 1 -name pack.toml -newer ./gc-gchat-adapter -print -quit)" ] ||
             [ -n "$(find . -type f \( -name '*.go' -o -name go.mod -o -name go.sum -o -name build.sh \) -newer ./gc-gchat-adapter -print -quit)" ]; then
            ./build.sh
        fi
    )
}
