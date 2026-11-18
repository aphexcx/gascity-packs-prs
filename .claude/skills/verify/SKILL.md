---
name: verify
description: "Run before every commit: the exact test, validate and Go module commands for this repository"
---

# Verify a pack change

Start each block in the repository root. Use Python 3.12, as CI does. Select an existing environment where `python3` imports pytest, PyYAML and jsonschema. The Makefile probes those imports and otherwise calls uv to add all three packages. On a no-install worker, select the provisioned interpreter on PATH before running make. Reachable and follow-through also require Node. Gastown's shell tests require Bash and ripgrep.

Worker sessions can export a template, a real gc executable and adapter feature switches. Clear `GC_TEMPLATE` so the claim tests select their fixture route, `GC_BIN` so adapter tests use their fake CLI, and `NEVER_SILENT` so tests control their own feature switch. Leave `GC_TEST_BIN` unset to skip CI's real-gc integration fixtures.

Define `verify` in Bash once per shell. It refuses worktree environment and credential files before launching a command, gives the command an empty home, and forwards only tool paths, caches and temporary-directory settings. Use trusted installed tools on PATH and a populated Go module cache.

```bash
verify() (
  set -eu
  root=$(git rev-parse --show-toplevel)
  cd -- "$root"
  config_files=$(git ls-files --cached --others -- \
    ':(glob)**/.env' ':(glob)**/.env.*' ':(glob)**/env' \
    ':(glob)**/.netrc' ':(glob)**/credentials.json' ':(glob)**/service-account*.json' \
    ':(exclude,glob)**/*.example')
  if [ -n "$config_files" ]; then
    printf '%s\n' 'Use a checkout free of local credentials and environment files.' >&2
    exit 2
  fi
  module_cache=
  build_cache=
  if command -v go >/dev/null; then
    module_cache=$(GOTOOLCHAIN=local go env GOMODCACHE)
    build_cache=$(GOTOOLCHAIN=local go env GOCACHE)
  fi
  temp_root=$(cd -- "${TMPDIR:-/tmp}" && pwd -P)
  test_home=$(mktemp -d "$temp_root/verify.XXXXXXXX")
  trap 'rm -rf -- "$test_home"' EXIT
  if [ "${1:-}" = -C ]; then
    cd -- "$2"
    shift 2
  fi
  env -i -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN \
    PATH="$PATH" HOME="$test_home" TMPDIR="$temp_root" \
    GOCACHE="$build_cache" GOMODCACHE="$module_cache" GOTOOLCHAIN=local GOPROXY=off \
    GC_NO_INSTALL=1 GIT_TERMINAL_PROMPT=0 "$@"
)
```

## Before a commit

For changes under `reachable/`, run its Node suite. For Gas City or shared worker changes, run the root and Gas City pytest suites.

```sh
verify make test-reachable
```

```sh
verify make test-gascity
```

Before handing off a change, run the Reachable Node tests and the repository's configured pytest directories together. The summary reports both exit codes.

```sh
verify make test
```

For follow-through, also run the Node suite that CI exercises on Linux. For Gastown, run each shell test and stop on the first failure. On macOS, use provisioned Homebrew coreutils and GNU sed. The fixtures need a physical temporary path and GNU in-place editing.

```sh
verify -C follow-through/test node --test
```

```sh
verify bash -ec 'if [[ $(uname -s) == Darwin ]]; then PATH="$(brew --prefix coreutils)/libexec/gnubin:$(brew --prefix gnu-sed)/libexec/gnubin:$PATH"; fi; for script in gastown/tests/test_*.sh; do bash "$script"; done'
```

## Registry and pack manifests

Use a complete Git checkout when changing `registry.toml` or a pack manifest. The Python validator checks release pins and content hashes against local Git history. CI additionally requires Git so a missing checkout cannot silently reduce coverage. The release validator requires a gc executable with pack release support and can fetch the public source repository to check releases.

```sh
verify make registry-format-validate
verify python3 validate_registry.py --require-git
verify make registry-validate
```

## Go modules

Run vet and tests in each touched module. Nested modules have independent package trees. The root module declares Go 1.22. The Slack modules, Cloudflare runtime and board-watcher declare 1.25.9. The Google Chat adapter declares 1.27.2. Use an installed toolchain at least as new as the module requires. CI selects Go from `slack-full/adapter/go.mod` and tests the root, four Slack modules and the Cloudflare runtime. It also vets the runtime. Run Google Chat and board-watcher locally when touched because CI omits both.

The test flags force an uncached run and emit one JSON event per test action, including skips.

```sh
verify go vet .
verify go test -count=1 -json .
```

```sh
verify -C slack-full/adapter go vet ./...
verify -C slack-full/adapter go test -count=1 -json ./...
```

```sh
verify -C slack-full/cli go vet ./...
verify -C slack-full/cli go test -count=1 -json ./...
```

```sh
verify -C slack-channel/adapter go vet ./...
verify -C slack-channel/adapter go test -count=1 -json ./...
```

```sh
verify -C slack-mini/adapter go vet ./...
verify -C slack-mini/adapter go test -count=1 -json ./...
```

```sh
verify -C runtime-cloudflare/runtime go vet ./...
verify -C runtime-cloudflare/runtime go test -count=1 -json ./...
```

```sh
verify -C gchat/adapter go vet ./...
```

Google Chat vet covers static checks only. Its full test run has a measured backlog timeout. [Full module transcript](../../../assets/pr-evidence/gp-me0t/gchat-adapter-test.txt)

```sh
verify -C board-watcher/watcher go vet ./...
verify -C board-watcher/watcher go test -count=1 -json ./...
```

## Hosted verification

On a pull request targeting main, CI runs on the synthetic merge commit. The checkout fetches full history. Its registry step runs `python3 validate_registry.py --require-git` at that commit. The Python step includes the root `tests/` directory even on a skill-only change. Go vet and Go test keep build products in caches or temporary directories. The checks open no persistent server or fixed port. After a test that reports a loopback port exits, `lsof -nP -iTCP:PORT -sTCP:LISTEN` should show no listener for that port. Substitute the numeric port from the test.

## Live integration

Keep production commands out of verification, including message and mail senders, adapter and gateway launches, city workflow and tmux commands, provider hooks, deployment, registry publishing and withdrawal. [Repository entry points and exceptions](../../../assets/pr-evidence/gp-me0t/operational-commands.md)

Use fixture data, temporary directories and local fakes for these checks. Keep deployed adapters, their state and media stores, their environment files, running processes, chat services and remote hosts outside the test environment. A founder provisions a scratch box for deployed-behavior checks. Customer boxes are outside that workflow.

Leave CI's installed-gc checks to CI. Pack lint and Gastown's lint baseline use a gc executable. Local pytest can run the Gastown static lint check if gc is on PATH. Shared-role composition, order scopes and maintained-pack doctor, command and formula checks need a real gc supplied as `GC_TEST_BIN`. The Cloudflare runtime protocol conformance job installs gc and probes its runtime-check support.
