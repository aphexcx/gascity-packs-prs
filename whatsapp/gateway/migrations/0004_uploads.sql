-- Outbound metadata only. Old code ignores this additive table during deployment.
CREATE TABLE uploads (
  city_id TEXT NOT NULL REFERENCES cities(id),
  idempotency_key TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  media_id TEXT UNIQUE NOT NULL,
  meta_id TEXT,
  kind TEXT NOT NULL,
  mime TEXT NOT NULL,
  filename TEXT NOT NULL,
  bytes INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('claimed', 'ready', 'unknown', 'length_mismatch')),
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY (city_id, idempotency_key)
);
CREATE INDEX uploads_expiry ON uploads(expires_at);
