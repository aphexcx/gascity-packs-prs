# Adapter runtime

`run.sh` loads the per-city env file, rebuilds stale sources, and starts the
service. The binary also accepts `auth`, `auth --check`, `version`,
`state restore <file>`, and `state reset`.

The socket serves health immediately while initialization and catch-up run in
the background. Health stays HTTP 200 with `X-GC-Health: degraded` until ready.
Token replacements are validated by the token watcher; accepted replacements
drain current work and rebuild the clients. Settings file changes are validated
before replacing the active settings.

Media and upload directories are created privately and canonicalized with
`filepath.EvalSymlinks` before clients are constructed. This resolves macOS
`/tmp` and `/var` aliases; subsequent attachment access still rejects symlinks.

Consent requests `openid` alongside the Chat and Pub/Sub scopes. The token
file stores the ID token's subject as `identity_user_id` (`users/<numeric id>`),
and `auth --check` prints it. The adapter seeds the ledger identity before
inbound processing, so the first DM needs no preceding outbound message.
Legacy token files still learn from an outbound response and warn once to run
`gc gchat auth`. If a create response contradicts the seeded identity, the
observed ID is saved and the client rebuilt; the observed ID then wins.

A rejected access token triggers a refresh before requiring sign-in. Permanent
refresh failure, or two consecutive credential rejections within five minutes
despite refresh, holds delivery until a validated token replacement.

Catch-up runs at startup and after subscription recovery. Unfinished intervals
retry with backoff; completed recovery stays idle. An hourly inventory refresh
can discover rejoined spaces and retry held history. Live messages arrive on
the Pub/Sub stream.

The service holds an advisory lock on `<ledger>.lock` for its lifetime and
records its socket there. Offline state commands take the same lock and refuse
while it is held, including under gc when the shell has no `GC_SERVICE_SOCKET`.
`--force` uses the recorded socket to request `/state`; the service must be in
`recovery-required`. An unheld lock file is stale and does not block recovery.
Never delete the lock file to bypass a running service. The standalone socket
defaults to `adapter.sock` beside the ledger. Reset reports that gap messages
may be lost or duplicated; the CLI replaces the ledger only while holding the lock.
