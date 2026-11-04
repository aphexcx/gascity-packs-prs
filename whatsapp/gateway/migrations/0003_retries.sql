-- whatsapp-gateway, schema v3 (gp-0g2s): Meta's retries. Meta redelivers a webhook it did not see
-- answered 200 for up to 7 days, in any order, and the gateway drops a message Meta stamped more
-- than 8 days ago (TOMBSTONE_SECONDS). Three rules hold across that horizon.

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

-- 2. Who held an id when Meta stamped a message. A message routes to the binding that held its
--    sender's id at the stamp, or to nobody: never to a binding that took the id later (a later
--    holder of a recycled number, or the same person bound again in another city after an
--    unbind). A current id is held from bound_stamp, or, for a wa_id the row took later, from
--    wa_since. bound_stamp is NULL on a row bound before this migration: held from the start, as
--    before.
ALTER TABLE phones ADD COLUMN bound_stamp INTEGER; -- Meta's stamp of the binding's invite text
ALTER TABLE phones ADD COLUMN wa_since INTEGER;    -- Meta's stamp of the later message that gave the row its wa_id; NULL: the invite text's

--    A previous id, one a number change replaced, is held from since until changed_at. A
--    message Meta stamped in that span and retried after the change routes to the owner under
--    its old ids, while its binding stands, or to nobody. No message can use it past
--    changed_at + 8 days: an alias routes only what Meta stamped at or before changed_at, and a
--    message stamped longer ago than 8 days is dropped. The cron deletes it then (expires_at).
--    One id can be held by two bindings in turn; each change is its own row.
CREATE TABLE phone_aliases (
  id TEXT NOT NULL,              -- the previous wa_id or user_id
  conversation_id TEXT NOT NULL, -- the owner it named
  bound_wamid TEXT NOT NULL,     -- the owner's binding: a later binding under the same key is another owner
  since INTEGER NOT NULL,        -- Meta's stamp from which the binding held it
  changed_at INTEGER NOT NULL,   -- Meta's stamp of the change that replaced it
  expires_at INTEGER NOT NULL,   -- changed_at + 8 days
  PRIMARY KEY (id, bound_wamid, changed_at)
);

-- 3. Every wrong-code text a stranger's attempt count has seen, by wamid, so a Meta redelivery of
--    any of them is never a second attempt. Before this, only the last one was remembered
--    (unknown_senders.last_code_wamid), so interleaved redeliveries (A, B, A, B, A) counted five.
--    Kept 8 days, as long as Meta may redeliver it. last_code_wamid is no longer read or written.
CREATE TABLE code_attempt_wamids (
  wamid TEXT NOT NULL PRIMARY KEY,
  counted_at INTEGER NOT NULL
);
