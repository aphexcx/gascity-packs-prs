# Round 2 validation

Base: `986cbad3604f0be455b8c29677be3b6760cff4ae`, fetched as aphexcx from
`origin/gp-zhtd`. PR 62 was OPEN and draft at that exact head on claim. The
lane's `.gc/stack-base` records reuse of `gp-zhtd`, originally made from
`refs/remotes/origin/jp/slack-inject-compact` at `52cbd2f8`. Branch and bead
metadata matched. The physical lane boundary and single live writer checks
passed. Sinan's three commits and the earlier six worker commits are unchanged.

Both mayor review records were read in full before edits. Only this round's
delta is submitted for the next reviews; the base is pinned rather than the
moving remote ref.

## Regressions

- `round2-peer-red.json`: on the base production code, failed immediate peer
  forwarding followed by a human, peer or batch delivery failed all three
  subcases. The event claim was committed after the 503. Ambient churn also
  demonstrated why recovery needs a separate queue. `round2-regressions-green.json`
  passes those cases and the four unchanged full-mode golden envelopes.
- `round2-pagination-red.json`: root plus 400 replies required **21 requests**
  at the base. The same fixture takes **one request** with 500-message pages,
  retaining root plus the newest 19 replies. The endless-cursor fixture stops
  at **four requests**. The real five-second deadline, HTTP 429 and Slack
  `ratelimited` cases retain the root and newest reply in hand, label context
  incomplete and leave the delivered-context watermark unchanged.
- `round2-recovery-budget-green.json`: recovery survives reminder-budget
  deferral and another failed delivery on human, peer and batch paths; older
  queued entries are delivered first within the configured budget. The initial
  budget-test failure was a fixture issue: a long uninterrupted string was
  redacted as credential-shaped input, so it was replaced with ordinary words.

## Suites and measurement

Go: `go version go1.27.3 darwin/arm64`. Python: 3.13.13 in uv's dependency
environment, with pytest, PyYAML and jsonschema imports verified. This change
does not use Node or pnpm. No unsupported-engine warning occurred.

- `python-tests.json` now records **822 passed, 2 skipped, 2 warnings** for
  `uv run --python 3.13 --with pytest --with pyyaml --with jsonschema python3
  -m pytest tests -q` from `slack-full/`. The warnings are the existing
  fork-in-a-multithreaded-process deprecations.
- `round2-adapter-vet.json` and `round2-cli-vet.json`: `go vet ./...` passed
  from each module's own directory.
- `round2-replay-prepare.json` reruns `replay_minimal_injections.py` against
  the same private 575-block baseline; `round2-replay-render.json` passes the
  production-renderer replay, with 657 members and **321,589 bytes**, unchanged
  at **12,761.47 estimated tokens/day**. Historical inputs have no failed-forward
  state; recovery adds bytes only when needed. No transcript text is committed.

Every invocation records its exact command and exit code in
`round2-completion.txt`; individual JSON manifests include test counts. The
base `head` recorded by precommit checks identifies the checkout ancestor;
the checks run against this round's modified working tree, including the new
regression tests. Raw logs remain in ignored lane scratch.

Self-review pass 1 found two edge cases: the larger pages exceeded the old
1 MiB response cap, and an individually oversized recovery could block the
queue under a small configured budget. Both were reproduced red in
`round2-self-review-red.json`. The response allowance is now bounded at 32 MiB;
oversized first recoveries use a budget-relative excerpt with a read-channel
marker while preserving the original body for any failed delivery retry.

The first full race run passed 1,400 top-level tests (12,710 with subtests),
one replay skip. Its initial manifest parser incorrectly read a synthetic
log phrase as a pytest failure count; Go counts now come exclusively from Go
test JSON events, and that spurious field was removed. The later final race
run covers the self-review fixes as well: `go test -race -count=1 -json ./...`
from `slack-full/adapter` passed **1,402 top-level tests, 12,712 with subtests**,
with one opt-in replay skip and no failing tests. See
`round2-adapter-race-final.json`. Adapter vet passed again after these fixes;
the fresh same-model review's second pass is CLEAN (`round2-self-review-r2.md`).

The founder's first-head read, mayor gate and Fable round 2 are handoff gates.
The PR remains draft; no live adapter mode, process or prompt is changed.

The first Codex gate (`round2-codex-review.md`, base 986cbad3 through 1418310d)
returned BLOCK for one full-mode regression: the non-evicting recovery queue
was populated in both modes, but only minimal mode drained it within budget.
`TestFullPeerFailuresRetainAmbientEviction` reproduced this red; the fix marks
failed forwards for the separate queue only in minimal mode. Full mode keeps
its original capped ambient FIFO, preserving its existing byte-format and
eviction behavior. The final review and verification records follow this fix.

At code head `10a9c9122ec11c54f7cb5b598d841b30b7653804`, the complete adapter
race suite passed again: **1,403 top-level tests, 12,713 with subtests**, one
opt-in replay skip, no failures (`round2-adapter-race-after-gate.json`).
Adapter vet passed again (`round2-adapter-vet-after-gate.json`). The focused
self-review of the gate fix is CLEAN (`round2-self-review-r3.md`). Python and
CLI code were unchanged after their recorded passing checks.

The second Codex gate is CLEAN for the complete round-2 delta from 986cbad3
through 10a9c912 (`round2-codex-review-r2.md`). Its one minor finding was also
fixed: recovery excerpts now reserve space for their marker below the peer
formatter's 2,000-byte cap and check the final rendered length. The boundary
fixture reproduced the 10-byte overflow at 2,005 available text bytes, then
passed with the fix (`round2-minor-red.json`, `round2-minor-green.json`). The
small follow-up is reviewed separately against 10a9c912; no earlier code is
outside the two scopes.

At final code head `941f4d9626e3dea8bd1e797181ee38e27cd886a9`, the full adapter
race suite passed **1,404 top-level tests, 12,717 including subtests**, with one
opt-in replay skip and no failures. The exact command was
`go test -race -count=1 -json ./...` from `slack-full/adapter`, recorded in
`round2-adapter-race-final-head.json`. Adapter `go vet ./...` passed at the
same head (`round2-adapter-vet-final-head.json`). The completion manifest was
read back before reporting these results. The final focused self-review is
CLEAN (`round2-self-review-r4.md`). `round2-history-final-code.json` verifies
the required trailers on every new code commit, preserved ancestors and
unchanged full-mode fixtures.

The final Codex gate is **CLEAN with no actionable findings** for
`10a9c9122ec11c54f7cb5b598d841b30b7653804..941f4d9626e3dea8bd1e797181ee38e27cd886a9`
(`round2-codex-review-r3.md`, helper exit 0 with an anchored verdict). Together
with the CLEAN full-round gate through 10a9c912, these reports cover every
round-2 code change. The last commit records only verification and review
evidence; its production tree matches the reviewed and fully tested 941f4d96.
The mayor's independent gate, Fable round 2 and Afik's read remain pending.
