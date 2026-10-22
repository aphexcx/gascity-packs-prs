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

The canonical Chat user ID is persisted in the ledger. A fresh ledger learns it
from the first successful outbound message or reaction. Until then, inbound
changes are retained at Google and catch-up waits; status explains the hold.
If a create response contradicts the stored identity, the observed ID is saved
and the client is rebuilt before it can remove reactions under that identity.

Offline state commands reserve the service socket while replacing the ledger.
The default socket is `adapter.sock` beside the ledger when `GC_SERVICE_SOCKET`
is unset. With a live socket, `--force` requests the service's `/state` recovery
operation; the service must be in `recovery-required`. Reset reports that gap
messages may be lost or duplicated. No live ledger is overwritten by the CLI.
