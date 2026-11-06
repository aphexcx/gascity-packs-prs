-- whatsapp-gateway, schema v1 (gp-nn90). Every time is Unix epoch seconds.
-- No message body is ever logged. A delivery's body is held here until the city's ack names its
-- id, or until the daily cron at 7 days; the body-less row stays one more day so a late Meta
-- redelivery of the same wamid is still one row.

-- A city: one box, enrolled once on an operator's enrollment code.
CREATE TABLE cities (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,          -- the operator's name for the city; the welcome line names it
  box_name TEXT NOT NULL,      -- the name the box gave at enrollment
  created_at INTEGER NOT NULL
);

-- A city's bearer, stored as its sha256 only.
CREATE TABLE credentials (
  token_sha256 TEXT PRIMARY KEY,
  city_id TEXT NOT NULL REFERENCES cities(id),
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE INDEX credentials_city ON credentials(city_id);

-- One-time enrollment codes the operator mints, one per city.
CREATE TABLE enroll_codes (
  code TEXT PRIMARY KEY,
  city_name TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER,
  city_id TEXT
);

-- 6-digit invite codes a city prints for its owners; one live code per city at a time.
CREATE TABLE invite_codes (
  code TEXT PRIMARY KEY,
  city_id TEXT NOT NULL REFERENCES cities(id),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER
);
CREATE INDEX invite_codes_city ON invite_codes(city_id);

-- An owner's phone, bound to exactly one city by the invite text whose wamid is bound_wamid.
-- last_inbound_at is the owner's last customer message (Meta's own timestamp): the window's clock.
CREATE TABLE phones (
  wa_id TEXT PRIMARY KEY,
  city_id TEXT NOT NULL REFERENCES cities(id),
  profile_name TEXT,
  bound_at INTEGER NOT NULL,
  bound_wamid TEXT NOT NULL,
  last_inbound_at INTEGER
);
CREATE INDEX phones_city ON phones(city_id);

-- What a city is owed: an owner's message, a status of one of the city's sends, an owner joined.
-- status is '' for kind message and joined, so the one UNIQUE index dedups a message and its
-- joined row on wamid alone and a status on wamid plus status.
CREATE TABLE deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  city_id TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('message', 'status', 'joined')),
  wamid TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  wa_id TEXT,
  profile_name TEXT,
  body TEXT,                   -- Meta's object as JSON; NULL once acknowledged or past the hold
  acked_at INTEGER
);
CREATE UNIQUE INDEX deliveries_once ON deliveries(city_id, wamid, status);
CREATE INDEX deliveries_held ON deliveries(city_id, id) WHERE body IS NOT NULL;
CREATE INDEX deliveries_received ON deliveries(received_at);

-- Media ids delivered to a city (Meta keeps a webhook media id 7 days); the media proxy's check.
CREATE TABLE media (
  city_id TEXT NOT NULL,
  media_id TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (city_id, media_id)
);

-- A city's sends by idempotency key. state: claimed (the Graph call may be in flight), sent (wamid
-- recorded), unknown (the call may have reached Meta and no answer said so; the key never sends
-- again). Statuses route to the city by wamid.
CREATE TABLE sends (
  city_id TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  wamid TEXT,
  state TEXT NOT NULL DEFAULT 'claimed' CHECK (state IN ('claimed', 'sent', 'unknown')),
  type TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (city_id, idempotency_key)
);
CREATE INDEX sends_wamid ON sends(wamid);

-- Statuses whose wamid no send has recorded yet: Meta's "sent" can race the send's own write.
-- The send adopts them into its city when it records its wamid; the cron drops them after a day
-- (the gateway's own welcome and canned lines leave theirs here).
CREATE TABLE orphan_statuses (
  wamid TEXT NOT NULL,
  status TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  recipient_id TEXT,
  body TEXT NOT NULL,
  PRIMARY KEY (wamid, status)
);

-- Strangers: when the canned line was last sent, and wrong invite-code attempts in the last day
-- (counted once per wamid, so a Meta redelivery of one attempt is not a second attempt).
CREATE TABLE unknown_senders (
  wa_id TEXT PRIMARY KEY,
  last_replied_at INTEGER,
  code_attempts INTEGER NOT NULL DEFAULT 0,
  attempts_since INTEGER NOT NULL DEFAULT 0,
  last_code_wamid TEXT
);

-- Counters and last_webhook_at; numbers only.
CREATE TABLE stats (
  name TEXT PRIMARY KEY,
  value INTEGER NOT NULL
);
