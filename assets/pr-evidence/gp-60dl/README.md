# gp-60dl: held gateway connection

Base: `dce538c80df3fa7731bec3d65c3d12b685530ce8` (remote main at claim).
All tests use local fakes, local D1 and a real local Durable Object in workerd.
No deployment or live gateway, database, Cloudflare API or Meta request was made.

## Behavior

The gateway holds wake-only WebSockets in `CityWake`. The adapter reads the
existing inbox on connect or wake, reads every five minutes while quiet and
connected, and uses its original polling interval whenever disconnected.
The original journal and acknowledgements remain the delivery mechanism.

The second ruling adds close code `4503`: `WAKE=off` closes a city's existing
sockets at its next stored delivery or inbox read. New connections receive
404 immediately. The per-box off switch restores polling on restart.

## Verification

The city pnpm wrapper reported `pnpm exec node --version` = `v24.21.0`.
The Python suite used an existing Python 3.12 venv with pytest because the
lane's default Python had no pytest. No dependency was added to the adapter.

| Check | Result |
| --- | --- |
| Gateway baseline: `pnpm test` in `whatsapp/gateway` | 273 passed |
| Adapter baseline: `pnpm test` in `whatsapp/adapter` | 252 passed |
| Pack baseline: `env -u GC_TEMPLATE python3 -m pytest whatsapp/tests -q` | 57 passed |
| New route on base | Five new cases failed: 404 instead of authentication/upgrade behavior |
| Focused gateway behavior before wake implementation | 11 failed, 8 passed; 273 existing cases excluded by the name filter |
| Adapter connection cases before implementation | 11 failed, 1 passed; the filter also reported 16 other file entries as passed |
| Status before CLI change | 2 failed, 57 passed |
| Shutdown ruling before implementation | 2 failed, 299 excluded by the name filter |
| Adapter final: `pnpm test` | 269 passed, 0 failed; includes all 252 original tests |
| Pack final: `env -u GC_TEMPLATE python3 -m pytest whatsapp/tests -q` | 59 passed |
| Gateway final: `pnpm test` | 306 passed, 0 failed; includes all 273 original tests |
| Stalled failure accounting before fix | Three failed: webhook, adopted-status send, rollback inbox |
| Deadline regressions after fix: `pnpm exec vitest run test -t bounds` | Seven passed, 299 excluded by the name filter |
| Held-row proof: `pnpm exec node --test --test-name-pattern='wake, reconnect and safety'` in adapter/test | Named case passed; Node also reports 17 other discovery entries as passes |
| `pnpm typecheck` | Passed |
| `pnpm build` | Dry run passed; `CITY_WAKE (CityWake)` is present in the binding output and Wrangler exits with `--dry-run: exiting now.` |

The pinned Wrangler accepts `[[durable_objects.bindings]]` with `CITY_WAKE`
and `class_name = "CityWake"`, plus `[[migrations]]` with
`new_sqlite_classes = ["CityWake"]`. This is a Durable Object class declaration;
the four D1 migration files are unchanged.

The original gateway replay-table cases grew slower with real object calls
enabled. The test timeout is 120 seconds; tests and assertions in those
original cases are unchanged. An intermediate concurrent run timed out two
existing adapter cases; the isolated full adapter run above passed all cases.

## Every delivery insertion

| Site | Result |
| --- | --- |
| Joined delivery | WAKES after the committed row |
| Ordinary message | WAKES after the committed row |
| Status of a known send | WAKES after the committed row |
| Early status adopted by a send | WAKES after the adoption batch |
| Retry through a historical link | WAKES using committed routing, with the old user id absent from the current phones table |
| Later webhook batch fails | Earlier committed rows still wake; the webhook still returns 500 |

Bad signatures, pending-cap rejection and stranger text attempt no wake.
The successful path adds one SELECT and no write statements. Throwing and
non-settling wake dependencies preserve the committed inbox rows and webhook
answer, produce a count-only `wake_failed` event and increment that counter.
The same bound protects rollback closes on inbox reads.

## Mutation checks

Each mutation was applied separately, its named test failed, and the original
source bytes were restored and checked by SHA256 before the next mutation.

| One-line mutation | Test that failed | Exit |
| --- | --- | --- |
| Remove webhook wake call | `wakes after the message delivery commit` | 1 |
| Remove live-hash filter | `closes a SQL-revoked credential with 4401 without wake while a second credential reads the held row` | 1 |
| Keep link up after missed pong | `held: a missed pong closes the socket within forty clock seconds and resumes polling` | 1 |
| Always schedule the held poll interval | `held: a runtime without WebSocket polls at its original interval` | 1 |
| Remove connect-triggered read | `held: every reconnect reads a delivery whose wake was lost` | 1 |
| Remove rollback close call | `WAKE off closes existing sockets with 4503 on the next message without a wake` | 1 |

Gateway mutations used `pnpm exec vitest run test --testTimeout=3000` plus
the named test filter. Each reported one failed test. Adapter mutations used
`pnpm exec node --test --test-concurrency=2 --test-name-pattern=<name> '*.test.js'`
from the adapter test directory; each reported one failed test and 17 other
file entries passed. No mutation survived.

## Boundaries and compatibility

- The frozen gateway contract diff is empty (zero bytes), including the
  original lines 40–51. The gateway README's Deploy section is unchanged.
- All four D1 migrations, the adapter gateway client, adapter run script,
  contract test, keeper, doctor, pack manifest and adapter package manifest
  are byte-identical to the base: eleven files checked.
- `FAKE_GATEWAY_ROUTES` is unchanged; the held route has its own export.
- A real socket remains accepted after hibernation with only its credential
  hash attached. Local workerd enumerates sockets newest first before and
  after eviction; the fifth connection closes the oldest.
- All received data frames in the new gateway suite are exactly `wake` or
  `pong`. Revoked sockets receive no wake; other cities remain silent.
- The allowed final import and stop-call hunks follow ruling 1. The
  merge-tree checks below cover them with the rest of the change.

## Merge checks

After refetching both heads, at code commit `b13d86f8bb33ae2c0a38216248bfe33b6a1ece3c`:

| Other head | Result | Output |
| --- | --- | --- |
| PR 98: `0b790454653fe4d6de64c057c0ae295b8d4a0057` | Exit 1; six pre-existing conflicts | [merge-pr98.txt](merge-pr98.txt) |
| PR 101: `674a09062c16b5c110c6dc59bf33dd97f077c7e2` | Exit 0; clean | [merge-pr101.txt](merge-pr101.txt) |

Merging the untouched base with PR 98 produces the same six conflicts
([merge-pr98-base.txt](merge-pr98-base.txt)): the gchat and slack-full
changelogs, three slack-full adapter files, and the older WhatsApp changelog
entry under `Unreleased (gp-g02t, PR 76 round 2)` (reaction catalog versus
owner-notice text). The new WhatsApp implementation hunks, including the
import and shutdown call permitted by ruling 1, merge without conflict.
The conflicts are recorded and left untouched as item 21 requires.

## Self-review and Codex review

Fresh self-review at `5a014a703465b05c9de02a2d264270d85cbfe577` inspected
all 25 changed files: 0 BLOCKER, 1 MAJOR, 1 MINOR, 0 NIT.

- MAJOR, failure accounting outside the wake deadline: FIXED in
  `b13d86f8bb33ae2c0a38216248bfe33b6a1ece3c`. The class is
  unbounded work after a bounded best-effort operation. A parameterized test
  stalled only the failure-counter write and reproduced pending responses
  for webhook, adopted-status send, and rollback inbox. The shared helper
  now reserves 100 ms of the two-second budget for accounting and bounds that
  write by the remaining budget. All seven deadline cases pass, including
  normal counter increments after a rejected or stalled notification.
- MINOR, dedup test supposedly acknowledged the held row too early: WRONG.
  `whatsapp/adapter/test/fakes.js:88-91` repeats the last behavior, so `[500]`
  keeps failing every acknowledgement. Strengthened the test with held-row
  assertions across reconnect and safety reads, then allowed acknowledgement
  and verified the post count remained one. The proof passes.

Codex DEEP round 1 reviewed `b13d86f8bb33ae2c0a38216248bfe33b6a1ece3c`
against the recorded base with model `gpt-6-astra`: **VERDICT: CLEAN**,
0 actionable findings. The installed helper validated the anchored verdict
and returned 0. The reviewer read all 28 changed files and traced callers,
storage and error paths; it ran no tests/builds in its read-only sandbox.
The lane's recorded runs are the validation of record. The final evidence
commit changes only this report and merge-output records. The mayor's gate
and Fable read remain pending.

## Public repository check

New fixtures use invented identities and message text. Raw test transcripts
and private coordination records remain outside the PR; this file records
the relevant commands, results and limits without copying them.
The entire added diff was checked for customer data, real phone/account/database
identifiers, credentials, private paths, service URLs and prices. Only invented
test identities, reserved test URLs and the required public documentation link
appear. Public-repo check: done, 0 things changed for redaction.
