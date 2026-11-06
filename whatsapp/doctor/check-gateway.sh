#!/bin/sh
set -eu
if ! command -v node >/dev/null 2>&1; then
  echo 'node not found; install Node.js 20 or newer'
  exit 2
fi
exec node "$(dirname "$0")/check.mjs" gateway
