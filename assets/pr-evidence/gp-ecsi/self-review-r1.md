# Codex lane self-review r1 — gp-ecsi

2026-11-07. Reviewer: the implementation lane's Codex gpt-6-astra session.
Scope: the five changed Go files against
`727f921460ed4673321e7ef7f5532ce236e042e6`, before the source commit.

Reviewed the current diff and the relevant lookup, recording, classification,
provenance, and fake-gc validation paths. No source finding remains in this round.

- M1: ResourceDenied now recognizes Google's specific resource-denial sentence
  positively. Classify still maps explicit membership denial to not_found.
  API-disabled, policy, project, bare, and empty 403s remain auth failures; inbound
  returns ErrCredentials and Nack before recording the event. The recovery test
  verifies successful, durable, deduplicated delivery after retry.
- The prior bare-403 expectation in TestUnavailableSpaceIsRetained was itself
  an assertion of the buggy behavior. It now expects a hold; the existing
  TestRetainedLookupFailureContinuesBatch cases retain both recognized denials.
  Catch-up's recognized resource-denial test is unchanged.
- M2: the e2e order assertion requires a provenance frame, removes precisely
  that prefix using FrameOf, and still checks the exact body, deletion state,
  unresolved set, and turn count. Adapter behavior is unchanged by this test fix.
- N2: both attachment fixtures now carry a resource Name. A separate fake-gc
  test checks that an empty provider ID still gets HTTP 422 without delivery,
  then succeeds when the ID is supplied.
- No dependency, registry, other pack, or pre-existing commit is changed.
  Matching the established resource-denial text is intentionally conservative:
  unfamiliar Google wording holds work for repair rather than discarding it.

Validation available at review: the new regressions failed on the old classifier
as intended; vet, formatting, Python tests, and diff whitespace checks passed.
The full Go suite is still running and is a required publication check. The
changed packages passed. The unchanged puller quarantine-recovery test reported
one-second acknowledgement timeouts in its three scenarios; its first-run log
is being retained and isolated repetitions are running. This is not recorded
as a green full run.
The installed Codex helper will review the committed delta separately.

VERDICT: CLEAN
