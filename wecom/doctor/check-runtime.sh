#!/bin/sh
set -eu
if ! command -v node >/dev/null 2>&1; then
  echo 'node not found; install Node.js 20 or newer'
  exit 2
fi
if ! node -e 'process.exit(Number(process.versions.node.split(".")[0]) >= 20 ? 0 : 1)'; then
  echo 'Node.js 20 or newer is required'
  exit 2
fi
if ! command -v pnpm >/dev/null 2>&1; then
  echo 'pnpm not found; enable it with corepack enable pnpm'
  exit 2
fi
echo "node $(node --version) and pnpm available"
