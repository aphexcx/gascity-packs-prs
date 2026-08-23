Reply to the latest Slack inbound event seen by the current session.

The session's recent transcript is scanned for the most recent
`extmsg.inbound` system-reminder. The reply is published through the
local Slack adapter's /publish endpoint to the same conversation.

When that latest inbound was a thread reply, the reply inherits its
thread_ts and lands in the same thread — including when
--conversation-id names the same conversation explicitly. An
unthreaded inbound keeps the channel-level reply. Use --no-thread to
force a channel-level post, or --reply-to <ts> to anchor elsewhere.
--thread-current threads under that latest inbound either way, at its
thread root when it was a thread reply and at its own ts when it was
not.

The inherited anchor is the newest inbound in the conversation, which
is not always the message being answered: in a shared channel, a
threaded message from someone else that arrives between that message
and this reply becomes the anchor, and the reply lands in their thread
instead. Nothing on the wire identifies which inbound woke the
session, so the command cannot tell the two apart. Pass --reply-to
<ts> when the anchor has to be exact, or --no-thread to stay at
channel level. When inheritance fires it prints `inheriting thread
<ts> from inbound <mid>` on stderr and reports reply_to_message_id in
the result JSON, so a reply that lands in an unexpected thread can be
traced back to the inbound that donated the anchor.

Examples:
  gc slack reply-current --body "ack"
  gc slack reply-current --body-file /tmp/reply.txt
  gc slack reply-current --turn-ref gct-0123456789abcdef0123 --body-file /tmp/reply.txt
  gc slack reply-current --conversation-id D0B0TTS550F --body "explicit channel"
  gc slack reply-current --body "top-level on purpose" --no-thread

If the session has no inbound history, --conversation-id is required.

Company reminders include an immutable `turn_ref` and an exact command. Copy
that command: `--turn-ref` binds the reply to the originating channel/thread
even if a newer message in another channel wakes the same agent session.
Post-rollout company turns fail closed when the flag is omitted.

Formatting guard: tildes that would accidentally pair into Slack
strikethrough (e.g. "~$58.5k … ~$16.5k" — tilde as "approximately") are
neutralized by default with a visually identical substitute (U+223C).
Deliberate tight-wrapped `~word~` strikethrough, code spans, lone
tildes, and every other formatting character pass through untouched.
"Lone" counts per line, though: two home-relative paths on one line
(`rsync ~/a ~/b`) can pair, so both are substituted — put twin paths in
a code span, or pass --raw to send the body byte-for-byte verbatim.

Receipt: on success the command prints a terse JSON receipt — delivered
flag, message_id, conversation_id, and thread_ts when threaded (gp-9e7).
A delivered=false receipt adds failure_kind, plus an error message when
the failing layer supplied one (direct-chat failure receipts often
carry only failure_kind). Pass
--verbose to print the full result envelope (the raw receipt, including
gc's transcript entry) instead. Company-turn replies follow the same
contract: the default is the terse receipt, and --verbose prints the
full company report (status/kind/nonce, delegation_key, allow_partial).
A company post parked on a transient failure is delivered=false with
failure_kind "parked" — recovery is automatic, and a retry is safe (the
durable intent reconciles instead of reposting).
