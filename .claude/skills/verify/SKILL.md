---
name: verify
description: "Run before every commit: the exact test, validate and Go module commands for this repository"
---

# Verify a pack change

Start in the repository root. Before each commit run the checks for the files you touched. Before handoff run the Reachable and full Python checks below. Both the full Python suite and the narrower Gas City Make check can invoke the real gc on PATH for the Gastown lint baseline. Its findings depend on whether the checkout sits inside a city. The checks that require an installed gc for integration run in hosted CI.

Use provisioned Python 3.12, Node 24 and the Go toolchain and module cache for the modules you touch. Select their executable directories on PATH. The Makefile falls back to downloading Python packages when its imports fail. The function checks the imports inside the test environment before starting Make. It sets the Node shim to offline mode, disables Go downloads, and restricts Git transport to local fixture repositories.

Use Bash. Define the following function in each new shell that runs a check, or keep the definition and call in the same shell invocation. Each subsequent block runs one check and returns its status. Enable Bash's pipefail option before piping a call so the pipeline reports a failed check. An empty temporary home isolates fixture configuration. The allowlisted environment clears worker values including `GC_TEMPLATE`, `GC_BIN`, `NEVER_SILENT` and `GC_TEST_BIN` so tests choose their own routes, fakes and feature switches.

The scan checks tracked, untracked and ignored names in this worktree against the environment, token, OAuth, bot-token, and credential-key patterns below. For each PEM file it checks the first 64 KiB for a private-key header. Public certificate bundles pass. An unreadable or nonregular PEM stops verification.

```bash
verify() (
  set -eu
  unset CDPATH || exit $?
  for selector in "${!GIT_@}"; do unset "$selector" || exit $?; done
  temp_root=$(cd -- "${TMPDIR:-/tmp}" && pwd -P) || exit $?
  test_home=$(mktemp -d "$temp_root/verify.XXXXXXXX") || exit $?
  trap 'rm -rf -- "$test_home"' EXIT || exit $?
  clean=(env -i PATH="$PATH" HOME="$test_home" TMPDIR="$temp_root"
    GC_NO_INSTALL=1 GC_TOOLCHAIN_NODE_INSTALL=off GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=file
    GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off)
  root=$( "${clean[@]}" git rev-parse --show-toplevel ) || exit $?
  cd -- "$root" || exit $?
  config_files=$( "${clean[@]}" git ls-files --cached --others -- \
    ':(glob)**/.env' ':(glob)**/.env.*' ':(glob)**/env' ':(glob)**/.envrc' \
    ':(glob)**/.netrc' ':(glob)**/credentials.json' ':(glob)**/service-account*.json' \
    ':(glob)**/install.env' ':(glob)**/adapter.env' ':(glob)**/slack.env' \
    ':(glob)**/bot-token*.txt' ':(glob)**/client.json' ':(glob)**/token.json' \
    ':(glob)**/.beads-credential-key' ':(glob)**/.dev.vars' \
    ':(exclude,glob)**/*.example') || exit $?
  if [[ -n $config_files ]]; then
    printf '%s\n' 'Use a checkout free of local credential and environment files.' >&2
    exit 2
  fi
  find . -name .git -prune -o -name '*.pem' -print0 > "$test_home/pem-names" || exit $?
  while IFS= read -r -d '' pem; do
    if [[ -f $pem && ! -L $pem && -r $pem ]]; then
      # Inspect both statuses even when the caller enabled errexit or pipefail.
      ! LC_ALL=C head -c 65536 "$pem" | LC_ALL=C grep -aE -- '^-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY-----[[:space:]]*$' >/dev/null
      status=("${PIPESTATUS[@]}")
      (( status[0] == 0 )) || { printf 'Unreadable PEM: %s\n' "$pem" >&2; exit "${status[0]}"; }
      case ${status[1]} in 1) continue ;; 0) ;; *) exit "${status[1]}" ;; esac
    fi
    printf 'Refusing credential or unreadable PEM: %s\n' "$pem" >&2
    exit 2
  done < "$test_home/pem-names" || exit $?
  if [[ ${1:-} == -C ]]; then
    cd -P -- "$root/$2" || exit $?
    [[ "$PWD/" == "$root/"* ]] || exit 2
    shift 2 || exit $?
  fi
  module_cache=
  build_cache=
  if command -v go >/dev/null; then
    probe=(env -i PATH="$PATH" HOME="${HOME:-$test_home}" GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off)
    for key in GOMODCACHE GOCACHE GOPATH XDG_CACHE_HOME; do
      value=${!key:-}
      [[ $value == /* && -d $value ]] && probe+=("$key=$value")
    done
    module_cache=$( "${probe[@]}" go env GOMODCACHE ) || exit $?
    build_cache=$( "${probe[@]}" go env GOCACHE ) || exit $?
  fi
  clean+=(GOMODCACHE="$module_cache" GOCACHE="$build_cache")
  need() { printf 'Missing %s in the verification environment.\n' "$1" >&2; exit 2; }
  command -v -- "$1" >/dev/null || need "$1"
  case "$1 ${2:-}" in
    'make test'|'make test-gascity'|'python3 -m')
      "${clean[@]}" python3 -c 'import pytest, yaml, jsonschema' >/dev/null 2>&1 || need 'pytest, PyYAML and jsonschema for python3' ;;
  esac
  case "$1 ${2:-} ${3:-} ${4:-}" in
    'make test '*|'make test-gascity '*|'make test-reachable '*|'python3 -m pytest tests'|node\ *|node_modules/.bin/vitest\ *)
      version=$( "${clean[@]}" node -p "process.versions.node.split('.')[0]" ) || need 'Node 24'
      [[ $version == 24 ]] || need 'Node 24' ;;
  esac
  if [[ $PWD == "$root/wecom/tests" && $1 == python3 ]]; then
    # The doctor fixture passes on PATH and HOME alone. Pin its node to the executable behind the offline shim.
    node_bin=$( "${clean[@]}" node -p 'process.versions.node.split(".")[0] === "24" && process.execPath' ) || need 'installed Node 24'
    [[ $node_bin == /* && -f $node_bin && -x $node_bin ]] || need 'installed Node 24'
    mkdir "$test_home/bin" || exit $?
    ln -s -- "$node_bin" "$test_home/bin/node" || exit $?
    clean+=(PATH="$test_home/bin:$PATH")
  fi
  if [[ $1 == bash ]]; then
    "${clean[@]}" bash -c '(( BASH_VERSINFO[0] >= 5 ))' || need 'Bash 5'
    for tool in jq rg; do "${clean[@]}" "$tool" --version >/dev/null 2>&1 || need "$tool"; done
    for tool in sed mktemp date; do
      version=$( "${clean[@]}" "$tool" --version 2>/dev/null ) || need "GNU $tool"
      [[ $version == *GNU* ]] || need "GNU $tool"
    done
  fi
  if [[ $PWD == "$root/whatsapp/adapter" && $1 == node && $# == 3 && $3 == --test-concurrency=2 ]]; then
    "${clean[@]}" git ls-files --others -z -- ':(glob)test/*.test.js' > "$test_home/untracked" || exit $?
    while IFS= read -r -d '' test_file; do
      printf 'not run: untracked %q\n' "$test_file" >&2 || exit $?
    done < "$test_home/untracked" || exit $?
    "${clean[@]}" git ls-files -z -- ':(glob)test/*.test.js' > "$test_home/node-tests" || exit $?
    [[ -s $test_home/node-tests ]] || need 'tracked adapter tests'
    while IFS= read -r -d '' test_file; do set -- "$@" "$test_file" || exit $?; done < "$test_home/node-tests" || exit $?
  fi
  if [[ $1 == node_modules/.bin/vitest ]]; then
    clean+=(WRANGLER_SEND_METRICS=false CLOUDFLARE_CF_FETCH_ENABLED=false NODE_ENV=test)
  fi
  "${clean[@]}" "$@"
)
```

## Repository checks

Name tracked test files or use the directory selected by the repository's runner configuration. Before measuring a directory, compare its collected files with Git's tracked list and stop if it includes extra files.

For `reachable/`, run the Node suite with only the archive replay case excluded by name (under one minute). Neither local verification nor CI verifies that case because it timed out on the unchanged base under its two-second child deadline and CI omits Reachable. Record a failure, the result alone and the load together. Green alone is reported as load-sensitive. Preserve the first suite result and report any timeout in isolation.

```sh
verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'
```

For root tests, Gas City roles and worker scripts run the Gas City check (about six minutes).

```sh
verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'
```

For handoff, run all fourteen Python directories selected by CI (about nine minutes). Start with a clean checkout if a previous run left a pytest cache inside a packaged skill. Every Python test command disables pytest’s cache because those files appear in the pack’s inventory.

```sh
verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider
```

Follow-through's Linux CI Node suite takes about one minute.

```sh
verify -C follow-through/test node --test
```

For Gastown (about one minute, measured at 86 seconds) the fixtures require Bash 5, ripgrep, jq, GNU coreutils (date and mktemp), GNU sed, and a physical temporary directory. On macOS use a separate Bash shell for the Gastown block. Put the provisioned Bash and the coreutils and GNU sed gnubin directories first on PATH only in that shell. The Gas City Python suite uses the native chmod for macOS ACLs. **Stop if a prerequisite is absent.** The native macOS tools alone fail the legacy worktree fixture. The loop stops on its first failing script.

```sh
verify bash -ec 'for script in gastown/tests/test_*.sh; do bash "$script"; done'
```

## Registry and pack manifests

For registry or pack manifest changes use a full Git checkout. Each local validator takes about one minute. The second is CI's check against the merge commit.

```sh
verify make registry-format-validate
```

```sh
verify python3 validate_registry.py --require-git
```

## Additional pack tests

Run the matching command when changing a suite or the code it tests. Each Python block and the gateway's local Vitest block took under one minute. Use the function even for local adapter fixtures so the process receives only fixture credentials and endpoints. The WeCom doctor retains PATH and HOME alone, so the function puts the installed Node executable first on its child PATH.

```sh
verify -C board-watcher/tests python3 -m pytest -q . -p no:cacheprovider
```

```sh
verify -C wecom/tests python3 -m pytest -q . -p no:cacheprovider
```

```sh
verify -C oversight-rig/assets/scripts python3 -m pytest -q . -p no:cacheprovider
```

```sh
verify -C oversight-rig/skills/city-executive-status/tests python3 -m pytest -q . -p no:cacheprovider
```

For the WhatsApp adapter, the function passes each tracked test file by name and reports every omitted untracked test on stderr. The 30 tracked files took about four minutes. The reconnect and slow-flush timing cases both passed five times alone after failing under load.

```sh
verify -C whatsapp/adapter node --test --test-concurrency=2
```

```sh
verify -C whatsapp/gateway node_modules/.bin/vitest run test
```

## Go modules

Use Go 1.22 or later for the root. The Slack modules, board-watcher and Cloudflare runtime specify 1.25.9. Google Chat specifies 1.27.2. The cache probe honors existing absolute cache directories and resolves their locations with downloads disabled. Stop when a compatible local toolchain or cached dependency is missing. Run the blocks listed for the module you touched. The test command disables result caching and emits JSON events for counting passes, skips and failures.

**Root package**. Vet takes under one minute. Tests take under one minute.

```sh
verify go vet .
```

```sh
verify go test -count=1 -json .
```

**slack-full/adapter**. Vet takes under one minute. The hosted workflow runs the tests.

```sh
verify -C slack-full/adapter go vet ./...
```

**slack-full/cli**. Vet takes under one minute. Tests take under one minute.

```sh
verify -C slack-full/cli go vet ./...
```

```sh
verify -C slack-full/cli go test -count=1 -json ./...
```

**slack-channel/adapter**. Vet takes under one minute. Tests take under one minute.

```sh
verify -C slack-channel/adapter go vet ./...
```

```sh
verify -C slack-channel/adapter go test -count=1 -json ./...
```

**slack-mini/adapter**. Vet takes under one minute. Tests take under one minute.

```sh
verify -C slack-mini/adapter go vet ./...
```

```sh
verify -C slack-mini/adapter go test -count=1 -json ./...
```

**runtime-cloudflare/runtime**. Vet takes under one minute. Tests take under one minute.

```sh
verify -C runtime-cloudflare/runtime go vet ./...
```

```sh
verify -C runtime-cloudflare/runtime go test -count=1 -json ./...
```

**board-watcher/watcher**. Vet takes under one minute. Tests take under one minute.

```sh
verify -C board-watcher/watcher go vet ./...
```

```sh
verify -C board-watcher/watcher go test -count=1 -json ./...
```

**gchat/adapter**. Vet takes under one minute. Tests took nine minutes with the pack's 30-minute timeout for the durable backlog fixture. Run through the function to isolate the copied launcher and its local fake endpoints.

```sh
verify -C gchat/adapter go vet ./...
```

```sh
verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...
```

## Not run here, because

The `test-reachable` and combined `test` Make targets include the excluded archive replay test. Use the separate Reachable and full Python checks above for local handoff. [Base measurements](../../../assets/pr-evidence/gp-me0t/round3-measured.md)

- `slack-full/adapter` — a registration test starts the adapter's main in a new environment. That child defaults to the real Slack address and the local function cannot require a fake inside it. CI runs the complete module. The earlier local result of 13,537 passes and three skips is carried from the [original run](../../../assets/pr-evidence/gp-me0t/slack-full-adapter-test.txt).
- `wecom/adapter/test` — the SDK dependency must already be provisioned. This checkout has no installed adapter SDK. Its local WebSocket tests start a fixture process.
- `assets/pr-evidence/gp-2awj/round2/base-hooks/gchat/adapter/internal/inbound` — archived Go comparison fixture from a different revision.
- `assets/pr-evidence/gp-2awj/round2/base-hooks/slack-full/adapter` — archived Go comparison fixture from a different revision.
- `assets/pr-evidence/gp-g02t/minor1` — archived TypeScript control outside the gateway test project.

The `registry-validate` and `registry-validate-all` Make targets can fetch the release repository. Keep both outside offline verification. The hosted Check job uses the local Python validator with full Git history. On registry and pack manifest changes, the pack-release compatibility workflow installs gc and runs the underlying release validator through `scripts/pack_release_compat.py`. CodeQL runs separately.

## CI and deployed behavior

Pull requests to main run the Check workflow on the synthetic merge commit. A skill-only change runs the root pytest directory. The workflow also runs the configured Python suites, follow-through Node tests, Gastown shell scripts, the root and four Slack Go modules and the Cloudflare runtime Go tests and vet. CI's installed gc exercises pack lint, composition, order scopes, maintained-pack stand-up, the Gastown baseline and runtime conformance.

No hosted workflow runs Reachable’s Node suite, the Google Chat or board-watcher Go suites, the additional Python suites above, or the WhatsApp and WeCom adapter or gateway JavaScript suites. Root and Slack vet and the format-only registry target also run locally. Go puts binaries in its cache and temporary directory. These checks need no persistent listener. For a test that prints a local port, use lsof with the reported port to check for a remaining listener after exit.

Use a founder-provisioned scratch box for deployed checks. Never use a customer's box. Keep operational adapters, chat services, credentials and running cities outside verification. Consult the [repository inventory](../../../assets/pr-evidence/gp-me0t/operational-commands.md) before running an unfamiliar entry point.
