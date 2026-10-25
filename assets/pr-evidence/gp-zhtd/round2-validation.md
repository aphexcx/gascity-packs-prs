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
