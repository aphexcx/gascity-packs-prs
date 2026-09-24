# gp-8tp1 execution plan

Approved scope: the claimed bead gp-8tp1. Work stays in its existing gc lane.

- [x] Reuse dc065d2f by cherry-pick for the template, how-to, tests, and prior evidence.
- [x] Add mixed-session regression cases to test_slack_chat_reply_current.py and observe failure with the existing guard.
- [x] Update _maybe_company_reply in slack_chat_reply_current.py to compare explicit targets with the selected company pointer, honor bound targets through ordinary resolution, and reject unbound targets naming both destinations. Preserve implicit company and mention-only routing.
- [x] Document the guard in slack-full/README.md; retain the placeholder contract and WeCom scope statement.
- [x] Run the full Go adapter suite and Python reply-template/reply-current suites; save commands, counts, rendered text, and zero-context diff here.
- [x] Run the worker STANDARD review and resolve findings. No actionable defects remain.

Delivery contract: commit and push the verified result, create a draft PR, send READY, stamp and close gp-8tp1, then acknowledge drain.
