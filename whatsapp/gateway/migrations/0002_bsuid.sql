-- whatsapp-gateway, schema v2 (gp-ac0u): Meta's business-scoped user ids (BSUID).
--
-- Meta names a WhatsApp user by a wa_id (the phone number), a user_id (the BSUID: a country code,
-- a period, up to 128 alphanumerics), or both. A user with a username is named by the user_id
-- alone, unless the business has a reason to see the phone. Meta regenerates the user_id when
-- the user changes phone number, and says so with a system message.
-- (developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids,
-- read 2026-11-04.)
--
-- An owner row is keyed by conversation_id. It is set once, at the bind: the binding message's
-- wa_id, unless that phone number is already another owner's conversation_id (a recycled number),
-- else its user_id. It never changes, because it is the id the city's box files the conversation
-- under. wa_id and user_id are the ids Meta names the owner by now. Each is unique
-- where present and NULL where Meta shares none. A number change replaces both and leaves the
-- conversation_id alone.

-- phones: rebuilt, because its key moves from wa_id (which a username owner lacks) to
-- conversation_id. No table references phones. Every v1 row keeps its wa_id, and that wa_id
-- becomes its conversation_id, so every box keeps the id it already holds.
CREATE TABLE phones_v2 (
  conversation_id TEXT NOT NULL PRIMARY KEY,
  wa_id TEXT UNIQUE,
  user_id TEXT UNIQUE,
  username TEXT,               -- the owner's WhatsApp username as Meta last showed it
  city_id TEXT NOT NULL REFERENCES cities(id),
  profile_name TEXT,
  bound_at INTEGER NOT NULL,
  bound_wamid TEXT NOT NULL,
  last_inbound_at INTEGER
);
INSERT INTO phones_v2 (conversation_id, wa_id, user_id, username, city_id, profile_name, bound_at, bound_wamid, last_inbound_at)
  SELECT wa_id, wa_id, NULL, NULL, city_id, profile_name, bound_at, bound_wamid, last_inbound_at FROM phones;
DROP TABLE phones;
ALTER TABLE phones_v2 RENAME TO phones;
CREATE INDEX phones_city ON phones(city_id);

-- deliveries: the ids the message or status named, and the owner's conversation_id (the one
-- field a box needs to file a delivery that carries no wa_id). All three are cleared with the
-- body, like wa_id and profile_name. A held v1 row's wa_id is its owner's key.
ALTER TABLE deliveries ADD COLUMN user_id TEXT;
ALTER TABLE deliveries ADD COLUMN username TEXT;
ALTER TABLE deliveries ADD COLUMN conversation_id TEXT;
UPDATE deliveries SET conversation_id = wa_id WHERE wa_id IS NOT NULL;

-- sends: the owner a send went to, so that its statuses reach the city under the same
-- conversation_id whichever id the send named. NULL on a v1 send, whose statuses fall back to
-- their recipient_id (a v1 send always went to the wa_id that is its key).
ALTER TABLE sends ADD COLUMN conversation_id TEXT;

-- orphan_statuses: the status's recipient_user_id and username, carried into the delivery at
-- adoption.
ALTER TABLE orphan_statuses ADD COLUMN recipient_user_id TEXT;
ALTER TABLE orphan_statuses ADD COLUMN username TEXT;

-- unknown_senders: a stranger is keyed by its user_id when Meta sends one, else its wa_id, so its
-- canned line and its code attempts do not split when Meta starts sharing its phone.
ALTER TABLE unknown_senders RENAME COLUMN wa_id TO sender_key;
