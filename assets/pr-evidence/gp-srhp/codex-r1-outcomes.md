# Codex round 1 (class DEEP): outcomes

The lane's own round, by the gate helper's review form as the newest merged ticket of this repository used it (codex-gate.sh review, the review rules read from REVIEW.md on main `926f17912914d9470b4ef477cc65e9fb70792b56`), at head `f4e0368f6c198f38830040bd9fcc17ba4413d749`. Model gpt-6.2-astra through the hub. Verdict **BLOCK**: CRITICAL 4, MAJOR 6, MINOR 4. The answer as given is in [codex-r1.txt](codex-r1.txt) (local paths redacted). The fixes are in `e27b2d0390d9a743865e7f80e11829d3d4b7aa3d`.

By the template's review rules: each blocking finding is named by its class; every case of the class in the diff is fixed and one class-level test added; a finding is fixed only after a test shows it; every finding gets one outcome.

| # | Grade | Class | Outcome |
|---|---|---|---|
| 1 | CRITICAL | A one-time code leaves for a party that need not read it (the arrival reaction's provider, `ACK_REACTION=jev`, off by default) | **FIXED** in `e27b2d03`. Every case: the owner page's 12 characters and the 10 of a join's or removal's approval. A text that reads as any of them is never sent to the provider (`config.AckRedact`, wrapping the shared redaction, which a Python script outside this pack also reads and is left as it is). Class test `TestChatOwnerCodesNeverReachTheReactionProvider`, red first: `"ABCD-EFGH-JKLM" reached the provider`. The ordinary turn and its record keep the text: **INTENDED**, the contract's C2 ("in the owner's message, which the pack delivers and records as any message") and ruling 8 of the ticket. |
| 2 | CRITICAL | A restored backlog names the sender of a knock | **INTENDED**. It needs the adapter's own state written by the mayor's user, and that user can knock with any sender's id directly: the contract's C13, WHAT IT DOES NOT CLOSE, FIRST ("Nothing stops the mayor's own user from knocking with another sender's id and a real code"). An honest backlog holds the sender Google gave. |
| 3 | CRITICAL | A saved frame is reused after the owners change (deletion notices, held, retried or replayed turns) | **DOES NOT MATTER** for who may ask: a frame is the sender's class when the message was received, as at base for every change of owners (a settings reload included); no message received after a removal is framed owner (`TestChatOwnerOneFunctionInbound`, "the copy stops naming the owner"). Framing again at delivery would change the delivery path of every turn, beyond this ticket. Named for the mayor. |
| 4 | MAJOR | A restored backup revives a removal's spent request | **INTENDED**. The contract's C5: "The requests of C6 stay in the adapter's state and are treated there exactly as the join's requests are today"; ruling 10: restore and reset are unchanged. A restore is the mayor's user's act, and that user can ask the door to remove directly (C13). |
| 5 | MAJOR | A yes's owner is checked only before later I/O, not at the write | **FIXED** in `e27b2d03`. Both cases: the join's one write and the removal's one remove ask the same one function again right before it (`checkOwnerYes` returns that check). Class test `TestChatOwnerApproverAskedAgainAtTheWrite` (removal and join rows), red first: the remove line was sent and the join's write was made. |
| 6 | MAJOR | The knock vouches with an identity Chat has contradicted | **FIXED** in `e27b2d03`: no knock while the client reports a conflict, and never for the observed account. Test `TestChatOwnerConflictedIdentityMakesNoKnock`, red first: the take was sent. |
| 7 | MAJOR | The adapter's status rides in the sender's text field | **FIXED** in `e27b2d03` for the cases the contract lets the adapter change: a knocked message is never merged with its neighbours, so its fixed line stays the last line of its own turn (`TestChatOwnerCatchUpClaimTurnIsItsOwn`, red first: the line sat mid-turn); history leaves the line out (`TestChatOwnerHistoryLeavesOutTheFixedLine`, red first). A sender who types a lookalike line in another message: **INTENDED**, the contract's C4 ("Every other message is untouched by this section"); the skill says who is an owner is the from line's word. Named for the mayor. |
| 8 | MAJOR | Redelivery knocks again and loses the first result | **INTENDED**: ruling 17 of the ticket ("No exactly-once is promised ... a second delivery knocks again"); `TestChatOwnerDeliveredTwice`. |
| 9 | MAJOR | A removal's yes may carry an attachment | **INTENDED**: the contract's C6, the check of a yes "is the join's, rule for rule", and the join's own rules do not change (ruling 1); the yes still carries the request's code alone as its text. |
| 10 | CRITICAL (PRECONDITION) | File metadata taken as proof that the owners did not change | **FIXED** in `e27b2d03`: the copy is read at every answer; the cache and `Refresh` are gone. Class test `TestChatOwnerReadsAnUnchangedLookingCopy`, red first: a copy rewritten in place was not read again. |
| 11 | MINOR | A root-decided yes waits on the directory | **FIXED** in `e27b2d03`: where root's copy decides, no address is looked up. `TestChatOwnerRootApprovalAsksNoDirectory`, red first: one directory request. |
| 12 | MINOR | Removal requests are never pruned | **INTENDED**: "exactly as the join's requests" (C5), which are never pruned either. |
| 13 | MINOR | Older tests read the host's fixed directory | **FIXED** for the join's fixture (`setupJoin` names an absent directory of its own; five lines added, none removed). The others are **PRECONDITION**: a test host that has `/run/gchat-owner-claim`, which only a box has. |
| 14 | MINOR | The log test's message name carried the code | **FIXED** in `e27b2d03`: names that say nothing of their text, and the code looked for in any case. |

## The red runs of the class tests, before the fixes

Each class test was run before its own fix, with `go test -count=1 -run '<the tests>'` on its package (the first six at `f4e0368f` plus the tests alone; the history test after the others' fixes, before its own):

```
--- FAIL: TestChatOwnerReadsAnUnchangedLookingCopy
    owners_test.go:248: a copy rewritten in place was not read again
--- FAIL: TestChatOwnerCodesNeverReachTheReactionProvider
    owner_code_test.go:32: "ABCD-EFGH-JKLM" reached the provider (row {eyes 👀 })
--- FAIL: TestChatOwnerConflictedIdentityMakesNoKnock
    owner_claim_test.go:449: knocked with a conflicted identity: [map[code:ABCDEFGHJKLM message:spaces/dm/messages/m1 request:take sender:users/1001]]
--- FAIL: TestChatOwnerApproverAskedAgainAtTheWrite
    owners_test.go:453: answer map[... result:removed], remove lines [map[message:spaces/dm/messages/yes-1 owner:users/333 request:remove]]
    owners_test.go:463: join map[... result:joined], writes [{POST spaces/room/members users/123456789}]
--- FAIL: TestChatOwnerRootApprovalAsksNoDirectory
    owners_test.go:479: the directory was asked 1 times for a root-decided yes
--- FAIL: TestChatOwnerCatchUpClaimTurnIsItsOwn
    owner_claim_test.go:484: the fixed line is not the last line of its turn: ["[catch-up: 2 messages, oldest first; ...]" "[from: ... | external | item gchat:spaces/dm/messages/c1]" "abcd-EFGH jklm" "[gchat: this message has the shape of an owner code and no owner code is live; a new one comes from the owner page]" "[from: ... | external | item gchat:spaces/dm/messages/c2]" "and one more thing"]
--- FAIL: TestChatOwnerHistoryLeavesOutTheFixedLine/the_adapter's_line
    owners_test.go:589: history = true "abcd-EFGH jklm\n[gchat: this message has the shape of an owner code and is not the live one]"
```
