#!/bin/bash
# codex-infisical-shim.sh — start a Codex session with an Infisical machine
# identity token in its environment, then exec the real codex.
#
# Built to be a Gas City provider `command`. An agent's `env` map is static and
# `pre_start` runs in its own process, so a provider command wrapper is the one
# city-side place a freshly minted value can enter the session environment.
#
# INSTALL (the city half; this pack does not sync assets/scripts anywhere):
#
#   install -m 0755 path/to/gascity/assets/scripts/codex-infisical-shim.sh \
#     "$CITY/.gc/shims/codex-astra/codex"
#   # optional per-city settings, beside the shim, named <shim>.env:
#   cat > "$CITY/.gc/shims/codex-astra/codex.env" <<'EOF'
#   CODEX_SHIM_PATH_PREPEND=/abs/path/to/city/.gc/shims/toolchain
#   CODEX_SHIM_EXEC=npx -y @openai/codex@0.153.3
#   EOF
#
#   # city.toml — the provider runs the shim; the role agents select it.
#   [providers.codex-astra]
#   base = "builtin:codex"
#   command = "/abs/path/to/city/.gc/shims/codex-astra/codex"
#   resume_command = "/abs/path/to/city/.gc/shims/codex-astra/codex resume {{.SessionKey}}"
#
#   # agents/<role>/agent.toml — one per rig
#   provider = "codex-astra"
#   [env]
#   INFISICAL_PROJECT_ID = "<project id, not a secret>"
#
#   # verify the installed copy is this file (a wake checks the md5):
#   test "$(md5 -q "$CITY/.gc/shims/codex-astra/codex")" = \
#        "$(md5 -q path/to/gascity/assets/scripts/codex-infisical-shim.sh)"
#
# Inputs (environment wins over <shim>.env; the file is plain KEY=VALUE lines,
# never evaluated by a shell; unknown keys are ignored with a WARN):
#   CODEX_SHIM_PATH_PREPEND  colon-separated directories put first on PATH, so
#                            the session and every child (git hooks included)
#                            resolve the city's toolchain wrappers.
#   CODEX_SHIM_EXEC          the command line that runs the real codex (split on
#                            whitespace, no quoting), for a pinned build such as
#                            `npx -y @openai/codex@0.153.3`. Default: `codex`
#                            resolved from PATH with the shim's own directory
#                            removed.
#   HOME                     token.sh is read from $HOME/.config/infisical-agent/.
#
# Contract:
#   * Fail-open token. If INFISICAL_TOKEN is unset and
#     $HOME/.config/infisical-agent/token.sh is readable, the shim sources it
#     (it exports INFISICAL_TOKEN from the universal-auth credentials and unsets
#     the client id/secret again). A missing token.sh is silent; a failing one
#     leaves INFISICAL_TOKEN unset and prints one WARN on stderr. The exec
#     happens either way. The token is never echoed.
#   * A set INFISICAL_TOKEN is kept as is; token.sh is not sourced.
#   * argv reaches the real codex intact, in order, nothing added or dropped.
#   * The shim never execs itself. Every PATH entry that resolves to the shim's
#     own directory is removed before the lookup (the child's PATH is the pruned
#     one), and an exec target that is the shim file itself is refused. No
#     codex left on PATH is an error (exit 127), never a loop.
#   * Nothing else in the environment is changed: PATH (prepend + prune) and
#     INFISICAL_TOKEN are the only writes.
#
# Relation to the copy this was extracted from (citadel, gp-e8r6, 2026-09-10):
# same token block and fail-open semantics; the PATH prepend and the pinned
# `npx` command line moved from hardcoded values into <shim>.env so one file
# installs on any city; the default exec target and the self-exec guard are new;
# the WARN prefix names this script.

self_path=$0
case $self_path in
  */*) ;;
  *) self_path=$(command -v -- "$self_path" 2>/dev/null || printf '%s' "$self_path") ;;
esac
self_dir=$(cd -- "$(dirname -- "$self_path")" 2>/dev/null && pwd -P)
self_file="$self_dir/$(basename -- "$self_path")"

# --- settings: environment, then <shim>.env ------------------------------------
sidecar="$self_file.env"
if [ -r "$sidecar" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in ''|'#'*) continue ;; esac
    case $line in *=*) ;; *) echo "codex-infisical-shim: WARN ignoring line without '=' in $sidecar" >&2; continue ;; esac
    key=${line%%=*}
    val=${line#*=}
    case $val in
      \"*\") val=${val#\"}; val=${val%\"} ;;
      \'*\') val=${val#\'}; val=${val%\'} ;;
    esac
    case $key in
      CODEX_SHIM_PATH_PREPEND) [ -n "${CODEX_SHIM_PATH_PREPEND:-}" ] || CODEX_SHIM_PATH_PREPEND=$val ;;
      CODEX_SHIM_EXEC) [ -n "${CODEX_SHIM_EXEC:-}" ] || CODEX_SHIM_EXEC=$val ;;
      *) echo "codex-infisical-shim: WARN ignoring unknown key $key in $sidecar" >&2 ;;
    esac
  done < "$sidecar"
fi

# --- PATH: prepend the city's toolchain, then remove the shim's own directory ---
if [ -n "${CODEX_SHIM_PATH_PREPEND:-}" ]; then
  PATH="$CODEX_SHIM_PATH_PREPEND:$PATH"
fi
pruned=
old_ifs=$IFS
set -f
IFS=:
for entry in $PATH; do
  phys=$(cd -- "${entry:-.}" 2>/dev/null && pwd -P) || phys=
  if [ -n "$self_dir" ] && [ "$phys" = "$self_dir" ]; then
    continue
  fi
  pruned="${pruned:+$pruned:}$entry"
done
IFS=$old_ifs
set +f
PATH=$pruned
export PATH

# --- Infisical machine identity, fail-open -------------------------------------
token_sh="${HOME:-}/.config/infisical-agent/token.sh"
if [ -z "${INFISICAL_TOKEN:-}" ] && [ -n "${HOME:-}" ] && [ -r "$token_sh" ]; then
  # shellcheck disable=SC1090
  . "$token_sh" 2>/dev/null \
    || { unset INFISICAL_TOKEN; echo "codex-infisical-shim: WARN Infisical machine-identity login failed; INFISICAL_TOKEN unset" >&2; }
fi

# --- exec the real codex --------------------------------------------------------
if [ -n "${CODEX_SHIM_EXEC:-}" ]; then
  set -f
  # shellcheck disable=SC2086
  set -- $CODEX_SHIM_EXEC "$@"
  set +f
else
  set -- codex "$@"
fi
target=$(command -v -- "$1" 2>/dev/null)
if [ -z "$target" ]; then
  echo "codex-infisical-shim: ERROR no '$1' on PATH after removing the shim's directory ($self_dir); set CODEX_SHIM_EXEC or install codex" >&2
  exit 127
fi
if [ "$target" -ef "$self_file" ]; then
  echo "codex-infisical-shim: ERROR '$1' resolves to the shim itself ($self_file); refusing to exec" >&2
  exit 127
fi
exec "$@"
