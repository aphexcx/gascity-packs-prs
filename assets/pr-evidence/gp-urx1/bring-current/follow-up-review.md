# STANDARD self-review: bring-current follow-up

Reviewed code SHA: `920210cd6b3d2c38934d7149fcc60240be97ea04`.
The follow-up changes evidence only. Final evidence-commit identity belongs
in the handoff and provenance record, avoiding a self-referential commit SHA.

Source-resolution findings: **0 BLOCKER, 0 MAJOR, 0 MINOR, 0 NIT**.
Acceptance status: **accepted under the ruling for one full-suite cancellation**.
This is a native self-review, not a Codex gate or a cross-model review.

The review read the remerge diff and changelog against both parents; all six
automatically merged files in full against each side's changes; the single
WAKE parser and its caller; the existing PR body and its prepared update; and every current
evidence file differing from the automatic merge. No source was modified.

| File | Review result |
| --- | --- |
| `whatsapp/CHANGELOG.md` | One conflicted hunk: both complete sections retained word for word, PR first, main order preserved. Relative to main, only the PR section was added. |
| `whatsapp/adapter/src/inbound.js` | The PR's announced-read/unannounced fallback and main's notice admission/result calls are both retained. No source-level clash. |
| `whatsapp/adapter/src/index.js` | One held-switch parse and startup warning coexist with notice configuration, publisher wiring, reactions and quoted replies. The warning precedes the first tick. |
| `whatsapp/adapter/test/fakes.js` | Inbox failures, quote headers, registration controls and delivered-byte overrides coexist; their test callers retain the matching fields. |
| `whatsapp/gateway/test/wake.test.ts` | Main's adopted-status replay test and all 18 PR switch cases remain; no duplicate names. |
| `whatsapp/gateway/README.md` | Migration 0005 precedes the current gateway deployment; held rollback separately closes sockets before removing the binding. The two procedures do not conflict. |
| `whatsapp/README.md` | Held-switch and polling rows coexist with quote, reaction and notice documentation without changing those features' meaning. |

`WAKE` is read at `whatsapp/gateway/src/index.ts:28`, passed to the single
parse at `src/wake-client.ts:15`, and consumed through `enabled`; the search
found no competing comparison in `src/gateway.ts`.

## Prior finding and current acceptance exception

The initial review's F1 at
`whatsapp/adapter/test/never-silent-held-link.test.js:128` recorded a real
timeout but did not establish a production defect. The merge-only inference
from three runs is **WRONG after the larger comparison**: main reproduces
the same timeout in 2/20 runs, versus 3/20 at the merge. Row b preserves
production behavior, every assertion and every timeout. The timing issue was
reported separately. This does not claim that the test cannot fail again.

The current full suite passed C but cancelled one audio crash case at
`whatsapp/adapter/test/files-out.test.js:432`. All six required whole-file
isolations passed 57/57. No production defect was reproduced, and no source
fix is justified by those results. The acceptance ruling on 2026-11-13 waived
the 431/431 condition for this one cancellation, which exceeded its deadline
by 44.6 ms. The full run remains recorded as 430 passes and 1 cancellation.
Outcome: **DOES NOT MATTER for this merge under the explicit ruling**;
the timing sensitivity was filed separately and no source or test was changed.

Evidence was checked against the completion manifests and log summaries.
No private home path, private work identifier, real chat or phone identifier,
secret, or customer/person name was introduced in these evidence changes.
