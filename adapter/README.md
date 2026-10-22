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

Offline state commands reserve the service socket while replacing the ledger.
The default socket is `adapter.sock` beside the ledger when `GC_SERVICE_SOCKET`
is unset. With a live socket, `--force` requests the service's `/state` recovery
operation; the service must be in `recovery-required`. Reset reports that gap
messages may be lost or duplicated. No live ledger is overwritten by the CLI.
