# gp-8tp1 execution plan

Approved scope: the claimed bead gp-8tp1. Work stays in its existing gc lane.

- [x] Reuse dc065d2f by cherry-pick for the template, how-to, tests, and prior evidence.
- [x] Add mixed-session regression cases to test_slack_chat_reply_current.py and observe failure with the existing guard.
- [x] Update _maybe_company_reply in slack_chat_reply_current.py to compare explicit targets with the selected company pointer, honor bound targets through ordinary resolution, and reject unbound targets naming both destinations. Preserve implicit company and mention-only routing.
- [x] Document the guard in slack-full/README.md; retain the placeholder contract and WeCom scope statement.
- [x] Run the full Go adapter suite and Python reply-template/reply-current suites; save commands, counts, rendered text, and zero-context diff here.
- [x] Run the worker STANDARD review and resolve findings. No actionable defects remain.

Delivery contract: commit and push the verified result, create a draft PR, send READY, stamp and close gp-8tp1, then acknowledge drain.

## Round 4: gp-1rqe

Authorized by the claimed gp-1rqe description; continue gp-8tp1 and draft PR #44.

- [x] Reproduce the coalesced-header, DM/MPIM no-thread, and whitespace defects at bcc16ff7 with tests only.
- [x] Render root-or-own-ts reply-to anchors; retain the newest human anchor even when a bot reaction follows.
- [x] Preserve matching top-level company DM/MPIM routes; verify conflicting unbound targets still refuse.
- [x] Update the compatibility wording, normalize reply-to, and document the header rule.
- [x] Record rendered evidence, RED/GREEN results, full suites, and a zero-context diff.
- [x] Finish Codex STANDARD review and resolve actionable findings; no actionable regressions.
Delivery contract: commit, push gp-8tp1, update existing DRAFT PR #44, send READY, stamp and close gp-1rqe, and acknowledge drain.
