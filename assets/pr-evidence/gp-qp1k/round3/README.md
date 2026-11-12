# Registration-independent outage watches and unattributed replays

Round 3 starts at `708ce3b87df69f6d0546d0232fe429b32d958b8b` and updates
the existing draft PR. All fixtures are invented. No production adapter state
or conversations were read or changed.

## Fixes

G1: Google Chat owns one never-silent engine and poll for the service lifetime.
It obtains the notice provider client before registration, but installs normal
Chat routes and starts inbound work only after registration succeeds. Slack
starts its existing watch before the unchanged registration barrier and
ten-minute registration budget.

G3: an explicit gateway replay without a matching accepted attempt ID returns
no activity. The speculative retry attempt is retired. Text, captioned-file and
template receipts still succeed; fresh sends and identified original attempts
retain their existing activity attribution.

The deferred G2 activity-state write reconciliation is unchanged. Owner copy,
operator mail bodies, retry timings, switches and gateway behavior are unchanged.

## Red-first regressions

- `TestNeverSilentStartsBeforeRegistration`, in each Go adapter: two restart
  rows fail at the base with zero owner lines and zero operator mails, despite
  repeated registration 503s. Both rows pass after the fix with exactly one
  owner line and one operator mail total. The second restart proves the marks
  remain effective, and normal work remains gated by registration.
- WhatsApp's process-level registration-503 guard passes both restart rows
  before and after the changes, with exactly one owner line and one operator
  mail total.
- WhatsApp's switch-enablement class adds 27 rows: three send kinds, three
  causes (definite failure, uncertain stop, health), and three attribution
  cases (switch initially off, initially on, unrecognized attempt ID). The
  18 unattributable rows fail at the base, suppressing the newer request's
  owner line and operator mail; the nine original-attempt controls pass.
  All 27 rows pass after the fix, including recovery across another restart.
- The filtered Node runner reports 45 entries: red 26 pass / 19 fail (the
  failing parent plus 18 rows); green 45 pass / 0 fail. This includes discovery
  entries for files with no matching test. The complete class file passes
  68 tests with no failures or skips.

The `*-red.log` and `*-green.log` files retain these runs. Paths in stack traces
are repository-relative. Each manifest records its command and exit status.

## Fresh DEEP review

One fresh native Codex reviewer read only this round's delta at
`3a5ec053556da0f1bc77147e089afec24f12f9c8`. Its verdict was BLOCK with one P2
finding: the new poll could reserve an owner line while the provider client
was still unavailable, permanently losing that line after credentials arrived.

FIXED in `69c90d2dcb43e434b93cbc05e18b5ad6b10e699a`: the engine checks owner readiness before reserving either
owner mark; outage tracking and operator mail continue while credentials are
missing. `TestNeverSilentCredentialsArriveAfterFirstPoll` reproduces the finding
red at the reviewed commit: operator mail 1, owner line still 0 after credentials
arrive. It passes after the fix: mail 1 before credentials; owner line 1 after
credentials, while registration still returns 503. A real refused or uncertain
provider post keeps the existing reservation semantics.

No other in-delta findings were reported. The final gate and independent read
remain the coordinator's handoff steps.

## Verification

`verification.txt` and completion manifests record whole-suite counts, failures
and isolated reruns. Go is 1.27.3; Node is v24.21.0 from
`pnpm exec node --version`. No unsupported-engine warning was observed.
Both modules ran `go vet ./...`; `make test PYTEST_DIRS=whatsapp/tests` passed
57 tests. Slack's formatting check reports only the unchanged
`company_thread_ambient.go`, also documented before this round.

The first Google Chat run exposed two intermediate lifecycle issues, corrected
before the final run: normal Chat clients were installed before registration,
and an inbound unit test still expected its processor to drive notice polling.
The correction run passes all 17 test events, including the registration guard,
the new restart case and the affected inbound test.

The optional installed-gc contract test also failed with HTTP 422, "no active
binding for conversation", before any adapter publish callback. That test and
the gc client are unchanged by this round. The final fake-only full run unsets
the inherited `GC_BIN`, leaving real-gc opt-in tests at their default skip.

Final whole-suite results (test-event counts include subtests):

| Run | Passed | Failed | Skipped | Isolated rerun |
| --- | ---: | ---: | ---: | --- |
| Google Chat, all packages | 1,892 | 2 | 2 | Both failed tests pass |
| Slack, all packages | 13,187 | 1 | 3 | Failed test passes |
| WhatsApp, whole adapter | 327 | 1 | 0 | Failed test passes when other suites finish |
| WhatsApp, class file alone | 68 | 0 | 0 | Not needed |
| WhatsApp Python | 57 | 0 | 0 | Not needed |

The final Google Chat failures are unchanged
`TestMissingTokenReplacementResumes` (five-second readiness wait) and
`TestShutdownSavesBeforeDrainingStuckPublish` (checkpoint deadline).
Their isolated run passes 2/2. Slack's unchanged
`TestRegistrationStartupRetriesCityNotRunning` missed its two-second startup
window in the whole run and passes alone. WhatsApp's previously documented
`(6b)` receipt-recovery case timed out waiting for acknowledgement in the whole
run and in a filtered rerun while Go suites were still active; after all other
suites exited, it passes (18 runner entries including unmatched discovery files).

Google Chat's complete end-to-end package passes, including all 3,000 recovered
messages. The final full Go run was at the implementation revision
`69c90d2dcb43e434b93cbc05e18b5ad6b10e699a`; subsequent changes are evidence only.
No unrelated deadlines or production behavior were changed for these reruns.
