"""Mutation check for the gateway suite at gp-ac0u: one mutation at a time, the suite run, the file
restored and verified by sha256. The suite must go red on every mutation.

The first block is gp-nn90's mutation list (assets/pr-evidence/gp-nn90/mutation-check.txt). Its
patterns are re-cut where gp-ac0u changed the line. The second block breaks one gp-ac0u rule each.

Usage: python3 -I mutate.py <gateway dir> <pnpm path>
"""
import hashlib
import pathlib
import subprocess
import sys

root = pathlib.Path(sys.argv[1])
pnpm = sys.argv[2]
G, M, W, I = "src/gateway.ts", "src/meta.ts", "src/window.ts", "src/index.ts"
V1, V2 = "migrations/0001_init.sql", "migrations/0002_bsuid.sql"

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
    ("redelivered code counted twice", G, "CASE WHEN last_code_wamid IS ?4 THEN code_attempts", "CASE WHEN 0 THEN code_attempts"),
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
    ("statuses deduped on wamid only", V1, "ON deliveries(city_id, wamid, status)", "ON deliveries(city_id, wamid)"),
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
    ("a recycled number stays with the old owner", G,
     '.prepare("UPDATE phones SET wa_id = NULL WHERE wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2")\n            .bind(wa, user),',
     '.prepare("UPDATE phones SET wa_id = NULL WHERE 0 AND wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2")\n            .bind(wa, user),'),
    ("a number change leaves the new number with its stale holder", G,
     '.prepare("UPDATE phones SET wa_id = NULL WHERE wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2")\n              .bind(m.change.waId, m.change.userId),',
     '.prepare("UPDATE phones SET wa_id = NULL WHERE 0 AND wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2")\n              .bind(m.change.waId, m.change.userId),'),
    ("inbox reads no conversation_id from a v1 row", G, "COALESCE(conversation_id, wa_id) AS conversation_id", "conversation_id"),
    ("cron leaves ids on a body-less row", G, "WHERE body IS NULL AND COALESCE(wa_id, user_id, username, conversation_id, profile_name) IS NOT NULL",
     "WHERE 0 AND body IS NULL AND COALESCE(wa_id, user_id, username, conversation_id, profile_name) IS NOT NULL"),
    ("new_wa_id not read", M, "str(m.system.wa_id) ?? str(m.system.new_wa_id)", "str(m.system.wa_id)"),
    ("a status's username not read", M, "const username = contactOf(people, recipientId, recipientUserId)?.username ?? null;", "const username = null;"),
    ("an adopted status loses its username", G, "recipient_id, recipient_user_id, username, ?3, NULL, body", "recipient_id, recipient_user_id, NULL, ?3, NULL, body"),
    ("no re-key on a number change", G, "if (m.change?.userId) {", "if (m.change?.userId && false) {"),
    ("re-key keeps the old phone", G, "THEN ?2 ELSE NULL END", "THEN ?2 ELSE wa_id END"),
    ("a number change answered with the canned line", G, "if (!owned && !m.change) {", "if (!owned) {"),
    ("a stale key sent as given", G,
     "const address = owner && (to === owner.wa_id || to === owner.user_id ? to : (owner.wa_id ?? owner.user_id));",
     "const address = owner && to;"),
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

# Equivalent mutants: each removes a rule that another statement of the same D1 batch already
# enforces, so no input can tell them apart. Each is run, and is expected to survive; it is not
# counted.
EQUIVALENT = [
    ("routing by a wa_id held beside another user_id (SQL)", G, " AND (user_id IS NULL OR ${user} IS NULL)", "",
     "the stale-number UPDATE at the head of every message group with both ids has already cleared that wa_id"),
    ("the hold expiry keeps the user_id", G,
     "`UPDATE deliveries SET body = NULL, wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL, profile_name = NULL\n",
     "`UPDATE deliveries SET body = NULL, wa_id = NULL, profile_name = NULL\n",
     "the cron's later statement in the same batch clears every id on a body-less row"),
]

survivors = []
total = 0
for block, mutations in (("gp-nn90 (re-cut)", GP_NN90), ("gp-ac0u", GP_AC0U)):
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
