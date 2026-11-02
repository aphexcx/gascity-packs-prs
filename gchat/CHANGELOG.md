# Changelog

## Unreleased

- Inbound attachments always carry a mime_type: Chat sends no contentType for
  Drive links, and gc rejects an attachment without one (HTTP 422), which
  silently dropped any message with a Drive attachment. Drive links without a
  type carry application/vnd.google-apps.file, other untyped attachments
  application/octet-stream, and the text line omits an empty "()". The fake gc
  now enforces gc's rule so the suite catches it.

## 0.1.0

0.1.0: first version, text, threads, DMs, ambient listening, history, files both ways, reactions, catch-up, unresolved deliveries

- Package the Google Chat user-identity adapter as a private gc proxy service.
- Add auth, publish, reply-current, upload, react, read, spaces, status, resolve
  and state commands, including idempotency keys and delivery/recording results.
- Add a doctor check for secrets, forced token refresh, subscription and route.
- Expose pending and unresolved delivery metadata in status for operator recovery.
- Document Google setup, per-city configuration, retained history, delivery
  uncertainty, registration caching and safe ledger recovery.
