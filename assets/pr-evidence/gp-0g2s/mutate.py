"""Mutation check for the gateway suite at gp-0g2s: one mutation at a time, the suite run, the file
written back and verified by sha256. The suite must go red on every mutation.

The first two blocks are gp-ac0u's list (assets/pr-evidence/gp-ac0u/mutate.py): gp-nn90's
mutations and gp-ac0u's. Their patterns are re-cut where gp-0g2s changed the line, each re-cut
marked "re-cut gp-0g2s". The third block breaks one gp-0g2s rule each. gp-ac0u's first equivalent
mutant (the recycled-number clause of the routing match) is live now in routing (heldAt): a late
retry skips the recycled-number strip, so routing alone must refuse a number held beside another
user_id. It is in the third block; the same clause in the stranger test (ownerMatch) stays
equivalent, with its reason.

Usage: python3 -I mutate.py <gateway dir> <pnpm path> [--check]
  --check only counts each pattern (each must occur exactly once) and runs nothing.
"""
import hashlib
import pathlib
import subprocess
import sys

root = pathlib.Path(sys.argv[1])
pnpm = sys.argv[2]
CHECK = "--check" in sys.argv[3:]
# --only=<text> runs just the mutations whose name contains <text> (a quick look; the record is the full run)
ONLY = [a[len("--only="):] for a in sys.argv[3:] if a.startswith("--only=")]
G, M, W, I = "src/gateway.ts", "src/meta.ts", "src/window.ts", "src/index.ts"
V1, V2, V3 = "migrations/0001_init.sql", "migrations/0002_bsuid.sql", "migrations/0003_retries.sql"

GP_NN90 = [
    ("ack ignores the city", G, "WHERE city_id = ?2 AND body IS NOT NULL AND id IN", "WHERE body IS NOT NULL AND id IN"),
    ("ack deletes the row (no dedup row)", G,
     "`UPDATE deliveries SET body = NULL, wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL,\n           profile_name = NULL, acked_at = ?1\n         WHERE city_id = ?2",
     "`DELETE FROM deliveries WHERE ?1 > 0 AND city_id = ?2"),
    ("no margin", W, "lastInboundAt + WINDOW_SECONDS - WINDOW_MARGIN_SECONDS", "lastInboundAt + WINDOW_SECONDS"),
    ("template refused on the window", G, 'if (req.type !== "template" && !windowOpen', "if (!windowOpen"),
    ("a send moves the window", G, 'log({ event: "send", type: req.type });',
     'await db.prepare("UPDATE phones SET last_inbound_at = ? WHERE conversation_id = ?").bind(now, owner.conversation_id).run();\n    log({ event: "send", type: req.type });'),
    ("no code-attempt limit", G, "AND COALESCE((SELECT code_attempts FROM unknown_senders WHERE sender_key = ?9), 0) <= ?10",
     "AND ?10 > 0 AND ?9 IS NOT NULL"),
    # re-cut gp-0g2s: the dedup reads code_attempt_wamids, not last_code_wamid
    ("redelivered code counted twice", G,
     "code_attempts = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN code_attempts",
     "code_attempts = CASE WHEN 0 THEN code_attempts"),
    ("invite expiry by receive time", G, "AND i.expires_at > ?7", "AND i.expires_at > ?5"),
    ("no pending cap", G, "if (held.some((r) => r.n >= MAX_PENDING)) {", "if (held.some((r) => r.n > 10 * MAX_PENDING)) {"),
    ("send to any city's owner", G, "WHERE city_id = ?2 AND (conversation_id = ?1 OR wa_id = ?1 OR user_id = ?1)",
     "WHERE (city_id = ?2 OR 1) AND (conversation_id = ?1 OR wa_id = ?1 OR user_id = ?1)"),
    ("idempotency ignored", G, "if (prior.wamid) return json({ message_id: prior.wamid });", ""),
    ("ambiguous outcome frees the key", G, "if (e.status >= 400 && e.status < 500) {", "if (true) {"),
    ("abandoned claim reads in flight", G, 'if (prior.state === "claimed" && prior.created_at > now - SEND_CLAIM_STALE_SECONDS) {', 'if (prior.state === "claimed") {'),
    ("no orphan statuses", G, "SELECT ?2, ?3, ?1, ?4, ?5, ?7, ?6 WHERE NOT EXISTS", "SELECT ?2, ?3, ?1, ?4, ?5, ?7, ?6 WHERE 0 AND NOT EXISTS"),
    ("orphans never pruned", G, "DELETE FROM orphan_statuses WHERE received_at <= ?\")", "DELETE FROM orphan_statuses WHERE 0 AND received_at <= ?\")"),
    ("naive batch slicing", G, "for (const [from, to] of batchRanges(plan)) {",
     "for (const [from, to] of Array.from({ length: Math.ceil(plan.statements.length / MAX_BATCH_STATEMENTS) }, (_, i): [number, number] => [i * MAX_BATCH_STATEMENTS, Math.min((i + 1) * MAX_BATCH_STATEMENTS, plan.statements.length)])) {"),
    ("one batch only", G, "if (end - from > MAX_BATCH_STATEMENTS && to > from) {", "if (false) {"),
    ("signature not checked", G, "if (!(await verifySignature(raw, request.headers.get(\"x-hub-signature-256\"), cfg.metaAppSecret))) {", "if (false) {"),
    ("phone number id not checked", M, "if (metadata.phone_number_id !== phoneNumberId) {", "if (false) {"),
    ("canned line every time", G, "WHERE unknown_senders.last_replied_at IS NULL OR unknown_senders.last_replied_at <= ?3", "WHERE ?3 > 0"),
    ("welcome on every bind attempt", G, "if (!committed(b.index)) continue;", ""),
    # re-cut gp-0g2s: 0003 rebuilds the dedup key, so 0001's no longer stands
    ("statuses deduped on wamid only", V3, "ON deliveries(wamid, status);", "ON deliveries(wamid);"),
    ("media not scoped to city", G, "FROM media WHERE city_id = ? AND media_id = ? AND received_at > ?\")\n      .bind(cityId, mediaId",
     "FROM media WHERE ? IS NOT NULL AND media_id = ? AND received_at > ?\")\n      .bind(cityId, mediaId"),
    ("hold expires early", G, "export const HOLD_SECONDS = 7 * DAY;", "export const HOLD_SECONDS = 6 * DAY;"),
    ("stale bound is the hold", G, "m.timestamp > now - TOMBSTONE_SECONDS", "m.timestamp > now - HOLD_SECONDS"),
    ("redelivery stretches the window", G, "const at = Math.min(m.timestamp ?? now, now);", "const at = now;"),
    ("reaction does not move the window", W, '  "text",\n  "reaction",\n', '  "text",\n'),
    ("system notice moves the window", W, '  "order",\n]);', '  "order",\n  "system",\n]);'),
    ("revoked credential accepted", G, "WHERE token_sha256 = ? AND revoked_at IS NULL", "WHERE token_sha256 = ?"),
    ("log carries the phone", G, '      event: "webhook",\n', '      event: "webhook",\n      who: batch.messages[0]?.from ?? null,\n'),
    ("entry logs the request URL (the handshake's query)", I, "    return gateway(env).handle(request);",
     "    console.log(request.url);\n    return gateway(env).handle(request);"),
]

GP_AC0U = [
    ("parse drops a message without from", M, "if (!wamid || (!from && !fromUserId && !change?.previousUserId)) continue;", "if (!wamid || !from) continue;"),
    ("user_id not read from contacts", M, "const fromUserId = ownUser ?? contact?.user ?? null;", "const fromUserId = ownUser;"),
    ("wa_id not read from contacts", M, "const from = ownFrom ?? contact?.wa ?? null;", "const from = ownFrom;"),
    ("username not read", M, "username: contact?.username ?? null,", "username: null,"),
    ("recipient_user_id not read", M, "const recipientUserId = str(s.recipient_user_id);", "const recipientUserId = null;"),
    ("number change not read", M, 'if (type !== "user_changed_number" && type !== "user_changed_user_id") return null;', "return null;"),
    ("BSUID sent in to", M, "return /^\\d+$/.test(id) ? { to: id } : { recipient: id };", "return { to: id };"),
    ("BSUID sent with to beside it", M, ": { recipient: id };", ": { recipient: id, to: id };"),
    ("BSUID country code in any case", M, "/^[A-Z]{2}\\.", "/^[A-Za-z]{2}\\."),
    ("BSUID 129 characters", M, "[A-Za-z0-9]{1,128}$/", "[A-Za-z0-9]{1,129}$/"),
    ("BSUID trimmed", G, "if (BSUID.test(v)) return v;", "if (BSUID.test(v.trim())) return v.trim();"),
    ("a recycled number planned as the owner's (the pre-read)", G,
     "r.wa_id === ids.wa && (r.user_id === null || ids.user === null)", "r.wa_id === ids.wa"),
    ("routing ignores the user_id", G, "`(user_id = ${user} OR (wa_id", "`(0 OR (wa_id"),
    ("the wa_id's row before the user_id's", G, "ORDER BY CASE WHEN user_id = ${user} THEN 0 ELSE 1 END LIMIT 1",
     "ORDER BY CASE WHEN user_id = ${user} THEN 1 ELSE 0 END LIMIT 1"),
    ("no user_id fill", G, "user_id = CASE WHEN user_id IS NULL AND ?6 IS NOT NULL", "user_id = CASE WHEN 0 AND ?6 IS NOT NULL"),
    ("a fill takes another row's wa_id", G,
     "wa_id = CASE WHEN wa_id IS NULL AND ?5 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.wa_id = ?5)",
     "wa_id = CASE WHEN wa_id IS NULL AND ?5 IS NOT NULL"),
    ("bind keyed by the user_id first", G, "THEN ?1 ELSE ?2 END AS id) k", "THEN ?2 ELSE ?1 END AS id) k"),
    ("bind keyed by a number that is another owner's key", G,
     "CASE WHEN ?1 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE conversation_id = ?1)", "CASE WHEN ?1 IS NOT NULL"),
    ("stranger keyed by the wa_id first", G, "const strangerKey = user ?? wa;", "const strangerKey = wa ?? user;"),
    ("a delivery's conversation_id from the message's wa_id", G,
     "COALESCE(?3, wa_id), COALESCE(?4, user_id), ?5, conversation_id, ?6, ?7",
     "COALESCE(?3, wa_id), COALESCE(?4, user_id), ?5, COALESCE(?3, conversation_id), ?6, ?7"),
    ("a delivery drops the owner's known phone", G, "COALESCE(?3, wa_id), COALESCE(?4, user_id), ?5", "?3, COALESCE(?4, user_id), ?5"),
    ("a delivery drops the owner's known user_id", G, "COALESCE(?3, wa_id), COALESCE(?4, user_id), ?5", "COALESCE(?3, wa_id), ?4, ?5"),
    # re-cut gp-0g2s: both strips gained the stamp test and moved to multi-line SQL
    ("a recycled number stays with the old owner", G,
     "`UPDATE phones SET wa_id = NULL\n               WHERE wa_id = ?1",
     "`UPDATE phones SET wa_id = NULL\n               WHERE 0 AND wa_id = ?1"),
    ("a number change leaves the new number with its stale holder", G,
     "`UPDATE phones SET wa_id = NULL\n                 WHERE wa_id = ?1",
     "`UPDATE phones SET wa_id = NULL\n                 WHERE 0 AND wa_id = ?1"),
    ("inbox reads no conversation_id from a v1 row", G, "COALESCE(conversation_id, wa_id) AS conversation_id", "conversation_id"),
    ("cron leaves ids on a body-less row", G, "WHERE body IS NULL AND COALESCE(wa_id, user_id, username, conversation_id, profile_name) IS NOT NULL",
     "WHERE 0 AND body IS NULL AND COALESCE(wa_id, user_id, username, conversation_id, profile_name) IS NOT NULL"),
    ("new_wa_id not read", M, "str(m.system.wa_id) ?? str(m.system.new_wa_id)", "str(m.system.wa_id)"),
    ("a status's username not read", M, "const username = contactOf(people, recipientId, recipientUserId)?.username ?? null;", "const username = null;"),
    ("an adopted status loses its username", G, "recipient_id, recipient_user_id, username, ?3, NULL, body", "recipient_id, recipient_user_id, NULL, ?3, NULL, body"),
    ("no re-key on a number change", G, "if (m.change?.userId) {", "if (m.change?.userId && false) {"),
    ("re-key keeps the old phone", G, "THEN ?2 ELSE NULL END", "THEN ?2 ELSE wa_id END"),
    ("a number change answered with the canned line", G, "if (!owned && !m.change) {", "if (!owned) {"),
    # re-cut gp-0g2s: the owner is the one named row
    ("a stale key sent as given", G,
     "const address = to === owner.wa_id || to === owner.user_id ? to : (owner.wa_id ?? owner.user_id);",
     "const address = to;"),
    ("a status's conversation_id from its recipient", G, "COALESCE(conversation_id, ?4, ?5)", "COALESCE(?4, ?5)"),
    ("an adopted status loses its user_id", G, "recipient_id, recipient_user_id, username, ?3, NULL, body", "recipient_id, NULL, username, ?3, NULL, body"),
    ("admin prints a user_id's tail", G, "CASE WHEN wa_id IS NULL THEN 'id' ELSE substr(wa_id, -4) END", "substr(COALESCE(wa_id, user_id), -4)"),
    ("inbox conversation_id is the wa_id", G, "conversation_id: r.conversation_id,\n      profile_name", "conversation_id: r.wa_id,\n      profile_name"),
    ("window conversation_id flips to the user_id", G, '"SELECT conversation_id, user_id, last_inbound_at FROM phones',
     '"SELECT COALESCE(user_id, conversation_id) AS conversation_id, user_id, last_inbound_at FROM phones'),
    ("ack keeps the user_id", G,
     "SET body = NULL, wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL,\n           profile_name = NULL, acked_at",
     "SET body = NULL, wa_id = NULL,\n           profile_name = NULL, acked_at"),
    ("log carries the user_id", G, '      event: "webhook",\n', '      event: "webhook",\n      who: batch.messages[0]?.fromUserId ?? null,\n'),
    ("migration loses the v1 key", V2, "SELECT wa_id, wa_id, NULL, NULL, city_id", "SELECT 'v1:' || wa_id, wa_id, NULL, NULL, city_id"),
    ("migration skips the held backfill", V2, "UPDATE deliveries SET conversation_id = wa_id WHERE wa_id IS NOT NULL;", ""),
    ("user_id not unique", V2, "  user_id TEXT UNIQUE,", "  user_id TEXT,"),
]

GP_0G2S = [
    # PR 76 MAJOR 1: a `to` naming two owners (the mayor's rule R1)
    ("a `to` naming two owners is routed", G, "if (named.length > 1) {", "if (false) {"),
    ("the ambiguity checked before the key (a repeat no longer answers its first answer)", G,
     "    if (named.length === 0) {\n",
     "    if (named.length > 1) throw new HttpError(409, \"ambiguous_recipient\", { hint: \"send to the owner's current id\" });\n    if (named.length === 0) {\n"),
    ("the 409 names the recipient", G,
     "{ hint: \"send to the owner's current id\" });\n    }\n    const owner",
     "{ hint: \"send to the owner's current id\", to });\n    }\n    const owner"),
    ("the ambiguous refusal is not logged", G, 'log({ event: "send_refused", reason: "ambiguous_recipient" });', ""),
    # PR 73 MAJOR: a stored wamid is one city's
    ("the dedup key keeps the city", V3, "CREATE UNIQUE INDEX deliveries_wamid_once ON deliveries(wamid, status);",
     "CREATE UNIQUE INDEX deliveries_wamid_once ON deliveries(city_id, wamid, status);"),
    ("media granted to the city of the latest delivery, not the wamid's", G,
     "SELECT city_id, ?1, received_at FROM deliveries WHERE wamid = ?2 AND status = ''",
     "SELECT city_id, ?1, received_at FROM deliveries WHERE status = '' AND wamid <> ?2 ORDER BY id DESC LIMIT 1"),
    # Who held the id at the stamp (self-review r1): the current spans
    ("a current user_id held from before its binding", G,
     "((user_id = ${user} AND COALESCE(bound_stamp, 0) <= ${stamp})", "((user_id = ${user})"),
    ("a current wa_id held from before it came", G,
     " AND COALESCE(wa_since, bound_stamp, 0) <= ${stamp}))`;", "))`;"),
    ("the binding's stamp not recorded", G, "SELECT k.id, ?1, ?2, ?3, i.city_id, ?4, ?5, ?6, ?7, ?11", "SELECT k.id, ?1, ?2, ?3, i.city_id, ?4, ?5, ?6, ?7, NULL"),
    ("a number filled later held from the binding", G, "THEN ?7 ELSE wa_since END,", "THEN NULL ELSE wa_since END,"),
    ("a number taken in a change held from the binding", G, "THEN ?5 ELSE NULL END,", "THEN NULL ELSE NULL END,"),
    ("routing by a wa_id held beside another user_id (gp-ac0u's equivalent mutant, live now)", G,
     "OR (wa_id = ${wa} AND (user_id IS NULL OR ${user} IS NULL) AND COALESCE(wa_since", "OR (wa_id = ${wa} AND COALESCE(wa_since"),
    ("the recycled-number rule strips a newer holder (a late retry)", G,
     " AND COALESCE(wa_since, bound_stamp, 0) <= ?3\n                 AND NOT EXISTS (SELECT 1 FROM phone_aliases WHERE id = ?2)`,",
     " AND ?3 IS NOT NULL\n                 AND NOT EXISTS (SELECT 1 FROM phone_aliases WHERE id = ?2)`,"),
    ("the recycled-number rule fires on a previous user_id's retry", G,
     "\n                 AND NOT EXISTS (SELECT 1 FROM phone_aliases WHERE id = ?2)`,", "`,"),
    ("a late notice takes its new number from a newer holder", G,
     " AND COALESCE(wa_since, bound_stamp, 0) <= ?3`,\n              )\n              .bind(m.change.waId, m.change.userId, stamp),",
     " AND ?3 IS NOT NULL`,\n              )\n              .bind(m.change.waId, m.change.userId, stamp),"),
    ("a notice strips its own old number from the next holder", G,
     "if (wa !== null && user !== null && !m.change) {", "if (wa !== null && user !== null) {"),
    # The previous holdings
    ("no previous holdings recorded", G, "               WHERE c.id IS NOT NULL AND c.id IS NOT ?1", "               WHERE 0 AND c.id IS NOT NULL AND c.id IS NOT ?1"),
    ("the old user_id not recorded", G, "UNION ALL SELECT user_id, b FROM o", "UNION ALL SELECT NULL, b FROM o"),
    ("the old wa_id not recorded", G, "SELECT wa_id AS id, w AS since FROM o", "SELECT NULL AS id, w AS since FROM o"),
    ("the notice's old number not recorded", G, "UNION ALL SELECT ?3, b FROM o WHERE ?3 IS NOT o.wa_id",
     "UNION ALL SELECT NULL, b FROM o WHERE ?3 IS NOT o.wa_id"),
    ("an old wa_id held from the binding, not from when the row took it", G, "SELECT wa_id AS id, w AS since FROM o",
     "SELECT wa_id AS id, b AS since FROM o"),
    ("a previous holding recorded when the re-key is refused", G,
     "\n                   AND NOT EXISTS (SELECT 1 FROM phones x WHERE x.user_id = ?1 AND x.conversation_id <> phones.conversation_id))",
     ")"),
    ("no routing by previous holdings", G,
     "`(CASE WHEN EXISTS ${pastHolding(wa, user, stamp)}\n         THEN (CASE WHEN ${ownerKey(wa, user, stamp)} IS NULL THEN ${aliasKey(wa, user, stamp)} END)\n         ELSE ${ownerKey(wa, user, stamp)} END)`",
     "`${ownerKey(wa, user, stamp)}`"),
    ("a previous holding falls through to the current holder", G,
     "`(CASE WHEN EXISTS ${pastHolding(wa, user, stamp)}\n         THEN (CASE WHEN ${ownerKey(wa, user, stamp)} IS NULL THEN ${aliasKey(wa, user, stamp)} END)\n         ELSE ${ownerKey(wa, user, stamp)} END)`",
     "`COALESCE(${aliasKey(wa, user, stamp)}, ${ownerKey(wa, user, stamp)})`"),
    ("current holdings before previous ones", G,
     "`(CASE WHEN EXISTS ${pastHolding(wa, user, stamp)}\n         THEN (CASE WHEN ${ownerKey(wa, user, stamp)} IS NULL THEN ${aliasKey(wa, user, stamp)} END)\n         ELSE ${ownerKey(wa, user, stamp)} END)`",
     "`COALESCE(${ownerKey(wa, user, stamp)}, ${aliasKey(wa, user, stamp)})`"),
    ("a previous holding wins when the records disagree", G,
     "THEN (CASE WHEN ${ownerKey(wa, user, stamp)} IS NULL THEN ${aliasKey(wa, user, stamp)} END)",
     "THEN ${aliasKey(wa, user, stamp)}"),
    ("a previous holding spans a message stamped after the change", G, " AND changed_at >= ${stamp})`;", ")`;"),
    ("a previous holding spans a message stamped before its since", G, " AND since <= ${stamp} AND changed_at", " AND changed_at"),
    ("two bindings' previous spans holding one stamp: one wins", G,
     "\n    WHERE (SELECT COUNT(DISTINCT bound_wamid) FROM ${pastHolding(wa, user, stamp)}) = 1)`;", ")`;"),
    ("a previous holding ignores the binding", G, " AND p.bound_wamid = a.bound_wamid", ""),
    ("a wa_id previous holding matches beside a user_id", G, "WHERE id = COALESCE(${user}, ${wa})", "WHERE id IN (${user}, ${wa})"),
    # What a message no one held, or a previous holding, does
    ("unrouted messages not counted", G, "SELECT 'unrouted_messages', 1\n             WHERE ", "SELECT 'unrouted_messages', 1\n             WHERE 0 AND "),
    ("a known sender whose message no span holds is a stranger", G,
     " AND NOT EXISTS (SELECT 1 FROM phones WHERE ${ownerMatch(wa, user)}))`;", ")`;"),
    ("a previous holding's sender is a stranger", G,
     "`(NOT EXISTS ${pastHolding(wa, user, stamp)} AND NOT EXISTS (SELECT 1 FROM phones", "`(NOT EXISTS (SELECT 1 FROM phones"),
    ("a routed retry moves no window", G, '               WHERE conversation_id = ${routeKey("?2", "?3", "?4")} AND EXISTS',
     '               WHERE 0 AND conversation_id = ${routeKey("?2", "?3", "?4")} AND EXISTS'),
    ("a retry the records disagree on rewrites the current holder", G, ' AND NOT EXISTS ${pastHolding("?5", "?6", "?7")}`,', "`,"),
    # (each replacement keeps every bound parameter referenced: D1 refuses an unused one)
    ("a routed retry counts a code attempt", G, 'SELECT ?1, 1, ?2 WHERE ${stranger("?5", "?6", "?7")}',
     'SELECT ?1, 1, ?2 WHERE ?7 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE ${ownerMatch("?5", "?6")})'),
    ("a routed retry binds", G, '                 AND NOT EXISTS ${pastHolding("?1", "?2", "?11")}\n', ""),
    ("a routed retry hears the canned line", G, 'SELECT ?1, ?2 WHERE ${stranger("?4", "?5", "?6")}',
     'SELECT ?1, ?2 WHERE ?6 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE ${ownerMatch("?4", "?5")})'),
    ("aliases never pruned", G, 'db.prepare("DELETE FROM phone_aliases WHERE expires_at <= ?").bind(now),',
     'db.prepare("DELETE FROM phone_aliases WHERE 0 AND expires_at <= ?").bind(now),'),
    ("the alias horizon is the hold (7 days)", G, "export const ALIAS_SECONDS = TOMBSTONE_SECONDS;", "export const ALIAS_SECONDS = HOLD_SECONDS;"),
    # PR 73 MINOR: every counted wamid
    ("code wamids never recorded", G, "INSERT OR IGNORE INTO code_attempt_wamids (wamid, counted_at) VALUES (?1, ?2)",
     "INSERT OR IGNORE INTO code_attempt_wamids (wamid, counted_at) SELECT ?1, ?2 WHERE 0"),
    ("code wamids never pruned", G, 'db.prepare("DELETE FROM code_attempt_wamids WHERE counted_at <= ?")',
     'db.prepare("DELETE FROM code_attempt_wamids WHERE 0 AND counted_at <= ?")'),
    ("code wamids kept a day", G, "const CODE_WAMIDS_KEEP_SECONDS = TOMBSTONE_SECONDS;", "const CODE_WAMIDS_KEEP_SECONDS = DAY;"),
]

# Equivalent mutants: each removes a rule that another statement of the same D1 batch already
# enforces, so no input can tell them apart. Each is run, and is expected to survive; it is not
# counted.
EQUIVALENT = [
    ("the hold expiry keeps the user_id", G,
     "`UPDATE deliveries SET body = NULL, wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL, profile_name = NULL\n",
     "`UPDATE deliveries SET body = NULL, wa_id = NULL, profile_name = NULL\n",
     "the cron's later statement in the same batch clears every id on a body-less row"),
    ("the routed-retry window statement runs for a current route too", G,
     ' AND EXISTS ${pastHolding("?2", "?3", "?4")}`,', "`,",
     "for a current route the plain UPDATE of the same group moves the same row's window to the same MAX"),
    ("the stranger test takes a number held beside another user_id (gp-ac0u's equivalent mutant, in ownerMatch)", G,
     "OR (wa_id = ${wa} AND (user_id IS NULL OR ${user} IS NULL)))`;", "OR (wa_id = ${wa}))`;",
     "the recycled-number strip at the head of the group has already cleared that wa_id, unless its holder took the "
     "number after the message's stamp; that message is then a stranger's stale text, undelivered either way (the "
     "original answers it with the canned line, the mutant does not)"),
]

BLOCKS = (("gp-nn90 (re-cut)", GP_NN90), ("gp-ac0u (re-cut)", GP_AC0U), ("gp-0g2s", GP_0G2S))

if CHECK:
    bad = []
    for _, mutations in (*BLOCKS, ("equivalent", [m[:4] for m in EQUIVALENT])):
        for name, rel, old, _new in mutations:
            n = (root / rel).read_text().count(old)
            if n != 1:
                bad.append(f"{name}: pattern count {n}")
    print("\n".join(bad) if bad else f"every pattern occurs once ({sum(len(m) for _, m in BLOCKS)} mutations, {len(EQUIVALENT)} equivalent)")
    sys.exit(1 if bad else 0)

if ONLY:
    BLOCKS = tuple((b, [m for m in ms if any(o in m[0] for o in ONLY)]) for b, ms in BLOCKS)
    EQUIVALENT = [m for m in EQUIVALENT if any(o in m[0] for o in ONLY)]

survivors = []
total = 0
for block, mutations in BLOCKS:
    print(f"## {block}")
    for name, rel, old, new in mutations:
        total += 1
        path = root / rel
        original = path.read_bytes()
        text = original.decode()
        if text.count(old) != 1:
            print(f"SKIP (pattern count {text.count(old)}): {name}")
            survivors.append(f"{name} (pattern)")
            continue
        path.write_bytes(text.replace(old, new).encode())
        try:
            run = subprocess.run([pnpm, "test"], cwd=root, capture_output=True, text=True, timeout=600)
        finally:
            path.write_bytes(original)
        if hashlib.sha256(path.read_bytes()).hexdigest() != hashlib.sha256(original).hexdigest():
            raise SystemExit(f"RESTORE FAILED: {rel}")
        tail = [line for line in run.stdout.splitlines() if "Tests " in line]
        verdict = "KILLED" if run.returncode != 0 else "SURVIVED"
        print(f"{verdict}: {name} :: {tail[-1].strip() if tail else run.stdout[-200:].strip()}")
        if run.returncode == 0:
            survivors.append(name)

print("## equivalent (expected to survive; not counted)")
for name, rel, old, new, why in EQUIVALENT:
    path = root / rel
    original = path.read_bytes()
    text = original.decode()
    if text.count(old) != 1:
        print(f"SKIP (pattern count {text.count(old)}): {name}")
        survivors.append(f"{name} (pattern)")
        continue
    path.write_bytes(text.replace(old, new).encode())
    try:
        run = subprocess.run([pnpm, "test"], cwd=root, capture_output=True, text=True, timeout=600)
    finally:
        path.write_bytes(original)
    if hashlib.sha256(path.read_bytes()).hexdigest() != hashlib.sha256(original).hexdigest():
        raise SystemExit(f"RESTORE FAILED: {rel}")
    tail = [line for line in run.stdout.splitlines() if "Tests " in line]
    verdict = "SURVIVED (equivalent)" if run.returncode == 0 else "KILLED (not equivalent after all)"
    print(f"{verdict}: {name} :: {tail[-1].strip() if tail else ''} :: {why}")

print(f"\n{total - len(survivors)}/{total} mutations killed")
for s in survivors:
    print(f"  not killed: {s}")
sys.exit(1 if survivors else 0)
