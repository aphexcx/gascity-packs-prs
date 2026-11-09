#!/bin/sh
# Shared prebuilt lookup for the CLI wrapper and the binaries doctor.
# A legacy #! wrapper must never win the PATH or pack-local fast path.
slack_binary_is_native() (
  [ -f "$1" ] && [ -r "$1" ] && [ -x "$1" ] &&
    magic=$(head -c 2 "$1") && [ "$magic" != '#!' ]
)

slack_prebuilt_bin() (
  # Arguments: override name, override value, executable name.
  if [ -n "$2" ]; then
    if [ ! -f "$2" ] || [ ! -r "$2" ] || [ ! -x "$2" ]; then
      printf 'ERROR: %s=%s is not an executable file\n' "$1" "$2" >&2
      return 1
    fi
    candidate=$2
  else
    candidate=
    # Preserve empty PATH entries, including a final one, without glob expansion.
    search_path=${PATH-}:
    IFS=:
    set -f
    for path_dir in $search_path; do
      if slack_binary_is_native "${path_dir:-.}/$3"; then
        candidate=${path_dir:-.}/$3
        break
      fi
    done
    [ -n "$candidate" ] || return 0
  fi
  case "$candidate" in
    /*) printf '%s\n' "$candidate" ;;
    *) printf '%s/%s\n' "$(pwd -P)" "$candidate" ;;
  esac
)

slack_cli_prebuilt_bin() {
  slack_prebuilt_bin GC_SLACK_CLI_BIN "${GC_SLACK_CLI_BIN:-}" gc-slack-cli
}

slack_adapter_prebuilt_bin() {
  slack_prebuilt_bin GC_SLACK_ADAPTER_BIN "${GC_SLACK_ADAPTER_BIN:-}" gc-slack-adapter
}
