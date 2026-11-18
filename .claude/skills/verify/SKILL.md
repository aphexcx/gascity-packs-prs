---
name: verify
description: "Run before every commit: the exact test, validate and Go module commands for this repository"
---

# Verify a pack change

Start each block in the repository root. Use Python 3.12, as CI does. Select an existing environment where `python3` imports pytest, PyYAML and jsonschema. The Makefile probes those imports and otherwise calls uv to add all three packages. On a no-install worker, select the provisioned interpreter on PATH before running make. Reachable and follow-through also require Node. Gastown's shell tests require Bash and ripgrep.

Worker sessions can export a template, a real gc executable and adapter feature switches. Clear `GC_TEMPLATE` so the claim tests select their fixture route, `GC_BIN` so adapter tests use their fake CLI, and `NEVER_SILENT` so tests control their own feature switch. Leave `GC_TEST_BIN` unset to skip CI's real-gc integration fixtures.

Run from a shell free of deployment settings and credentials. The measurements used installed tools and a populated Go module cache with `GOTOOLCHAIN=local` and `GOPROXY=off`.

## Before a commit

For changes under `reachable/`, run its Node suite. For Gas City or shared worker changes, run the root and Gas City pytest suites.

```sh
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN make test-reachable
```

```sh
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN make test-gascity
```

Before handing off a change, run the Reachable Node tests and the repository's configured pytest directories together. The summary reports both exit codes.

```sh
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN make test
```

For follow-through, also run the Node suite that CI exercises on Linux. For Gastown, run each shell test and stop on the first failure. On macOS, use provisioned Homebrew coreutils and GNU sed. The fixtures need a physical temporary path and GNU in-place editing.

```sh
(cd follow-through/test && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN node --test)
```

```sh
if [ "$(uname -s)" = Darwin ]; then
  export PATH="$(brew --prefix coreutils)/libexec/gnubin:$(brew --prefix gnu-sed)/libexec/gnubin:$PATH"
  export TMPDIR="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
fi
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN bash -ec 'for script in gastown/tests/test_*.sh; do bash "$script"; done'
```

## Registry and pack manifests

Use a complete Git checkout when changing `registry.toml` or a pack manifest. The Python validator checks release pins and content hashes against local Git history. CI additionally requires Git so a missing checkout cannot silently reduce coverage. The release validator requires a gc executable with pack release support and can fetch the public source repository to check releases.

```sh
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN make registry-format-validate
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN python3 validate_registry.py --require-git
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN make registry-validate
```

## Go modules

Run vet and tests in each touched module. Nested modules have independent package trees. The root module declares Go 1.22. The Slack modules, Cloudflare runtime and board-watcher declare 1.25.9. The Google Chat adapter declares 1.27.2. Use an installed toolchain at least as new as the module requires. CI selects Go from `slack-full/adapter/go.mod` and tests the root, four Slack modules and the Cloudflare runtime. It also vets the runtime. Run Google Chat and board-watcher locally when touched because CI omits both.

The test flags force an uncached run and emit one JSON event per test action, including skips.

```sh
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet .
env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json .
```

```sh
(cd slack-full/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
(cd slack-full/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json ./...)
```

```sh
(cd slack-full/cli && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
(cd slack-full/cli && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json ./...)
```

```sh
(cd slack-channel/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
(cd slack-channel/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json ./...)
```

```sh
(cd slack-mini/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
(cd slack-mini/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json ./...)
```

```sh
(cd runtime-cloudflare/runtime && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
(cd runtime-cloudflare/runtime && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json ./...)
```

```sh
(cd gchat/adapter && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
```

Google Chat vet covers static checks only. Its full test run has a measured backlog timeout. [Full module transcript](../../../assets/pr-evidence/gp-me0t/gchat-adapter-test.txt)

```sh
(cd board-watcher/watcher && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go vet ./...)
(cd board-watcher/watcher && env -u GC_TEMPLATE -u GC_BIN -u NEVER_SILENT -u GC_TEST_BIN go test -count=1 -json ./...)
```

## Live integration

Use fixture data, temporary directories and local fakes for these checks. Keep deployed adapters, their state and media stores, their environment files, running processes, chat services and remote hosts outside the test environment. A founder provisions a scratch box for deployed-behavior checks. Customer boxes are outside that workflow.

Leave CI's installed-gc checks to CI. Pack lint and Gastown's lint baseline use a gc executable. Local pytest can run the Gastown static lint check if gc is on PATH. Shared-role composition, order scopes and maintained-pack doctor, command and formula checks need a real gc supplied as `GC_TEST_BIN`. The Cloudflare runtime protocol conformance job installs gc and probes its runtime-check support.
