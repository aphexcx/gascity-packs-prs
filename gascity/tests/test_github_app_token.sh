#!/bin/bash
# Standalone real-flow test; the same Python fixture is discovered by pytest.
set -euo pipefail
gat_test_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
exec python3 "$gat_test_dir/test_github_app_token.py" "$@"
