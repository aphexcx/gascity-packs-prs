#!/bin/sh
set -eu
if ! command -v node >/dev/null 2>&1; then
    echo 'follow-through: install Node.js 20 or newer' >&2
    exit 2
fi
if ! node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)'; then
    echo 'follow-through: Node.js 20 or newer is required' >&2
    exit 2
fi
exec node "$(dirname "$0")/../src/cli.js" "$@"
