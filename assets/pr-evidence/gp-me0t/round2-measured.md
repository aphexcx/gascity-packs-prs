# Round-two verification

Commands ran serially on macOS arm64 with Python 3.12.13, Node 24.21.0 and Go 1.27.3. Each new suite started with the one-minute load below 20. The guard checked files, isolated HOME and environment, and disabled tool downloads. The Google Chat timeout was 30 minutes. Counts include named subtests where reported.

## New measurements

| Transcript | Result | Exit | Seconds | Starting load |
| --- | --- | ---: | ---: | ---: |
| [board-r2](round2-board-r2.txt) | 18 passed in 0.05s | 0 | 8.93 | 14.18 |
| [wecom-python-r2](round2-wecom-python-r2.txt) | 16 passed in 2.61s | 0 | 6.2 | 15.06 |
| [oversight-scripts-r2](round2-oversight-scripts-r2.txt) | 12 passed in 0.02s | 0 | 3.36 | 14.57 |
| [oversight-skill-r2](round2-oversight-skill-r2.txt) | 35 passed, 7 subtests passed in 0.20s | 0 | 3.99 | 14.61 |
| [gateway](round2-gateway.txt) | Test Files  6 passed (6), Tests  351 passed (351) | 0 | 16.22 | 13.06 |
| [gchat-r2](round2-gchat-r2.txt) | 25 packages passed, 1 without tests, 2217 named tests passed, 2 skipped, 0 failed | 0 | 568.66 | 6.82 |
| [whatsapp-r2](round2-whatsapp-r2.txt) | tests 477, pass 471, fail 5, cancelled 1, skipped 0 | 1 | 540.88 | 14.61 |
| [whatsapp-tracked](round2-whatsapp-tracked.txt) | tests 464, pass 464, fail 0, cancelled 0, skipped 0 | 0 | 254.21 | 7.92 |
| [replay-r2](round2-replay-r2.txt) | tests 1, pass 0, fail 1, cancelled 0, skipped 0 | 1 | 25.64 | 10.21 |
| [class-final](round2-class-final.txt) | 47 passed, 551 deselected in 22.28s | 0 | 25.41 | 14.55 |
| [class-handoff](round2-class-handoff.txt) | 48 passed, 551 deselected in 23.08s | 0 | 29.75 | 11.97 |
| [gastown](round2-gastown.txt) | 11 shell scripts passed | 0 | 85.51 | 12.96 |
| [gascity](round2-gascity.txt) | 1 failed, 1006 passed, 127 skipped, 10518 subtests passed in 354.85s (0:05:54) | 2 | 358.04 | 12.2 |
| [gascity-native](round2-gascity-native.txt) | 1007 passed, 127 skipped, 10518 subtests passed in 349.65s (0:05:49) | 0 | 352.79 | 8.62 |

## CARRIED measurements

These results come from the named round-one transcripts on the same pack source. R2 changed the guard. The command-block tests exercise the new guard and failure status. Historical rows for the Slack full tests and online release validator are outside the current offline command list.

| Round-one transcript | Result | Exit | Seconds |
| --- | --- | ---: | ---: |
| CARRIED [python-version](python-version.txt) | Python 3.12.13 | 0 | 0.03 |
| CARRIED [node-version](node-version.txt) | v24.21.0 | 0 | 0.04 |
| CARRIED [root-version](root-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.07 |
| CARRIED [slack-full-adapter-version](slack-full-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.02 |
| CARRIED [slack-full-cli-version](slack-full-cli-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.04 |
| CARRIED [slack-channel-adapter-version](slack-channel-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.04 |
| CARRIED [slack-mini-adapter-version](slack-mini-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.05 |
| CARRIED [runtime-cloudflare-runtime-version](runtime-cloudflare-runtime-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.03 |
| CARRIED [gchat-adapter-version](gchat-adapter-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.03 |
| CARRIED [board-watcher-watcher-version](board-watcher-watcher-version.txt) | go version go1.27.3 darwin/arm64 | 0 | 0.05 |
| CARRIED [all](all.txt) | pass 92, fail 0, skipped 1, 2998 passed, 131 skipped, 2 warnings, 10746 subtests passed | 0 | 683.69 |
| CARRIED [follow-through](follow-through.txt) | pass 108, fail 0, skipped 0 | 0 | 59.53 |
| CARRIED [registry-format](registry-format.txt) | 1 registry checked | 0 | 61.24 |
| CARRIED [registry-git](registry-git.txt) | 1 registry checked | 0 | 58.78 |
| CARRIED [root-package-vet](root-package-vet.txt) | vet clean | 0 | 1.15 |
| CARRIED [root-test](root-test.txt) | 1 package, 3 passed, 0 skipped, 0 failed | 0 | 0.81 |
| CARRIED [slack-full-adapter-vet](slack-full-adapter-vet.txt) | vet clean | 0 | 1.13 |
| CARRIED [slack-full-adapter-test](slack-full-adapter-test.txt) | 5 packages, 13537 passed, 3 skipped, 0 failed | 0 | 252.95 |
| CARRIED [slack-full-cli-vet](slack-full-cli-vet.txt) | vet clean | 0 | 2.28 |
| CARRIED [slack-full-cli-test](slack-full-cli-test.txt) | 8 packages, 237 passed, 0 skipped, 0 failed | 0 | 1.43 |
| CARRIED [slack-channel-adapter-vet](slack-channel-adapter-vet.txt) | vet clean | 0 | 0.49 |
| CARRIED [slack-channel-adapter-test](slack-channel-adapter-test.txt) | 1 package, 168 passed, 0 skipped, 0 failed | 0 | 3.73 |
| CARRIED [slack-mini-adapter-vet](slack-mini-adapter-vet.txt) | vet clean | 0 | 0.60 |
| CARRIED [slack-mini-adapter-test](slack-mini-adapter-test.txt) | 1 package, 67 passed, 0 skipped, 0 failed | 0 | 3.21 |
| CARRIED [runtime-cloudflare-runtime-vet](runtime-cloudflare-runtime-vet.txt) | vet clean | 0 | 0.49 |
| CARRIED [runtime-cloudflare-runtime-test](runtime-cloudflare-runtime-test.txt) | 2 packages, 12 passed, 0 skipped, 0 failed | 0 | 0.80 |
| CARRIED [gchat-adapter-vet](gchat-adapter-vet.txt) | vet clean | 0 | 1.57 |
| CARRIED [board-watcher-watcher-vet](board-watcher-watcher-vet.txt) | vet clean | 0 | 0.49 |
| CARRIED [board-watcher-watcher-test](board-watcher-watcher-test.txt) | 1 package, 46 passed, 0 skipped, 0 failed | 0 | 0.80 |
| CARRIED [registry-release](registry-release.txt) | 39 release hashes checked | 0 | 806.21 |
| CARRIED [reachable](reachable.txt) | pass 92, fail 0, skipped 1 | 0 | 20.46 |

## Failures in isolation

Each cell gives the exit code and starting one-minute load. Exit 1 means the named test failed, exit 0 means it passed. A full-run failure is retained even when the isolated run passes.

| Test and source | Trial 1 | Trial 2 | Trial 3 | Trial 4 | Trial 5 |
| --- | --- | --- | --- | --- | --- |
| Held config “false”, `config.test.js:13` | 1 / 15.02 | 1 / 17.33 | 1 / 16.66 | 1 / 16.12 | 1 / 15.27 |
| Held config “0”, `config.test.js:13` | 1 / 19.62 | 1 / 17.27 | 1 / 16.93 | 1 / 15.71 | 1 / 15.27 |
| Held config “no”, `config.test.js:13` | 1 / 17.9 | 1 / 16.97 | 1 / 16.93 | 1 / 15.71 | 1 / 15.01 |
| Held safety read until reconnect, `link.test.js:278` | 0 / 17.9 | 0 / 16.97 | 0 / 16.7 | 0 / 15.41 | 0 / 15.01 |
| Slow flush with acknowledgement reaction unset, `slow-flush.test.js:52` | 0 / 14.88 | 0 / 15.53 | 0 / 15.4 | 0 / 15.4 | 0 / 14.97 |
| Reachable archive replay, `review.test.cjs:98` | 1 / 10.21 | 0 / 15.13 | 0 / 14.8 | 0 / 15.07 | 0 / 15.67 |

The three configuration fixtures came from an untracked, pre-existing config.test.js, absent from the base tree. Their failures are specific to this checkout. They replace the entire environment themselves. They expect a warning during configuration loading. The implementation records the unknown value and prints the warning later during adapter startup. The discrepancy is independent of caller credentials.

The held reconnect and slow-flush cases passed all five isolated runs. For Reachable one of five isolated runs timed out. The two-second archive child deadline was unchanged. The WhatsApp runner, its pipeline worker and its local fixture process were checked by PID after shutdown. All three had exited.

The WhatsApp adapter's tracked-files run passed all 464 tests, with zero failures, cancellations or skips. The draft now prescribes that form and tests the actual argument list received by Node with an extra untracked file present.

The first Gas City Make run selected GNU chmod outside Gastown. The failing fixture was `test_a_pid_that_cannot_be_written_leaves_no_lock_and_fails_closed`. With the native tool each of five isolated runs passed. The isolated command was `verify python3 -m pytest -q gascity/tests -k test_a_pid_that_cannot_be_written_leaves_no_lock_and_fails_closed`.

| ACL trial | Exit | Seconds | Starting load |
| --- | ---: | ---: | ---: |
| 1 | 0 | 3.32 | 9.15 |
| 2 | 0 | 3.07 | 9.15 |
| 3 | 0 | 3.1 | 8.82 |
| 4 | 0 | 3.18 | 8.76 |
| 5 | 0 | 3.15 | 8.76 |
