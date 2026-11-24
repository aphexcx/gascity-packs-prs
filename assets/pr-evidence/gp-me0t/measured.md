# Verification measurements

These are the round-one measurements. The current skill uses the [round-two results and isolated controls](round2-measured.md).

Commands ran serially on macOS arm64 with a provisioned Python 3.12 environment, the no-install toolchain, and local Go dependencies. The runner cleared worker and integration overrides. Git history was available. Durations are elapsed seconds.

Each link gives the exact command, exit code and elapsed time. Commands beginning with verify use the Bash function in the skill. The first Make run and the Google Chat timeout predate that function. Test streams are reduced to runner summaries or package completion events. Go counts include named subtests, so a failing child and its parent both count. The red and mutation controls deliberately introduce a defect; their nonzero exits demonstrate detection.

| Command transcript | Result | Exit | Seconds |
| --- | --- | ---: | ---: |
| [red](red.txt) | 1 failed, 551 deselected | 1 | 7.61 |
| [green](green.txt) | 1 passed, 551 deselected | 0 | 2.81 |
| [absent-target](absent-target.txt) | 1 failed | 1 | 0.23 |
| [python-version](python-version.txt) | Python 3.12.13 | 0 | 0.03 |
| [node-version](node-version.txt) | v24.21.0 | 0 | 0.04 |
| [reachable-original](reachable-original.txt) | pass 92, fail 0, skipped 1 | 0 | 16.97 |
| [root-version](root-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.07 |
| [slack-full-adapter-version](slack-full-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.02 |
| [slack-full-cli-version](slack-full-cli-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.04 |
| [slack-channel-adapter-version](slack-channel-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.04 |
| [slack-mini-adapter-version](slack-mini-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.05 |
| [runtime-cloudflare-runtime-version](runtime-cloudflare-runtime-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.03 |
| [gchat-adapter-version](gchat-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.03 |
| [gchat-adapter-test](gchat-adapter-test.txt) | 26 packages, 2215 passed, 2 skipped, 2 failed | 1 | 549.93 |
| [board-watcher-watcher-version](board-watcher-watcher-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.05 |
| [all-first](all-first.txt) | pass 91, fail 1, skipped 1, 2998 passed, 131 skipped, 2 warnings, 10746 subtests passed | 2 | 686.88 |
| [all](all.txt) | pass 92, fail 0, skipped 1, 2998 passed, 131 skipped, 2 warnings, 10746 subtests passed | 0 | 683.69 |
| [reachable-guarded-first](reachable-guarded-first.txt) | pass 91, fail 1, skipped 1 | 2 | 25.35 |
| [control-guard-removed](control-guard-removed.txt) | 1 failed | 1 | 8.44 |
| [control-quoted-green](control-quoted-green.txt) | 1 passed | 0 | 18.96 |
| [control-quoted-red](control-quoted-red.txt) | 1 failed | 1 | 0.39 |
| [control-path-green](control-path-green.txt) | 1 passed | 0 | 0.74 |
| [control-path-renamed](control-path-renamed.txt) | 1 failed | 1 | 0.21 |
| [control-sentinel](control-sentinel.txt) | 1 passed | 0 | 0.33 |
| [gascity](gascity.txt) | 961 passed, 127 skipped, 10518 subtests passed | 0 | 450.66 |
| [follow-through](follow-through.txt) | pass 108, fail 0, skipped 0 | 0 | 59.53 |
| [gastown-mac](gastown-mac.txt) | 11 scripts passed | 0 | 231.85 |
| [registry-format](registry-format.txt) | 1 registry checked | 0 | 61.24 |
| [registry-git](registry-git.txt) | 1 registry checked | 0 | 58.78 |
| [root-package-vet](root-package-vet.txt) | vet clean | 0 | 1.15 |
| [root-test](root-test.txt) | 1 package, 3 passed, 0 skipped, 0 failed | 0 | 0.81 |
| [slack-full-adapter-vet](slack-full-adapter-vet.txt) | vet clean | 0 | 1.13 |
| [slack-full-adapter-test](slack-full-adapter-test.txt) | 5 packages, 13537 passed, 3 skipped, 0 failed | 0 | 252.95 |
| [slack-full-cli-vet](slack-full-cli-vet.txt) | vet clean | 0 | 2.28 |
| [slack-full-cli-test](slack-full-cli-test.txt) | 8 packages, 237 passed, 0 skipped, 0 failed | 0 | 1.43 |
| [slack-channel-adapter-vet](slack-channel-adapter-vet.txt) | vet clean | 0 | 0.49 |
| [slack-channel-adapter-test](slack-channel-adapter-test.txt) | 1 package, 168 passed, 0 skipped, 0 failed | 0 | 3.73 |
| [slack-mini-adapter-vet](slack-mini-adapter-vet.txt) | vet clean | 0 | 0.60 |
| [slack-mini-adapter-test](slack-mini-adapter-test.txt) | 1 package, 67 passed, 0 skipped, 0 failed | 0 | 3.21 |
| [runtime-cloudflare-runtime-vet](runtime-cloudflare-runtime-vet.txt) | vet clean | 0 | 0.49 |
| [runtime-cloudflare-runtime-test](runtime-cloudflare-runtime-test.txt) | 2 packages, 12 passed, 0 skipped, 0 failed | 0 | 0.80 |
| [gchat-adapter-vet](gchat-adapter-vet.txt) | vet clean | 0 | 1.57 |
| [board-watcher-watcher-vet](board-watcher-watcher-vet.txt) | vet clean | 0 | 0.49 |
| [board-watcher-watcher-test](board-watcher-watcher-test.txt) | 1 package, 46 passed, 0 skipped, 0 failed | 0 | 0.80 |
| [registry-release](registry-release.txt) | 39 release hashes checked | 0 | 806.21 |
| [reachable](reachable.txt) | pass 92, fail 0, skipped 1 | 0 | 20.46 |
| [control-final-pytest](control-final-pytest.txt) | 1 passed, 551 deselected | 0 | 12.25 |
