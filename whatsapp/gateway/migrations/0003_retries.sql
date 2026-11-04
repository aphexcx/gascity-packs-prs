-- whatsapp-gateway, schema v3 (gp-0g2s): Meta's retries. Meta redelivers a webhook it did not see
-- answered 200 for up to 7 days, in any order, and the gateway drops a message Meta stamped more
-- than 8 days ago (TOMBSTONE_SECONDS). Three rules hold across that horizon.

-- 1. A wamid is one row whichever city holds it. The dedup key loses city_id, so a retry that
--    arrives after the phone moved to another city (the operator's unbind, then a new invite) is
--    the first city's row, held or acknowledged, and never a second city's delivery. The media
--    proxy's grant follows the wamid's row (src/gateway.ts), so the new city is granted nothing.
--    A status stays one row per (wamid, status): an outbound wamid is one send's, of one city.
--    The index is built over the rows already there. It can fail only where two cities already
--    hold one wamid, which only the misroute this fixes could have written.
DROP INDEX deliveries_once;
CREATE UNIQUE INDEX deliveries_once ON deliveries(wamid, status);

-- 2. The ids an owner was named by before a number change, kept as aliases of that owner's
--    binding. A message Meta stamped at or before the change and retried after it routes to the
--    owner under its old ids, never to a stranger or to the number's next holder. An alias routes
--    only what Meta stamped at or before changed_at, and a message stamped longer ago than 8 days
--    is dropped, so nothing can use an alias past changed_at + 8 days. The cron deletes it then
--    (expires_at). One id can be an alias of two bindings (a number one owner left and another
--    took and left too); each change is its own row, and a message goes to the first change at or
--    after its stamp: the binding that held the id then.
CREATE TABLE phone_aliases (
  id TEXT NOT NULL,              -- the previous wa_id or user_id
  conversation_id TEXT NOT NULL, -- the owner it named
  bound_wamid TEXT NOT NULL,     -- the owner's binding: a later binding under the same key is another owner
  changed_at INTEGER NOT NULL,   -- Meta's timestamp of the change, clamped to the gateway's clock
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
