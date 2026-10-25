# Fresh same-model review, gp-qwc2

Reviewer: fresh `self_review` subagent, inherited gpt-6-astra model.
Scope: whole delta against `f9e1dd8a217ae794337b1c4b64ad4582732302ac`,
including new tests/goldens and docs. Read-only in the implementation lane;
the reviewer did not run suites, builds, or write files.

## Round 1 findings

P2: The fitter reserved an omission marker before checking whether all compact
quote lines fit. Two small quotes at a 20-byte budget could therefore become a
truncation marker and omission count. Fixed by checking the complete compact
window first; a regression expects both quotes to survive.

P3: Replay partial-context counts only recognized omission markers. A clipped
lone quote has a truncation marker without an omission count and was missed.
Fixed with `replayContextOutcome`, the composer's unchanged-preamble signal,
and regressions for lone clipping, baseline whole omission, and unchanged
quoted marker text.

The reviewer independently confirmed that the historical window's unchanged
totals are credible: its 336 context-bearing single-unit rows are at most
2,951 bytes, so the sample does not exercise context fitting.

## Recheck

Reviewer response: CLEAN. Both findings are resolved. Compact windows retain
all quotes when they fit without framing. Replay classification counts clipped
lone quotes as partial and ignores unchanged quoted markers. Regression
coverage is present. No remaining findings across source, tests, goldens,
and documentation. Read-only review; no suites or builds run.

The author also corrected files-only separator accounting: measuring the
protected unit with context present reserves its attachment separator. A
files-only regression checks the budget and preserved attachment description.
The reviewer confirmed the source correction and requested ASCII fixture text:
the first CJK fixture left two spare bytes at a rune boundary, masking the old
bug. The fixture now uses ASCII, exposing 102 bytes before versus 100 after.

## Codex measurement correction recheck

After external Codex round 1, the same reviewer checked the batch replay fix
against `ab95b2cdd394f17c741aaf59b90fbbeba7457a26` read-only. Verdict: CLEAN.
Member anchors are removed and `formatCoalescedBlock` supplies production
framing. The parity test compares replay byte totals with an actual delivery
capture. The baseline overlay preserves the original composer behavior.
Corrected before/after full totals both equal 482,843 bytes. No new findings;
no builds, tests, or writes performed by the reviewer.
