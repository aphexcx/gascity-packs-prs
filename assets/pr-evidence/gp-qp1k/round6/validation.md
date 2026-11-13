# Round 6 validation

Production source is the merge at `2d86174a7852176789f70a1e70f57e08df5c9bf7`.
Tests were added in `54f0a032fd2035383db69ac65645695954333c90`; the review's
test assertion and evidence clarification are in
`c4cd82471f41336496da4e8a1e6577014c932a51`. Later commits contain evidence only.
The Go adapters are unchanged throughout this round. Versions: Go 1.27.3
darwin/arm64; Node v24.21.0, measured with `pnpm exec node --version`.
Gateway dependencies already existed; no install or dependency mutation ran.
No unsupported-engine warning appeared.

## Interaction checks

| Row | Test | Result at merged production source |
| --- | --- | --- |
| A | `notice clock and immediate mail do not wait for the held inbox poll`; `continuous failed receipts notify at three minutes without an inbox read` | 2 pins: uncertain stop mails immediately and posts at +180000 fake ms; continuous failure gives neither early, then both at +180000; inbox read count does not change |
| B | `wake and poll carry the same admission and receipt result into notices` | 2 child pins (3 nodes including parent); removing the PR's receipt observation produces 2 assertion failures, 3 failed nodes including parent |
| C | `dropping and reconnecting a held link preserves the outage and notice marks` | 1 pin: fallback read, reconnect and wake preserve the outage id; one line, one start mail, no cleared mail |
| D | `a lost wake starts no outage before arrival and uses the adapter arrival clock` | 1 pin: no tracked message/notice before +300000 ms; first owner line +480000 ms from provider send |
| E | `test_status_keeps_link_and_never_silent_in_plain_and_json` | 4 pins; removing main's link line gives 4 failures, removing the PR's notice line gives 4 failures |
| F | `an adopted status wakes once while a lost-answer replay keeps the accepted send attempt` | 1 pin; removing main's adoption wake times out waiting for the frame; removing the PR's accepted-attempt header fails its assertion |

The complete new adapter file passes 7 nodes (6 leaf cases plus B's parent),
including after the review correction. A's pre-bound check is one fake tick
before the threshold; its post is stamped exactly at the threshold. The main
loop is configured at 250 ms, and its independent notice-poll call is retained.
As `interactions.md` explains, this does not measure wall-clock latency under
host load or slow I/O.

D uses the adapter arrival clock: `inbound.js` tracks with `created: clock()`
and result/stop use that same clock. The old provider timestamp does not make
the owner line immediate. The fixture's zero-byte terminal pending receipt
starts operator mail on arrival and gives the owner the three-minute grace.

F exercises the adjacent send-completion hunks: main wakes adopted status rows
after the durable batch; the PR then returns fresh/replayed and accepted-attempt
headers. Replaying a lost answer uses the original attempt and neither sends
nor wakes a second time. Existing gateway tests also cover retention cleanup.

All mutation runs used the merge's production files plus tests. Before the
next mutation, the exact hunk was restored and a zero source diff checked.
The prescribed `git checkout -- FILE` was blocked by the command guard;
reversing the exact patch restored the same bytes. No mutation was committed.

## Final suites

| Module | Command | Result |
| --- | --- | --- |
| WhatsApp adapter | `pnpm test` (`node --test --test-concurrency=2 'test/*.test.js'`) | 362 nodes: 358 passed, 3 failed, 1 cancelled; affected cases rerun below |
| WhatsApp adapter | `pnpm exec node --test --test-concurrency=1 test/never-silent.test.js` | 78 passed |
| WhatsApp adapter | `pnpm exec node --test --test-concurrency=1 test/never-silent-held-link.test.js` | 7 passed after review correction |
| WhatsApp adapter | `pnpm exec node --test --test-concurrency=1 test/link.test.js test/link-engine.test.js` | 17 passed; both files byte-identical to main |
| WhatsApp adapter | `pnpm exec node --test --test-concurrency=1 test/conversation.test.js test/files-out.test.js test/recovery.test.js test/review.test.js` | 103 nodes: 102 passed, 1 cancelled; all original failing cases passed |
| WhatsApp adapter | `pnpm exec node --test --test-concurrency=1 --test-name-pattern='^video survives SIGKILL at after-media-receipt' test/files-out.test.js` | 1 passed; final remaining timeout case |
| WhatsApp gateway | `pnpm test` | 308 passed, 6 files |
| WhatsApp gateway | `pnpm typecheck` | exit 0 |
| Slack adapter | `env -u GC_BIN -u NEVER_SILENT go vet ./...` | exit 0 |
| Slack adapter | `env -u GC_BIN -u NEVER_SILENT go test -p=1 -json ./... -count=1` | 13,443 test rows passed, 3 skipped; 3 packages passed |
| Google Chat adapter | `env -u GC_BIN -u NEVER_SILENT go vet ./...` | exit 0 |
| Google Chat adapter | `env -u GC_BIN -u NEVER_SILENT go test -p=1 -json ./... -count=1 -timeout=30m` | 1,902 test rows passed, 1 failed, 2 skipped; 21 packages passed, 1 failed |
| Google Chat adapter | `env -u GC_BIN -u NEVER_SILENT go test -p=1 -json ./internal/puller -run '^TestRunRotationsCloseAndReuseQuarantineHandles$' -count=1` | 1 passed, 1 package passed |
| Repository root | `make test PYTEST_DIRS='slack-full/tests whatsapp/tests gchat/tests follow-through/tests'` | 1,163 passed, 3 skipped, 2 warnings, 99 subtests passed |

Commands run from their named module directories; Go counts include subtests.
`completion.manifest` records command exits, including failed development and
mutation runs. The final evidence commit changes no executable source or tests.
Published logs remove local paths and trailing whitespace; the original raw
outputs are retained outside the branch.

The initial full adapter run under concurrent suite load recorded 362 nodes:
358 passed, 3 failed, 1 cancelled. The unchanged cases were the conversation
M2 recipient check, video crash recovery at `before-media-upload`, receipt
recovery case 7b, and the review M2 compaction/ack race. The video timeout left
its fake adapter child running; its ownership, ancestry and module cwd were
verified before terminating only that fixture to allow the failed run to end.
Original output is retained in `adapter-final.log`; no test/source fix was made.
The sequential four-file retry passed all those cases, but a different video
boundary (`after-media-receipt`) cancelled at its timeout: 102 passed, 1 cancelled.
That exact case then passed alone (1/1 in 0.63 seconds). The full-run failures
remain failures in this record; isolated passes do not turn the full run green.

Google Chat's unchanged `TestRunRotationsCloseAndReuseQuarantineHandles` hit
its context deadline in the loaded full run, then passed alone in 3.08 seconds.
The isolated run keeps GC_BIN and NEVER_SILENT unset, like every Go invocation.
The full Google Chat run completed, including the 3,000-message catch-up.
These are observed timeout/retry results; host load is a likely explanation,
not a separately proven root cause. All affected existing test files and source
remain unchanged by this round's post-merge commits.

## Test-development corrections

Early setup failures are retained as development evidence, not product reds:
the fixture initially combined `failed` with a terminal flag, but terminal
holds require `pending`; the gateway assertion initially treated a raw D1 row
as an inbox row. The next adapter fixture exposed a partial append read and a
fake-mail child still writing during cleanup. The new test now publishes each
fake mail atomically and waits for local-mail completion before cleanup.
Only this new test file changed; existing fixtures and product logic did not.
`adapter-pins.log`, `adapter-pins-corrected.log`, `adapter-pins-green.log` and
`gateway-pin.log` preserve those failed attempts. The first gateway invocation
passed an extra separator through the package script and therefore ran the
whole suite (307 passed, only the new malformed assertion failed). Subsequent
focused invocations use `pnpm exec vitest run test -t ...`.

## Review and limits

One fresh DEEP review checked the merge against both parents for all eleven
shared files and every post-merge test/evidence hunk. BLOCKER 0, MAJOR 0,
MINOR 1, NIT 0. The minor concerned pre-bound mail coverage and timing-claim
precision; it was fixed in `c4cd82471f41336496da4e8a1e6577014c932a51`, and the
same reviewer's narrow addendum is clean. See `self-review.md` and
`self-review-addendum.md`. No new outside-round finding was identified.

The earlier round-5 whole-delta gate's three switch-on faults remain held as
`gp-ll79`, `gp-rt67` and `gp-dt2p`. Slack's `IsStopped` remains a Slack-only
engine method; this round changes neither Go engine copy. The round-6 whole
gate and second-model read belong to the mayor and remain pending.

All tests used invented fixtures, fake gc/provider endpoints and local
loopback/workerd sockets. No live adapter state, gateway, city event log or
real conversation was read; no test mail reached a live city. No deployment,
feature enablement, migration execution, ready flip or PR merge was performed.
