# gp-gu49: thread every inbound reply

**BLOCKED — do not merge or install this draft.** The worker's STANDARD
review found a P1 wrong-conversation regression that requires a command
behavior change outside this bead's authorized text-only scope.

The registered Slack Reply line and once-per-channel how-to now prescribe
`--reply-to`. The registered template uses `{thread_ts}` for every inbound.
The how-to retains `--no-thread` as the explicit top-level override.

## Placeholder contract and scope

gc's `renderExtmsgReplyInstructions` fills `{thread_ts}` with the inbound's
thread root, falling back to `{message_ts}` for a top-level message. This is
why one static template starts a thread under a top-level post and replies
at the root of an existing thread. The contract was checked in
`/Users/tailor512/code/gascity/internal/api/handler_extmsg.go` at gc commit
`92a7f383ff69a2b9a995239a6612fe0735c6f09d`.

WeCom does not render the Slack Reply line. Its `inbound.js` how-to and
`index.js` registered template use `gc wecom publish --chat … --text-file …`.
No WeCom files changed. The Slack command's behavior, other adapter message
text (including the coalesced-batch header), and batching logic are unchanged.

## Rendered evidence

- [Before](rendered-before.txt) and [after](rendered-after.txt) show the full
  how-to and Reply line for both inbound kinds, using synthetic channel and
  timestamp values. These are source-derived renderings, with the documented
  gc substitution contract applied; they are not live Slack deliveries.
- The before source is packs base commit
  `7621e7b6a7f1db0a69c181d33b8c4e03f34fbc7a` (`origin/main`).
- [Zero-context diff](change.diff) is `git diff --unified=0 origin/main --`
  for the three changed Slack files.

## Verification

The original three focused tests passed before assertion changes. With the
new assertions and old production text, all three failed for the expected
instruction differences; see [red transcript](tests-red.txt).

The full adapter suite then passed on Go 1.26.5, Darwin/arm64: **1,185
top-level tests, 1,837 passing test/subtest results, zero failures or skips**.
Run from `slack-full/adapter` (a separate Go module; the root Makefile has no
adapter test target):

```sh
env -u GC_TEMPLATE CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c@78/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c@78/lib go test -count=1 -json ./...
```

[Go transcript](tests-go.txt) contains the command, decoded test output, and
counts. Trailing spaces and the trailing blank line are trimmed.

The Python reply-template and reply-current checks passed: **63 passed,
467 deselected**. Run from the lane root:

```sh
env -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest slack-full/tests -q -k 'reply_template_contract or slack_chat_reply_current'
```

[Python transcript](tests-python.txt) has no trailing blank line. The first
attempt with bare `python3 -m pytest` could not start because that Python
environment had no pytest; the isolated uv run above resolved it.

## Review and handoff

The worker's Codex STANDARD review completed from the lane with:

```sh
codex review --base origin/main -c 'sandbox_workspace_write.network_access=true'
```

Round 1 verdict: **changes required, one P1**, preserved in
[the review result](review-standard-1.txt). Full log:
`/Users/tailor512/city/assets/ops/codex-gates/gp-gu49-standard-1.txt`.

The worker independently confirmed the finding with an
[offline reproduction](reproduce-company-routing.py) using existing test
fixtures and intercepted HTTP calls; see [output](company-routing-reproduction.txt).
With a live company turn and no newer mention-only delivery, the old
`--turn-ts` command refuses before posting. The new command requests
`C_INBOUND` / `100.000001` but posts to `C0AAAAAAA` / `1700000000.000100`,
the company pointer's channel and root. No live Slack message was sent.

The guard is in this pack's `slack-full/scripts/slack_chat_reply_current.py`:
`main` calls `_maybe_company_reply` before normal channel/thread resolution;
company routing rejects `--turn-ts` but ignores `--conversation-id` and
`--reply-to`. The spec describes command behavior as core gc's concern, but
this particular guard is pack-owned. Resolving it still requires expanding
the explicitly text-only scope. Alternatively, retaining `--turn-ts` would
violate the required template. Papercut: `pc_db3f738b1f0b`.

The review sandbox's separate full-suite run failed the unchanged
`TestTightenStorePermissions/setgid_bit_preserved_on_dir` case because the
setgid bit was absent there. That case passed in the worker's full suite and
in a [focused rerun outside the review sandbox](tests-review-environment.txt)
(one top-level test, eight subtests). This environment discrepancy is logged
as papercut `pc_bbccc243c44c`.

The mayor receives BLOCKED instead of READY. This draft preserves the patch
and evidence for rescoping; the mayor's gate, Fable read, founder merge gate,
and installation remain pending.
