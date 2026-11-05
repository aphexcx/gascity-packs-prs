# PR 77, round 3: completed text retry receipts

Bead: `jp-dvg`. Branch: `jp-n3d`. Prior reviewed head:
`0175bdd9d66139cfe2158a21650d0afcbba98593`.

Completed identical text requests return HTTP 200, the original receipt
(including message ID, acknowledged chunk count, and delivery outcome), and
`replayed: true`. The CLI exits successfully and prints “already delivered
under this key, nothing resent”. This also works after restoring the journal.

Scope clarification from mayor mail `jg-wisp-5l7ukmf` on 2026-11-05: existing
409 responses for ambiguous media/legacy text attempts and idempotency conflicts
remain unchanged. Only completed text replays stop being 409 responses.

## Validation

The tested source commit is `c480d050fad68467116416a627b38ca5bd31c198`.
The following evidence commit adds only this directory; the implementation and
tests are identical to that source commit. Filenames identify the exact HEAD
at execution, avoiding a self-referential evidence-commit hash.

Both commands ran from `wecom/adapter` on Node **v26.7.0**. Full stdout/stderr
was captured with `2>&1 | tee ...` and shell `pipefail` enabled.

- [Dependency setup](deps-c480d050fad68467116416a627b38ca5bd31c198.txt):
  `bash run.sh --deps-only`, exit 0, dependencies ready.
- [Full passing suite](node-c480d050fad68467116416a627b38ca5bd31c198.txt):
  `node --test 'test/*.test.js'`, exit 0; **410 passed**, 0 failed,
  0 cancelled, 0 skipped, 0 todo.
- [Initial suite](node-451fbce38debb039cfa51a87157b075c3ac99ba6.txt):
  407 passed, 3 failed because old process/reconnect assertions still expected
  409 for completed requests. The follow-up source commit updates those
  assertions to verify 200, the original receipt with `replayed: true`, and
  no extra provider frames. Its dependency output is also retained.

The obsolete canned-response `already_delivered` CLI error case was replaced
with a real CLI-to-adapter flow: deliver multiple chunks, lose the first HTTP
response, retry successfully, recreate the publisher from its on-disk journal,
then retry successfully again. The test compares full receipts, checks the
plain success hint and absence of refusal wording, rejects changed text/chat
under the same key, and verifies no additional provider sends. Existing tests
also cover concurrent retries, completed suffix recovery, retained checkpoints,
and actual adapter process reconnect/restart behavior.

`sh -n wecom/commands/publish.sh` and `git diff --check` passed.
