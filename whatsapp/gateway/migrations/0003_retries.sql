-- whatsapp-gateway, schema v3 (gp-0g2s; re-cut in place by gp-lv49 to the design of record,
-- whatsapp/docs/design-bsuid-pairing-and-spans.md, section 5): Meta's retries, and routing by the
-- user id. Meta redelivers a webhook it did not see answered 200 for up to 7 days, in any order,
-- and the gateway drops a message Meta stamped more than 8 days ago (TOMBSTONE_SECONDS).

-- 1. A wamid is one row whichever city holds it. The dedup key loses city_id, so a retry that
--    arrives after the phone moved to another city (the operator's unbind, then a new invite) is
--    the first city's row, held or acknowledged, and never a second city's delivery. The media
--    proxy's grant follows the wamid's row (src/gateway.ts), so the new city is granted nothing.
--    A status stays one row per (wamid, status): an outbound wamid is one send's, of one city.
--    The new index is built over the rows already there, before the old one is dropped, so a
--    failed build leaves the old dedup in place. It can fail only where two cities already hold
--    one wamid, which only the misroute this fixes could have written.
CREATE UNIQUE INDEX deliveries_wamid_once ON deliveries(wamid, status);
DROP INDEX deliveries_once;

-- 2. Who a message is from: the binding answering to its sender's user id when Meta stamped it,
--    never a number (design note, section 2). A binding answers to the user id it was bound by
--    and to each id a number change's notice links to it, from its invite text (bound_stamp) or
--    that id's last unbind, whichever is later, until the notice that replaces the id. NULL
--    bound_stamp: a row bound before this migration, by PR 73's code, by its number alone; it
--    holds no user id, so no message routes to it (the mayor unbinds and re-invites each such
--    owner at the deploy, the design note's question 1 (a)).
ALTER TABLE phones ADD COLUMN bound_stamp INTEGER; -- Meta's stamp of the binding's invite text

--    A number change's notice, as a link from the user id it replaces to the one Meta gave in its
--    place. Links are recorded in any order of arrival: one whose old id a binding answers to
--    re-keys that binding (phones.user_id) to the end of its chain; one no binding reaches yet
--    waits. A replay changes nothing (one link per old id), and a link to an id another binding
--    answers to is never recorded. Kept 8 days from the notice's stamp: no message the old id
--    may route (one Meta stamped at or before the notice) can pass the stale filter later.
CREATE TABLE links (
  old_id TEXT NOT NULL PRIMARY KEY, -- the user id the change replaced (system.previous_user_id)
  new_id TEXT NOT NULL,             -- the user id Meta gave in its place (system.user_id)
  stamp INTEGER NOT NULL            -- Meta's stamp of the notice
);
CREATE INDEX links_new ON links(new_id);

--    The operator's unbind (POST /v1/admin/unbind) records every user id the binding answered to
--    in the last 8 days: its current one and each its links reached. A message stamped at or
--    before an id's last unbind is the unbound binding's: it reaches no city, binds nothing and
--    is counted, and no later binding answers to the id at that stamp. Kept 8 days.
CREATE TABLE unbinds (
  user_id TEXT NOT NULL,
  unbound_at INTEGER NOT NULL,      -- the gateway's clock at the unbind
  PRIMARY KEY (user_id, unbound_at)
);

--    Each phone number's newest pairing with a user id: the high-water mark. It decides only
--    which binding shows the number (the admin view, a delivery that omits it, a send's `to`
--    naming it), never a route. Any message naming the number beside a user id moves it unless
--    Meta stamped the message below it; a notice moves its new number to the new id and clears
--    its old number (user_id NULL). A mark naming no binding is deleted 8 days after its stamp.
CREATE TABLE marks (
  number TEXT NOT NULL PRIMARY KEY,
  user_id TEXT,                     -- NULL: a notice cleared it
  stamp INTEGER NOT NULL            -- Meta's stamp of the pairing
);
CREATE INDEX marks_user ON marks(user_id);

-- 3. Every wrong-code text a stranger's attempt count has seen, by wamid, so a Meta redelivery of
--    any of them is never a second attempt. Before this, only the last one was remembered
--    (unknown_senders.last_code_wamid), so interleaved redeliveries (A, B, A, B, A) counted five.
--    Kept 8 days, as long as Meta may redeliver it. last_code_wamid is no longer read or written.
CREATE TABLE code_attempt_wamids (
  wamid TEXT NOT NULL PRIMARY KEY,
  counted_at INTEGER NOT NULL
);

--    The one wamid each stranger's count already holds, the last code text the running Worker
--    counted, is carried in, so Meta's redelivery of it after the deploy is still no new attempt
--    (gp-w376). Its count happened in the day that began at attempts_since and so before a day
--    later: kept from then, it lives at least as long as a row the gateway writes when it counts.
--    No attempt is counted between this migration and the deploy: the running Worker's stranger
--    path names unknown_senders.wa_id, which 0002 renames, so it answers 500 and Meta redelivers
--    to the new code.
INSERT OR IGNORE INTO code_attempt_wamids (wamid, counted_at)
  SELECT last_code_wamid, attempts_since + 86400 FROM unknown_senders WHERE last_code_wamid IS NOT NULL;
