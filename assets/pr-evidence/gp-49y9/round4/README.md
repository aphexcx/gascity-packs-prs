# Coalescing round 4

An enrolled mention now receives its merged turn's delivery observations and
operator resolution under its own notice ID. Header comparison covers the full
specified Unicode invisible/blank class while retaining the original delivered
bytes. A box that sets nothing changes behavior when its adapter updates:
coalescing stays on at eight seconds. Notices remain off unless
`NEVER_SILENT=on` is configured.

All endpoints are fakes. Tools are Go 1.27.3 (darwin/arm64) and Python 3.15.0.
Every Go test unsets `GC_BIN` and `NEVER_SILENT`; enabled notice tests use
engine options and fake senders. No existing test, timeout, wait, default,
pack version or notice-engine source was changed.

## Base and commit order

Both parent checks returned PR 106 **OPEN**, still at
`48c4097b1e48660b3c1373572e71bd7bce44d065`. This is case B: **no merge**, no
conflict hunks. The round 4 base is
`52e91dbe5902fb98daeb8e3ff3150cb80b8d9527`. The two case-A merge comparisons
do not apply. PR 107 remains stacked on PR 106; retargeting is not part of
this change.

| Change | Commit |
| --- | --- |
| Notice test before its fix | `2deb0f6711728144da0d2f688d593ffef84cefca` |
| Act-cap boundary pin before the fix | `3a83c35e66af5bca332dd56f593ef028195078ed` |
| Notice fix | `986c84dc11ca5daaa13fdf11c9699d9dd2a00397` |
| Direct Unicode test before its fix | `43ca22c7ac9d352483e027ea362ce6dae07391b7` |
| Delivered Unicode test before its fix | `3eca8e0366102167616e8c65fbc7e41e37a99ffe` |
| Display comparison fix | `1f38a75eab098c55038f897f44f2b73cfb7a2a56` |
| Operator text | `8d0987d30d026edc170caf75e5206cb8e7b83899` |

## Interleaved app measurement

A is merge commit `be5a7add42870f7795f2197ab96f6f3dbfcafaf7`; B is round 3
head `52e91dbe5902fb98daeb8e3ff3150cb80b8d9527`. Separate archived source
trees ran one command at a time, A then B for each pair, before product edits:

`env -u GC_BIN -u NEVER_SILENT go test ./internal/app -count=1 -p=1 -timeout=30m -json`

| Pair | Tree | Load before | Load after | Exit | Seconds | Failing tests |
| --- | --- | ---: | ---: | ---: | ---: | --- |
| 1 | A | 18.51 | 15.29 | 0 | 546.987 | none |
| 1 | B | 15.29 | 35.33 | 0 | 512.155 | none |
| 2 | A | 35.33 | 31.70 | 0 | 542.420 | none |
| 2 | B | 31.70 | 39.60 | 0 | 521.056 | none |
| 3 | A | 39.60 | 39.98 | 0 | 548.571 | none |
| 3 | B | 39.98 | 44.47 | 0 | 534.008 | none |
| 4 | A | 44.47 | 42.31 | 0 | 549.493 | none |
| 4 | B | 42.31 | 30.62 | 0 | 548.867 | none |
| 5 | A | 30.62 | 31.07 | 0 | 532.016 | none |
| 5 | B | 31.07 | 4.20 | 0 | 524.906 | none |

All five pairs started within 10 load units. A failed 0/5 and B failed 0/5;
the specified threshold of two excess B failures was not met. No app change
was made. Each run passed 81 top-level tests and 142 subtests, with no skips.
This experiment did not reproduce the earlier excess; it does not establish
the cause of those historical failures. [Exact records](app-runs.json).

## Notice observations and resolution

The adapter sends one owner's observation, with the same kind, sessions and
time, to the owner and every distinct member; restart follows the durable
delivery, and operator resolution persists on each member before forgetting
its tracked notice.

`NoticeDeliveryIDs` reads the stored membership under the ledger lock without
cloning the ledger. Disabled notices return immediately. Single deliveries
retain the engine's own observation clock. Unknown IDs remain unknown under
Result/Stop, and member forgetting uses `ForgetTracked`. The owner's original
Forget behavior and deletion's existing tombstones remain intact.

Changed observation call sites in `internal/inbound/inbound.go` are lines
516 (receipt pending), 534 (terminal whole paste), 541 (landed whole), 1256
(refused/unreachable POST), 1358 (gc answer), 1409 (fragment bound), 1412
(no route), 1437 (withdrawal) and 1450 (terminal stop).
`internal/inbound/never_silent.go:72` reconciles each member through
`Ledger.Delivery`, preserving its own retirement/resolution first.
`internal/server/server.go:1782` persists each member's `NoticeResolved` and
line 1799 forgets tracked members before replay. No file in
`internal/neversilent/` changed.

A cold merged `no_route` restart now reconstructs the recorded no-route
answer followed by its definite stop. The baseline owner's control itself
lost the observed kind at that point. This repair applies to merged turns;
ordinary single-message reconciliation is unchanged.

The enabled-engine class test uses the gate's exact byte-cap/503 sequence
for every normal outcome, and an enrolled-owner control with the same
outcome. It compares observed kind, delivered state, failure kind, stop,
uncertainty, removal, notification and owner lines. Both enrolled members
also share observation time and targets in the two-mention row. Comparing
the recorded observations finds identical times and target sessions at all
75 member/control checkpoints ([results](notice-observation-equality.json)). The local
HTTP resolve handler is exercised for accept and replay. Cold restarts load
the saved ledger with only the mention's original enrollment in a new
notice sidecar.

The complete baseline has **26 failing rows and 2 passing pins**; all **28
rows pass** after the fix. [Every verdict](notice-verdicts.json) and [every
member/control record, ambient tombstone and posted line](notice-rows.json)
are retained. Owner-change and deletion rows also fail before the fix because
their earlier observation history was missing; their names are not counted
as passing pins merely because the spec expected that by reading.

The [row summary](notice-summary.md) places each final member record beside
its control. Tuple fields are observed kind, delivered, failure kind, stopped,
uncertain, removed, notified and notice-line count; booleans are 0/1. Its
ambient column lists any Removed record found at any stage of that row.

The act-cap row is a measured pin: the fiftieth ordinary act freezes before
the later mention arrives, even when that first POST is refused. The mention
therefore owns a separate turn. `coalesce.go:187-202` decides that boundary;
the fixture does not invent an unreachable non-owner. The restart-before-
urgent-freeze row uses an older delayed mention with newer ordinary C already
held, saves its admission, then stops before the anchor freezes it. Recovery
selects C and demonstrates the missing member observation.

Ambient records never become live enrollments. Resolution can leave the
ambient owner's existing Removed tombstone, written by the owner's `Forget`;
deletion can do the same through `inbound.go:873`. Member forgetting adds no
ambient record. The per-stage maps in `notice-rows.json` distinguish absence
from a Removed record for every row.

## Display class and original bytes

Only comparison inside `QuoteMergedHeaders` changes. It removes the union
of Unicode `Cc`, `Cf`, `Mn`, `Me`, `Z`, `Other_Default_Ignorable_Code_Point`
and `Variation_Selector`, including ordinary spaces, and compares the compact
from marker and the compact existing header regexp. Thus a control or filler
that could mean nothing or a space is accepted under either interpretation.
Harmless extra quoting such as `[from:Boss` is intentional. `Render`, `Strip`
and `IsFrame` retain their prior bodies.

The single production list of additional symbols is:

| Code point | Reason |
| --- | --- |
| U+2800 | Braille blank cell; no relevant blank/ignorable property |
| U+1D159 | Musical null notehead; conservatively treated as blank |
| U+FFFC | Object replacement placeholder; the object may be absent, so quote conservatively |

The four Hangul fillers U+3164, U+FFA0, U+115F and U+1160 are already covered
by Unicode's ignorable property table. Names and categories were checked
against Unicode 17.0.0; actual glyph rendering was not tested against Google.
Go has no single `Default_Ignorable_Code_Point` table; the union of Cf,
Other_Default_Ignorable_Code_Point and Variation_Selector covers the required
class, with the wider categories added by the ruling.

| Test | Baseline failing leaves | Baseline passing pins | Fixed passing leaves |
| --- | ---: | ---: | ---: |
| `TestQuoteMergedHeadersWholeDisplayClass` | 43,366 | 1,259 | 44,625 |
| `TestFromLineWholeClassIsDelivered` | 108 | 18 | 126 |

The direct test generates every code point from Go's tables, excluding LF
because it ends a line, at the specified marker positions and both headers.
The delivery test uses table representatives in both merged and retried-
singleton paths. The nine named rows are CGJ after the bracket, VS16 after
the bracket, VS18 after the bracket, Mongolian FVS1, Khmer inherent vowel,
Hangul filler, halfwidth Hangul filler, Braille blank, and a live header with
CGJ. All nine fail before and pass after in both delivery paths.
[Per-table counts and every named result](display-results.json).

Each delivered assertion requires the exact bytes the old renderer would
deliver, prefixed by `> `. It also checks that only the adapter's own from
lines remain unquoted.

## Verification records

[checks.json](checks.json) records every invocation, source, exit and count,
including red runs. The three new tests together pass 3 top-level tests and
44,781 subtests (including the delivery test's two form groups).

An additional exact-base check copies only the three new test files into
the archived round-4 base. All 153 original adapter files match that
commit's Git blob hashes. It reproduces the same 26 notice, 43,366 direct
display and 108 delivered-display failing leaves, with 2, 1,259 and 18
passing pins respectively ([counts](exact-base-results.json)). This expected
red run is retained separately from the single full-suite run.

Each new test alone at `-count=20` passes:

| Test | Top-level passes | Subtest passes |
| --- | ---: | ---: |
| Notice class | 20 | 560 |
| Direct display class | 20 | 892,500 |
| Delivered display class | 20 | 2,560 |

The notice `-race` run passes 1 top-level test and 28 subtests. `gofmt -l .`
prints nothing and `go vet ./...` passes.

There was **one full Go run**, at
`8d0987d30d026edc170caf75e5206cb8e7b83899`:

`env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json`

It passes **22 packages, 787 top-level tests and 48,960 subtests** in
1,385.116 seconds. The two expected skips are `TestRealGCContract` and
`TestRealGCReplyReminder`. The 15 named round-2 tests and four named round-3
tests all pass in this run; [their recorded verdicts](prior-named-tests.json)
are extracted from the full-suite log.

From the repository root, the prior environment-clearing Python invocation
passes **465 tests and 250 subtests, with 127 skips**:

```sh
make test 'PYTEST_DIRS=tests gchat/tests' \
  'PYTHON=env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTHON python3'
```

The unchanged round-3 window-off harness runs at the tested head and parent
`48c4097b1e48660b3c1373572e71bd7bce44d065`. Both combined POST/ledger SHA-256
values are
`b66f9b224a75fb8ebdaf9e7605c544b4e243e515af0fdbf448710ce7732853af`;
[the two invocations and results](window-off.json) retain the source SHAs.
No existing test or test timeout changed.

## Three final app runs

All three prescribed runs used
`8d0987d30d026edc170caf75e5206cb8e7b83899`, one at a time, with the same
isolated app command as the interleaved measurement. Subsequent changes only
record evidence; the published gchat source tree is identical to this tested
and reviewed tree.

| Run | Load before | Load after | Exit | Seconds | Failing tests |
| --- | ---: | ---: | ---: | ---: | --- |
| 1 | 23.42 | 14.25 | 1 | 508.884 | `TestObservedIdentityReplacesContradictedLedgerIdentity` |
| 2 | 14.25 | 16.45 | 0 | 512.749 | none |
| 3 | 16.45 | 7.41 | 0 | 495.397 | none |

The result is **2/3 passing**, with 242 top-level passes, 426 subtest passes
and no skips across the three runs. Run 1 has 80 top-level passes and one
failure; runs 2 and 3 each have 81 passes. All have 142 subtest passes.
[Exact records](final-app-runs.json) and
[the complete failure diagnostic](final-app-failures.json) retain the failed
run; no replacement runs were made.

At `internal/app/app_test.go:365`, the first run's POST to the fake
`http://adapter/publish` exceeded the fixture's one-second HTTP client timeout
while awaiting headers. The client deadline is at `app_test.go:93`, and that
fixture disables coalescing at line 83. The same test passes in the full
suite, all ten interleaved runs and the later two prescribed final runs.
Its relationship to this round's changes is **NOT SHOWN**. No timeout or
speculative product change was made, and the failure was reported for the
mayor's handoff. The final app measurement is not an all-green result.

## Fresh DEEP self-review

The one fresh same-model review of this round's delta returned **CLEAN** at
`8d0987d30d026edc170caf75e5206cb8e7b83899`, with no findings. It tried owner
promotion that leaves an enrolled non-owner, an unknown delivery outcome,
withdrawal of every member, matching and unrelated session activity, ambient
enrollment, and mixed invisible/blank characters through both callers.
All five scratch tests and 138 subtests pass on their first run, without
skips. A separate bounded CoreText probe found no additional empty glyph
among present BMP candidates in five local fonts; this does not prove Google
rendering or supplementary-plane behavior.

[The review](self-review.md) records each attempted break, whether executed
or read, and its result. [The review result](self-review-results.json) and
[scratch command/counts](review-check.json) preserve the reviewed SHA and
verdict. To reproduce the scratch Go checks in an archive of that SHA, add
[review-notice.go.txt](review-notice.go.txt) as
`internal/inbound/zz_round4_deep_test.go` and replace that archive's
`internal/inbound/coalesce_display_class_test.go` with
[review-display.go.txt](review-display.go.txt), then run the recorded command.
[The font probe](review-blank-probe.py) and
[its output](review-blank-results.jsonl) are also retained. The mayor's gate
and second-model read remain pending; no fifth round was opened.

## Operator text and limits

One new CHANGELOG bullet is the only operator string changed this round.
[Exact old/new text](operator-text.json). The never-silent and coalescing
README sections and the Google Chat skill contain no sentence made false by
these fixes and are unchanged.

The existing rule for a message waiting behind an earlier delivery remains;
it receives no outcome until its own POST. Record-path cleanup ordering and
mail-mark inheritance are outside this round. So are case/confusable letters,
fullwidth brackets and mid-line breaks after CR, VT, FF, NEL, Zl or Zp.
Live Google group identity, a held create meeting catch-up edits, deployed
image pins and attachment bounds remain unproven. The mayor's round 3 gate
and second-model read were BLOCKED; round 4 addresses their two classes.
Round 4 mayor gate and second-model read remain pending.

## Ruling interpretations

1. An observation reaches each enrolled member with the owner's kind,
   sessions and time. An ambient message stays absent or Removed.
2. Two enrolled messages retain separate observations; the engine's existing
   conversation cooldown still limits notice lines.
3. Waiting behind an earlier delivery, cleanup ordering and inherited mail
   marks remain outside this round.
4. Ambiguous invisible/blank characters are judged both ways through the
   compact comparison; either match causes quoting.
5. Tests enumerate Go's Unicode tables. The one additional production list
   contains the three symbols documented above.
6. Mid-line breaks and visible confusables remain outside the byte-preserving
   prefix rule.
7. Complete tables run directly; representatives and all nine named forms
   run through both delivery paths.
8. All five load-matched pairs count; there are zero excess head failures.
9. Both required parent checks returned OPEN at the same SHA, so no merge
   was required.
