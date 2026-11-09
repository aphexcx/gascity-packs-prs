# Adversarial code review: PR 76 round 2 (bead gp-g02t), the box adapter on the gateway's stable conversation_id

You are reviewing a git branch in the current directory (a read-only checkout of aphexcx/gascity-packs-prs,
branch feat/gp-ac0u-whatsapp-bsuid plus this round). The round's delta is exactly:

    git diff af7e2d9b99461481a94f57ab252269a07f364b15..HEAD
    git log --format='%h %s' af7e2d9b99461481a94f57ab252269a07f364b15..HEAD

Read the whole delta (6 commits) and the files it touches, at HEAD. You cannot run the tests (the sandbox is read-only and has
no network); read the code and the tests and reason. Quote file:line for every claim.

## What the round must do (the bead's scope, condensed)

PR 76's WhatsApp gateway (whatsapp/gateway/src/gateway.ts) routes by the WhatsApp user id, never the phone number.
Its inbox (`inbox()`, gateway.ts around :951-:986) gives every delivery `conversation_id` (the owner's key, fixed at
the bind, never changed: gateway.ts bind around :704-:741; a v1 row reads `COALESCE(conversation_id, wa_id)`), and a
`wa_id` that is only the number the message or the owner's newest pairing shows: NULL for an owner known by user id
alone, a NEW number after a number change, the SAME number for two owners after a recycled number (gateway.ts
:752-:761 message rows, :733-:741 joined rows, :871-:879 status rows). `POST /v1/send` resolves `to` as an owner's
conversation_id, its current user_id, or a number it shows (:1015-:1070), and answers 409 ambiguous_recipient when
`to` names two owners. `GET /v1/window` lists owners by conversation_id (:1168-:1182). PR 73's gateway, which the
boxes run until this gateway deploys, is on main: `git show 2372b4f2:whatsapp/gateway/src/gateway.ts` (its inbox
has NO conversation_id field; its wa_id is the owner's key).

The pack's box adapter (whatsapp/adapter/src) at af7e2d9b keyed every owner by wa_id. This round must make it key
every conversation, ack, journal line, chain, watermark, lookup, probe, dead letter, window-ledger row, keeper bind
and media directory by the delivery's conversation_id, carry the number for display only, acknowledge and render a
delivery with a NULL wa_id, keep a number change in ONE gc conversation, never mix two owners on a recycled number,
and migrate or tolerate the adapter's old journal lines. The adapter rolls to the boxes BEFORE the gateway deploys,
so it must still serve PR 73's inbox. The round also fixes a gateway test (the Worker-entry console test,
whatsapp/gateway/test/gateway.test.ts, must seed the owner with its user id, assert the delivery, and never reach
the network) and gateway README sentences (rule 1 and the data section about which phone number a row keeps; the
Deploy step 3 stop when 0003 already shows applied; the adapter precondition; a stale cross-reference). Nothing in
the gateway's routing may change.

The author's design (verify it, do not trust it): render.js `conversationOf(delivery)` is the one key function
(conversation_id when the field is present, else wa_id for the v1 contract); takeDelivery writes the key into the
journal's received line; every later step reads the fold's `e.conversationId`; a received line with no
conversation_id field (written by the PR 74 adapter) is read as its wa_id; outbound progress entries written
before the round carry `wa_id` and are read by `entryConversation`; the local API (/consent, /hours,
/inbound/clear-gap) and the pack's verbs now take `conversation_id` and answer `conversation_last4`.

## What to hunt for

1. Any place in whatsapp/adapter/src, whatsapp/doctor or whatsapp/commands that still keys, matches, files or
   serializes an owner by the number (wa_id, or a value derived from it), or that derives an owner key by a second
   path that can disagree with the received line's key.
2. The contract boundary: a PR 73 delivery, a PR 76 delivery, a PR 76 delivery whose conversation_id is null or
   empty, a status delivery, a joined delivery; mixed journals across the upgrade (old lines and new lines for one
   owner); a delivery the old adapter journaled and the new adapter resolves (in-doubt lookup, watermark,
   boundary, dead letter, probe); compaction and replay.
3. Cross-path consistency: the id the inbound side records == the gc conversation the mayor replies in == the
   `to` the outbound side sends == the window ledger's row == the keeper's row (the gateway's window
   conversation_id) == the media directory. Find any pair that can differ for a NULL wa_id, a number change, or a
   recycled number.
4. Operator surfaces: can an operator still name each conversation (status shows last four digits; clear-gap,
   consent, hours take the id)? Did the rename break any verb, doctor row or test fixture?
5. The tests: do test/conversation.test.js's six cases really pin the three shapes (NULL wa_id; a wa_id changed
   within one conversation_id; a recycled number under two conversation_ids), the old-journal tolerance and the
   contract boundary, through the real child? Is any assertion vacuous or satisfied by the old code? Was any
   existing test weakened rather than changed with the behaviour (gate.test.js's recycled-number case in
   particular)?
6. The gateway test: does the Worker-entry test now take the bound path, assert the delivery, and assert no
   network call across the webhook, the refused handshake and the cron? Can the fetch mock miss a call?
7. The READMEs (whatsapp/README.md, whatsapp/gateway/README.md), the skill and the fragment: any sentence the code
   at HEAD contradicts.


## Since the r1 read (verify these too; the mayor ruled on them, 12:05Z 11/6)

- THE RECIPIENT (adapter/src/outbound.js, "THE RECIPIENT" and recipientFor/toOwner): each publish and
  update reads GET /v1/window once and sends to the user_id of the entry whose conversation_id is the
  conversation; no entry, or user_id null: not_bound locally, nothing sent; an entry with no user_id
  field (PR 73's gateway): to = the conversation id. The gateway key (textChunkKey/templateKey) stays
  bound to the conversation id. The update path resolves before its window-open check. Hunt: any send
  that can still reach a different owner; a resume after a partial send; concurrency with once()/chain();
  the 409 guard; a window read failure; the CLI transcript path.
- THE KEEPER (adapter/src/keeper.js): knownConversation matches a whole id exactly, and phone-shaped
  text only against the conversation that is its digits; a row the last good window read did not list
  raises nothing and its retries drop (window_ids persisted in keeper.json; null = no read recorded).
- THE GATEWAY BIND (gateway/src/gateway.ts bind): a new binding's conversation_id is the owner's
  user id, never the number; PR 73's number-keyed rows keep theirs. Hunt: any routing, REKEY, unbind,
  floor, link or mark path that assumed a number key for a new binding; the eight cases
  (test/bsuid-orders.test.ts) and every test changed in a69e99a2 (were expectations weakened, or only
  the key renamed?).
- The fold: dead-letter names keep the period; the verbs send conversation_id and wa_id and read either
  last-four key; docs.

Test evidence at the head (run by the lane, not you): the gateway suite 229/229, typecheck 0, dry run 0;
the pack pytest 46/46; the adapter suite 194/195 with one load-wait timeout (a different test in each
of three full runs, each green alone); conversation.test.js 11/11.

## Severity and verdict

- CRITICAL: a message reaches the wrong owner or city, an owner's message is lost, or a reply goes to the wrong
  person.
- MAJOR: a correctness defect on a reachable path (a delivery left unacknowledged that should be filed, a split
  conversation, a broken verb or doctor row, a test that does not pin what it claims).
- MINOR: a defect with a narrow or cosmetic effect, a misleading comment or doc sentence.
- NOTE: an observation, no change needed.

For each finding: severity, file:line, the concrete failure scenario (inputs and state, then the wrong outcome),
and a fix sketch. Do not report style preferences. End with exactly one line:

    VERDICT: CLEAN        (no CRITICAL and no MAJOR finding)
    VERDICT: BLOCK        (any CRITICAL or MAJOR finding)
