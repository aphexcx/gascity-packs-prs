#!/usr/bin/env python3
"""gp-lv49 mutation probe: one targeted change at a time to the model's decision points in
src/gateway.ts, each run against the whole suite in a detached probe worktree. A mutant the suite
does not kill is a decision the tests do not pin. Every mutation keeps every bound ?N referenced
(D1 refuses a parameter the SQL never names, which would "kill" a mutant for the wrong reason).
The source is restored and verified by sha256 after every run."""
import hashlib, json, subprocess, sys, time

TREE = "/private/tmp/gp-lv49-probe/mut/whatsapp/gateway"
SRC = f"{TREE}/src/gateway.ts"
PNPM = "/Users/tailor512/city/.gc/shims/toolchain/pnpm"

MUTANTS = [
    ("M1 route: a previous id routes past the notice that replaced it", "(walk.until IS NULL OR ${stamp} <= walk.until) AND ", "", 1),
    ("M2 route: a binding takes what Meta stamped before its invite text", "${stamp} >= COALESCE(p.bound_stamp, 0)", "${stamp} >= 0", 1),
    ("M3 route: no unbind floor", "AND ${stamp} > COALESCE(${unboundAt(id)}, -1)\n     LIMIT 1)", "AND ${stamp} > -1\n     LIMIT 1)", 1),
    ("M4 walk: does not stop at the first current id", "WHERE walk.depth < ${LINK_DEPTH} AND NOT EXISTS (SELECT 1 FROM phones h WHERE h.user_id = walk.id))", "WHERE walk.depth < ${LINK_DEPTH})", 1),
    ("M5 stranger: no unbind floor (a text from before an unbind is a stranger's)", "${holder(id)} IS NULL AND ${stamp} > COALESCE(${unboundAt(id)}, -1))", "${holder(id)} IS NULL AND ${stamp} > -1)", 1),
    ("M6 link: recorded even when its new id is another binding's", "WHERE ?1 <> ?2 AND ${holder(\"?2\")} IS NULL", "WHERE ?1 <> ?2", 1),
    ("M7 re-key: walks into an id another binding holds", "WHERE fwd.depth < ${LINK_DEPTH} AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = l.new_id))", "WHERE fwd.depth < ${LINK_DEPTH})", 1),
    ("M8 no re-key after a bind", "push(db.prepare(REKEY).bind(user));", "", 1),
    ("M9 no re-key on a notice", "push(db.prepare(REKEY).bind(previousUserId));", "", 1),
    ("M10 a pairing moves the mark even when stamped below it", "WHERE excluded.stamp >= marks.stamp`,\n            )\n            .bind(wa, m.fromUserId, stamp)", "WHERE excluded.stamp >= 0`,\n            )\n            .bind(wa, m.fromUserId, stamp)", 1),
    ("M11 a notice does not clear its old number", "if (wa !== null && wa !== waId) {", "if (false && wa !== waId) {", 1),
    ("M12 a send goes to `to` as given", "const address = owner.user_id;", "const address = to;", 1),
    ("M13 the welcome goes to the id the code text named", "tell(bound?.user_id ?? b.id,", "tell(b.id,", 1),
    ("M14 an unbind records only the current id", "JOIN links l ON l.new_id = answered.id", "JOIN links l ON 0", 1),
    ("M15 a notice no binding answers to is not counted", "IS NULL AND (?3 OR NOT ${stranger(\"?1\", \"?2\")})", "IS NULL AND (?3 = -1 OR NOT ${stranger(\"?1\", \"?2\")})", 1),
    ("M16 the cron deletes marks naming a current binding", "AND (user_id IS NULL OR NOT EXISTS (SELECT 1 FROM phones p WHERE p.user_id = marks.user_id))", "AND 1 = 1", 1),
    ("M17 a code binds a previous id of a standing binding (holder dropped)", "AND ${stranger(\"?2\", \"?10\")}", "AND ?2 IS NOT NULL AND ?10 > COALESCE((SELECT MAX(unbound_at) FROM unbinds WHERE user_id = ?2), -1)", 1),
    ("M18 the window moves only for a message under the current id", "THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,\n               profile_name", "THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,\n               profile_name", 1),
    ("M19 a retry under a previous id overwrites the names", "profile_name = CASE WHEN user_id = ?5 THEN COALESCE(?1, profile_name) ELSE profile_name END", "profile_name = COALESCE(?1, profile_name)", 1),
    ("M20 a delivery omitting the number carries none", "COALESCE(?3, ${shownNumber(\"p\")})", "COALESCE(?3, NULL)", 1),
    ("M21 a send's `to` never names a number an owner shows", "OR EXISTS (SELECT 1 FROM marks k WHERE k.number = ?1 AND k.user_id = p.user_id)) LIMIT 2", ") LIMIT 2", 1),
    ("M22 the route ignores the walk's end (any id on the chain)", "SELECT p.conversation_id FROM walk JOIN phones p ON p.user_id = walk.id\n     WHERE", "SELECT p.conversation_id FROM walk JOIN phones p ON p.user_id = walk.id OR p.conversation_id = walk.id\n     WHERE", 1),
]
# M18 is a placeholder kept equal on purpose? No: replace it with the real window mutation below.
MUTANTS[17] = (
    "M18 the window moves only for a message under the current id",
    "last_inbound_at = CASE WHEN ?3 THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,",
    "last_inbound_at = CASE WHEN ?3 AND user_id = ?5 THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,",
    1,
)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def run_suite():
    r = subprocess.run([PNPM, "exec", "vitest", "run", "--reporter=json", "--outputFile=/private/tmp/gp-lv49/mut-run.json"],
                       cwd=TREE, capture_output=True, text=True)
    try:
        j = json.load(open("/private/tmp/gp-lv49/mut-run.json"))
        failed = [a["title"][:110] for f in j["testResults"] for a in f["assertionResults"] if a["status"] == "failed"]
        file_errors = [f["message"][:200] for f in j["testResults"] if f.get("message")]
        return j["numPassedTests"], j["numFailedTests"], failed, file_errors
    except Exception as e:  # noqa: BLE001
        return 0, -1, [f"no json: {e}"], [r.stderr[-400:]]


original = open(SRC, "rb").read()
orig_sha = sha(original)
only = sys.argv[1] if len(sys.argv) > 1 else None
out = []
p0, f0, _, _ = run_suite()
out.append(f"# baseline at the probe head: {p0} passed, {f0} failed; src/gateway.ts sha256 {orig_sha[:12]}")
for name, old, new, n in MUTANTS:
    if only and not name.startswith(only):
        continue
    text = original.decode()
    c = text.count(old)
    if c != n:
        out.append(f"{name}: SKIPPED (pattern found {c} times, expected {n})")
        continue
    open(SRC, "wb").write(text.replace(old, new).encode())
    t0 = time.time()
    p, f, failed, errs = run_suite()
    open(SRC, "wb").write(original)
    assert sha(open(SRC, "rb").read()) == orig_sha, "restore failed"
    verdict = "KILLED" if f != 0 else "SURVIVED"
    out.append(f"{name}: {verdict} ({p} passed, {f} failed, {time.time() - t0:.1f}s)" + (f"; first failures: {failed[:3]}" if failed else "") + (f"; file errors: {errs[:1]}" if errs else ""))
    print(out[-1], flush=True)
open("/private/tmp/gp-lv49/mutation-check.txt", "w").write("\n".join(out) + "\n")
print("\n".join(out))
