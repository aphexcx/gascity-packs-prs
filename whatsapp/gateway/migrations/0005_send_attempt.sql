-- Opaque adapter attempt identity; existing sends remain valid with NULL.
ALTER TABLE sends ADD COLUMN activity_attempt_id TEXT;
