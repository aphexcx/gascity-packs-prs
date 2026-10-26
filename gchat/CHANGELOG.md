# Changelog

## 0.1.0

0.1.0: first version, text, threads, DMs, ambient listening, history, files both ways, reactions, catch-up, unresolved deliveries

- Package the Google Chat user-identity adapter as a private gc proxy service.
- Add auth, publish, reply-current, upload, react, read, spaces, status, resolve
  and state commands, including idempotency keys and delivery/recording results.
- Add a doctor check for secrets, forced token refresh, subscription and route.
- Expose pending and unresolved delivery metadata in status for operator recovery.
- Document Google setup, per-city configuration, retained history, delivery
  uncertainty, registration caching and safe ledger recovery.
