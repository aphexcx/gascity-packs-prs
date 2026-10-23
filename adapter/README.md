# Adapter runtime

`run.sh` loads the per-city env file, rebuilds stale sources, and starts the
service. The binary also accepts `auth`, `auth --check`, `version`,
`state restore <file>`, and `state reset`.

`serve`, `auth`, and `state` all load endpoint and path overrides from the
resolved env file, including `GCHAT_STATE_FILE`, `GCHAT_MEDIA_DIR`,
`GCHAT_UPLOAD_ROOT`, `GCHAT_CHAT_API_BASE`, `GCHAT_EVENTS_API_BASE`, and
`GCHAT_PUBSUB_ENDPOINT`. Nonempty process environment values take precedence.

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
observed ID is saved and the client rebuilt; the observed ID then wins across
restarts. The ledger's `identity_seed` records the token subject separately;
only a new subject (new consent) replaces the observed identity with a new seed.

A rejected access token triggers a refresh before requiring sign-in. Permanent
refresh failure, or two consecutive credential rejections within five minutes
despite refresh, holds delivery until a validated token replacement.

Workspace Events API 403/404 responses are subscription setup failures, shown
as `catching-up` with `subscription refused: <sanitized cause>`. They send one
subscription notice per incident, including the cause and the README setup
checklist: enable the Workspace Events API and grant the Chat service account
Publisher on the topic. Reconciliation follows the subscriber's backoff
(including discovery after ambiguous mutations), and logs `subscription
recovered` on success. These failures do not ask for sign-in.

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
