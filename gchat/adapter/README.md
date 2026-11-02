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

On SIGTERM the adapter stops intake and saves the ledger immediately, then
drains requests and workers for at most one second before exiting. Request
contexts are canceled with the service, keeping shutdown inside gc's two-second
SIGKILL window even when an outbound request is stuck.

Media and upload directories are created privately and canonicalized with
`filepath.EvalSymlinks` before clients are constructed. This resolves macOS
`/tmp` and `/var` aliases; subsequent attachment access still rejects symlinks.

Consent requests `openid` alongside the Chat and Pub/Sub scopes. The token
file stores the ID token's subject as `identity_user_id` (`users/<numeric id>`),
and `auth --check` prints it. The adapter seeds the ledger identity before
inbound processing, so the first DM needs no preceding outbound message.
Legacy token files still learn from an outbound response and warn once to run
`gc gchat auth`. Without a persisted identity they hold inbound events at
Google until renewed consent or the first outbound response establishes it.
If a create response contradicts the seeded identity, the
observed ID is saved and the client rebuilt; the observed ID then wins across
restarts. The ledger's `identity_seed` records the token subject separately;
only a new subject (new consent) replaces the observed identity with a new seed.
The first upgrade of a seedless ledger seeds once from the token and logs that
upgrade explicitly; an old observed identity can change on that first start.

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

Credential and subscription notices go to `[[identity]].agent` (default
`mayor`), with a stable mail `--dedup` key per city, identity, notice kind and cause.
Dedup suppresses repeats across restarts while the prior notice remains in the
recipient's mailbox. Startup registers once; a healthy run resets connection
restart backoff for the next interruption.

Catch-up runs at startup and after subscription recovery. Unfinished intervals
retry with backoff. Once an hour the adapter performs a full history sweep:
one paginated spaces inventory plus message and space-event history lists for
each eligible space, including those with completed recovery. This discovers
rejoined spaces and retries held history; `/status` and health name it
`hourly history sweep` while it runs. Between sweeps and recovery work, live
messages arrive on the Pub/Sub stream without periodic history reads.

The service holds an advisory lock on `<ledger>.lock` for its lifetime and
records its socket there. Offline state commands take the same lock and refuse
while it is held, including under gc when the shell has no `GC_SERVICE_SOCKET`.
`--force` uses the recorded socket to request `/state`; the service must be in
`recovery-required`. An unheld lock file is stale and does not block recovery.
Never delete the lock file to bypass a running service. If it was removed or
replaced, restart the service: ownership loss permanently revokes the old
process's saves, even if the path later reappears. The standalone socket
defaults to `adapter.sock` beside the ledger. Reset reports that gap messages
may be lost or duplicated; the CLI replaces the ledger only while holding the lock. Use `gc gchat state` for recovery through the running service's proxy.

The OAuth listener requests an ephemeral loopback port; bind failure ends that
auth attempt with an actionable error. It does not retry another port inside
the same attempt. A Pub/Sub `codes.Canceled` response under a still-live context
is fatal to that Receive run, so the app restarts the stream with backoff;
cancellation of the caller context ends the run normally.
