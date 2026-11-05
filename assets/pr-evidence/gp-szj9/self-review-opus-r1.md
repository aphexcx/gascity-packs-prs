# gp-szj9 self-review r1: PR 74 round 3, 24d75c41..95c9c1b9 (read-only)

**What I read.** The bead gp-szj9 in full, including ADDENDUM 1. The commits e5a7fda6, be93583a and 95c9c1b9 (messages and diffs). All of outbound.js at 95c9c1b9. The adapter's gateway.js, http.js, render.js (renderMessage), inbound.js (takeDelivery, and the post at :535) and keeper.js (readWindow). The round's tests in test/gate.test.js and test/outbound.test.js, and test/fakes.js. whatsapp/README.md and the evidence README.

On the gateway side:
- At ba82ab46: gateway.ts `send` (:1022-1133), `repeatAnswer`, `windows` (:1164-1177), the bind's conversation_id rule (:686-705), the message delivery insert (:731-739), the inbox select (:966), and README rules 1-8.
- At 392f9c09: gateway.ts:56, :746, `windows` (:859-872) and its 409s.

**What I ran.** I ran the round head's archive at /tmp/gp-szj9-selfreview/whatsapp/adapter with `pnpm exec node --test test/gate.test.js test/outbound.test.js`: 37/37 pass.

I wrote one probe, test/probe-selfreview.test.js, and ran it in two trees:
- At 95c9c1b9 it FAILS. Output: /tmp/gp-szj9-selfreview/probe-selfreview-output.txt.
- At e5a7fda6, using the round's fakes.js, it PASSES. Tree: /tmp/gp-szj9-selfreview/at-e5a7fda6. Output: /tmp/gp-szj9-selfreview/probe-selfreview-output-e5a7fda6.txt.

I modified nothing in the worktree.

---

## MAJOR 1: outbound.js:296-310 (sendMessage; the match at :303, the premise at :289): on a recycled number, the resend gives one owner's reply to the other owner

**The defect.** The resend assumes "the box means the owner of its conversation" (:289). It resolves that owner as the GET /v1/window entry whose `conversation_id` equals the adapter's conversation id. At this head, though, the adapter's conversation id is the delivery's **wa_id**, not the gateway's `conversation_id`:
- render.js:118-122 builds the conversation from `delivery.wa_id`.
- inbound.js:535 posts that conversation.
- The gp-ac0u conversation_id keying is deferred (the bead's OUT OF SCOPE).

So in exactly the case where PR 76 answers 409, the adapter's conversation P holds the **new** owner's messages, while the window entry with `conversation_id` P is the **old** owner's.

**The failure scenario** (README rule 7's own example at ba82ab46):
1. Owner A binds on phone P, so A's conversation_id is P.
2. A changes number. A's user_id becomes `US.ownerA2`, and A's wa_id becomes the new phone or NULL.
3. Owner B joins the same city on the recycled P. The bind takes `CASE WHEN P is not already a conversation_id THEN P ELSE user_id` (gateway.ts:697-698), so B's conversation_id is `US.ownerB1` and B's wa_id is P.
4. B writes. The delivery carries `wa_id = COALESCE(from, row.wa_id) = P`, `conversation_id = US.ownerB1` (gateway.ts:737, :966).
5. The adapter files it under gc conversation P (render.js:121).
6. The mayor answers B in conversation P. `gc whatsapp publish --update --to P` behaves the same way: the CLI's `--to` is "the conversation id gc shows in the paste".
7. The gateway sees that `to: P` names A (by conversation_id) and B (by wa_id), and answers 409 ambiguous_recipient (gateway.ts:1054-1057).
8. sendMessage reads the window and takes the entry with `conversation_id === 'P'`. That entry is A's, so it resends to `US.ownerA2`. **B's reply is delivered to A.**

The template path has no window check, so a B update always reaches A. On the text path, if A's window is closed, the resend answers 422. Then:
- sendChunks calls `noteWindowClosed(P)` (:346-348). That closes B's window in the adapter's ledger, although it is open at the gateway.
- The receipt's hint sends the operator to `--update --to P`, which reaches A.

At e5a7fda6, which is 24d75c41's 409 handling, the same message was refused as `gateway_refused` and nothing was sent. **This round introduced the misdelivery.** It is the exact harm the 409 exists to prevent: "a guess hands one owner's message to the other" (ba82ab46 README rule 7). On privacy grounds it could be rated CRITICAL; the trigger is narrow: a recycled number bound again in the same city, with PR 76 deployed.

**Why the round's tests pass anyway.** ADDENDUM tests (c)-(f) put only owner A in conversation P and never deliver B's messages with `wa_id P`, `conversation_id ≠ P`. So the fake never models the delivery shape that makes the 409 happen.

**The proof.** /tmp/gp-szj9-selfreview/whatsapp/adapter/test/probe-selfreview.test.js runs B's delivery end to end through the in-process inbound engine and the fake gc. The fake gateway is set up as ba82ab46 would be: P ambiguous; A's window entry `{conversation_id: P, user_id: US.ownerA2}`; B's entry `{conversation_id: US.ownerB1, user_id: US.ownerB1}`. The mayor's reply and an update both go to the conversation gc filed B's message under.

At 95c9c1b9:
```
PROBE OUTCOME {"filedUnder":"5511900000001","replyDelivered":true,"updateDelivered":true,"metaRecipients":[["text","US.ownerA2"],["template","US.ownerA2"]],"sendTos":["5511900000001","US.ownerA2","5511900000001","US.ownerA2"]}
✖ probe: the mayor's reply to B's message (filed by this adapter under conversation P) never reaches owner A
  AssertionError [ERR_ASSERTION]: B's reply and B's update must never reach owner A (A_USER); Meta was asked to send: [["text","US.ownerA2"],["template","US.ownerA2"]]
```

At e5a7fda6, the same probe with the same fakes:
```
PROBE OUTCOME {"filedUnder":"5511900000001","replyDelivered":false,"updateDelivered":false,"metaRecipients":[],"sendTos":["5511900000001","5511900000001"]}
✔ probe: ...
```

**Fix sketch.** The resend is right only once the adapter's conversation id is the gateway's `conversation_id`. Two options:

1. **Land the keying first.** File each delivery under `delivery.conversation_id ?? delivery.wa_id` (gp-ac0u's keying) in render.js, the journal and the ledger. Then the resend to the conversation's `user_id` names the owner the box means.
2. **Until then, do not resend.** Answer 409 ambiguous_recipient as a permanent refusal with its own `reason: ambiguous_recipient` and a hint. The hint says the number now names two owners and the reply must be addressed to the owner's user_id.

Either way, add a gate test where B's delivery (`wa_id` P, `conversation_id ≠ P`) is followed by a reply in the conversation it was filed under. Assert the reply never reaches A's user_id.

---

## NOTE 1: outbound.js:129 (templateKey binds `language` from WHATSAPP_UPDATE_LANGUAGE)

Suppose an update's first attempt gets no answer (the gateway may have sent it; sendUpdate saves no progress on a failure). The operator then changes WHATSAPP_UPDATE_LANGUAGE and restarts. The retry uses a new gateway key, so a template that did go out is sent again. At 24d75c41 the key `[K, text]` was language-free. The bead's SCOPE item 1 specifies this operand, and a different language is arguably a different message, so this is a NOTE only. Name it in the PR body, or leave `language` out of the key, since the store does not fingerprint it either.

## NOTE 2: outbound.js:128-129 (the key shape changed under an in-flight publish)

Take a publish that was mid-flight at 24d75c41: progress `delivered: 1`, and chunk 1's outcome unknown. After an upgrade, it resumes chunk 1 under the new key `text:<sha256 [K, to, text]>:1`. The gateway has not seen that key, so a chunk 1 that was sent goes out twice. The same holds for an update whose first attempt got no answer. This is moot if no adapter at 24d75c41 (or earlier) has run against a real gateway; PR 74 is a draft. Worth one line in the PR body.

## NOTE 3: test/outbound.test.js:370-371 (a vacuous-capable assertion on a line the round rewrote)

`cAt = findIndex(... === textChunkKey('gc-C', OTHER, 'C', 0)); assert.ok(cAt < 3)` passes when `cAt` is -1. If the key shape moved again and this line were not updated, the test would still pass. The round updated the line correctly. Assert `cAt >= 0 && cAt < 3`.

## NOTE 4: outbound.js:300-305 / failure() :237-283 (an unresolved ambiguous_recipient carries no reason or hint of its own)

A second 409, or a window entry without a user_id, ends as `reason: gateway_refused`, `gateway_status: 409` and no hint. The mayor's session and the CLI cannot tell "this number names two owners" from any other refusal. This is per the bead ("gateway_refused as today"). If MAJOR 1 is fixed by refusing, this is where the dedicated reason belongs.

---

CRITICAL 0 / MAJOR 1 / MINOR 0 / NOTE 4
VERDICT: BLOCK
