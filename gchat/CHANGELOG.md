# Changelog

## Unreleased

- Resolve reports whether a replay landed or is waiting and why. Each replay
  clears its waiting and end mail marks, so a stuck replay is mailed once
  after two minutes even if an earlier life was mailed. Clear a fragment
  from the input box before replaying it. Pending waiting-mail age counts
  from the later of recording and the latest POST; a burst released by a
  stuck head or gc outage gets a fresh two minutes. No ledger field is added.
- Text status includes the raise time, end mail time and fragment mark.

- A Chat message gc answers `failed` or `held` (nothing reached the agent's
  terminal) is retried until it lands, no longer given up after four POSTs:
  the first four run as before, then the waits are 8, 16 and 32 seconds and
  one minute, for up to 24 hours from when the adapter recorded it (then it
  is unresolved as `not delivered within the retry bound`). `partial` keeps
  its four-POST bound (a `partial` first met after the fourth POST stops
  there), and so does `held` with a `partial` member, also when gc answers
  the message's other POSTs `failed`: the record keeps the mark across a
  restart, and a replay clears it. gc's `held` status, from gc's
  composer-hold change, is understood.
- The messages of one space reach gc in the order they were recorded: a later
  message waits while an earlier one is retrying or not yet posted. A message
  pending at gc holds nothing back, so a burst to a busy agent flows.
- A retrying message, or one waiting behind it, no longer keeps catch-up
  open: health can read `ready` while messages wait or are stuck, and
  outbound recovery keeps running. `status` and the mail show them.
- A message waiting two minutes or more, and one that becomes unresolved, is
  mailed to the identity's agent (`gchat: delivery`): ids, cause, age and
  commands, never the text, the sender or gc's error text. Of the messages
  waiting in one space only the first is mailed, naming how many wait behind
  it. The waiting mail says the message still arrives when the session
  clears. A message mailed while waiting gets one more mail if it ends
  unresolved for a reason other than the 24-hour bound, a withdrawal
  included; no message is mailed more than twice in one life (a replay
  starts a new one). A withdrawn message never mailed while waiting, and an
  unresolved row from before this version, are not mailed. A mail still in
  flight when a replay starts marks nothing, so the replay is mailed as if
  that mail had not been sent; each life of a message has its own mail keys
  at gc.
- `gc gchat status` shows each delivery's age, cause, POST count, next attempt
  and raise time, and counts `stuck` deliveries.
- A receipt that concludes `pending` after every member took the whole paste
  commits as `landed_unconfirmed` (counted under `turns:<space>` and
  `unconfirmed:<space>`); a terminal hold is unresolved with the cause `held`.
- A pending receipt's one-hour deadline counts from the latest POST, not from
  when the message was recorded.
- A record adapter 0.5.0 left between two saves after a fourth "not
  delivered" (intent, four POSTs, no cause) is never posted again: it ends
  unresolved as `delivery retries exhausted`, cause `unknown`, and is mailed.
- The ledger's change record gains `next_attempt`, `posted`, `cause`,
  `fragment`, `raised` and `raised_end`, each omitted when empty; the version
  stays 1 and a 0.5.0 ledger loads and saves unchanged. Going back to 0.5.0
  turns a waiting message with four or more POSTs unresolved without a mail
  and posts a message that waited behind another at once: resolve waiting
  rows first.
- Docs only: the README's sample owners line uses an invented given name. No
  code change.

## 0.5.0 — 2026-11-09

- Prefer a prebuilt adapter on PATH (or GC_GCHAT_ADAPTER_BIN) before building;
  stamp the pack version into local and Linux release binaries.
- Keep unrecognized per-space history 403s scoped to that space; reserve credential holds for 401 and scope errors.
- Preserve supported nonempty path and endpoint overrides in shell launchers; keep authentication mode file-based, matching the binary.
- Bound subscription create, get, list, renew, and reactivate requests to the two-minute operation timeout.
- Give uploads a size-based transfer budget and align the command's timeout, including automatic retries.
- Retire spaces whose history returns 404, preserving fetched records and allowing live-space send recovery.
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
