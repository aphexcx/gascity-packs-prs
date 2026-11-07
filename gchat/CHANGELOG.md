# Changelog

## Unreleased

- Persist join-time history bounds and clamp catch-up overlap so pre-join messages stay unread.
- Serialize media accounting and downloads across readers; refuse downloads when usage cannot be measured.
- Clear the learned organization domain atomically when the authenticated identity changes.
- Reuse or strip provenance frames only when the ledger marks them adapter-authored; preserve legacy user text.
- Retry unknown managed identities and rebuild the running Chat client after discovery.
- Bound sender directory lookups to 30 seconds, then use the existing external fallback.
- Inbound attachments always carry a mime_type: Chat sends no contentType for
  Drive links, and gc rejects an attachment without one (HTTP 422), which
  silently dropped any message with a Drive attachment. Drive links without a
  type carry application/vnd.google-apps.file, other untyped attachments
  application/octet-stream, and the text line omits an empty "()". The fake gc
  now enforces gc's rule so the suite catches it.

## 0.5.0

- Bound inbound attachments: `media.max_attachments` (default 20) caps the
  files one message downloads and `media.max_total_bytes` (default 2 GiB) caps
  the store. Past either bound the attachment is reported unavailable —
  "attachment limit reached" or "storage quota reached" — and nothing already
  stored is deleted, because the agent may still be reading a path it was
  handed.
- Refuse endpoint overrides that would put a credential on the wire: the Chat,
  events, People and both OAuth bases must be `https`, or plain `http` on this
  machine, with no credentials or query. `GCHAT_PUBSUB_ENDPOINT` must name this
  machine too, since its transport is unauthenticated. The error names the key
  and the value, and the adapter does not start.

## 0.4.1

- The frame's directory lookup asks `people.get` for `READ_SOURCE_TYPE_PROFILE`
  only. `READ_SOURCE_TYPE_DOMAIN_PROFILE` is not a `ReadSourceType` and Google
  answers HTTP 400, so every sender resolved to no address and was classed
  `external`, owners included.

## 0.4.0

- The frame line ends with the message's own id, `| item
  gchat:spaces/A/messages/B`, so the agent can name the message it is
  acting on and the box's key holder can look it up at the source (sender
  and space members, from the Chat and People APIs) before reading another
  mailbox on that person's behalf. Reactions and the adapter's own notices
  carry no item.

## 0.3.0

- Every turn starts with a provenance frame, `[from: Name <address> |
  class]`, written from Chat's record of the sender: `owner` (`GCHAT_OWNERS`),
  `organization` (`GCHAT_ORGANIZATION_DOMAIN`, or the identity's own Workspace
  domain learned from its messages), `external` (everyone else, bots, deleted
  users). Addresses are read from the Workspace directory with the key holder's
  `directory.readonly`; without it the frame shows `users/<id>`. Invisible
  characters (zero-width, bidi overrides, Unicode tags) are removed and counted
  in a trailing line. A deletion keeps the original turn's frame. History
  (`gc gchat read`) returns the text without the frame.

## 0.2.0

- Add `GCHAT_TOKEN_SOURCE=keyholder`: the adapter asks a key holder over a Unix
  socket (`GCHAT_KEYHOLDER_SOCKET`) for the identity's access tokens instead of
  refreshing a desktop OAuth credential. No client file, no token file, no
  keyboard sign-in; a refused mint is retried every 30 seconds, and the Chat
  identity is learned from the Workspace directory (`directory.readonly`).
- With a key holder, a Chat refusal of a freshly minted token (no Chat app
  configured, Chat off for the user, a new user without a conversation) keeps
  the adapter retrying in `initializing` with one `chat-access` notice, instead
  of parking in `needs-sign-in` for a sign-in nobody can do.
- `gc gchat auth` forwards to `boxboxbox-google enroll` when sign-in is managed;
  `auth --check` asks the key holder.
- Drop a leading agent handle (`**mayor:**`) from outbound text: gc's reply
  reminder asks for it, Chat already shows the identity.
- Sweep a newly joined space's history from the join time: live, Google's
  all-spaces subscription delivered the identity's membership but not the
  message posted in the new space seconds later. Nothing before the join is
  read.
- With a key holder, learn the Chat identity from the user's own direct
  messages when the Workspace directory is closed to it.
- Hold a space, not the credential, on Google's generic 403 ("permission
  denied ... or the resource doesn't exist"): seen live for a direct message the
  identity was invited to as an external user and never joined. Only a 401 or
  an explicit "insufficient authentication scopes" is a credential problem.
- Send Chat's reply-fallback option only with a thread: Google refuses it on a
  top-level post into a named space ("does not specify which message to reply
  to", seen live), which broke `publish` and `upload` without `--thread`.
- `upload` passes the file's physical path: the service checks it against the
  upload root without following symlinks, and a home directory may be one.
- Run against gc 1.5: its inbound answer is the synchronous routing result
  (mapped to `delivered` or `no_route`; no receipts), and it rejects
  `reply_instructions` on registration, which is then resent without them.
  The contract test accepts both the 1.5 and the newer shapes.
- Ship the `google-chat` skill: when to speak, how to reply in a thread, files,
  reactions, history, sharing work.

## 0.1.0

0.1.0: first version, text, threads, DMs, ambient listening, history, files both ways, reactions, catch-up, unresolved deliveries

- Package the Google Chat user-identity adapter as a private gc proxy service.
- Add auth, publish, reply-current, upload, react, read, spaces, status, resolve
  and state commands, including idempotency keys and delivery/recording results.
- Add a doctor check for secrets, forced token refresh, subscription and route.
- Expose pending and unresolved delivery metadata in status for operator recovery.
- Document Google setup, per-city configuration, retained history, delivery
  uncertainty, registration caching and safe ledger recovery.
