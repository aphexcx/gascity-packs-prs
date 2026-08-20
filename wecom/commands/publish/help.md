# gc wecom publish

Send a markdown message, an image, or a video to a WeCom chat through the
running wecom adapter.

## Usage

```
gc wecom publish --chat <chatid-or-userid> --text-file <path>
gc wecom publish --chat <chatid-or-userid> --text "message"
gc wecom publish --chat <chatid-or-userid> --image /abs/path/photo.png [--text "caption"]
gc wecom publish --chat <chatid-or-userid> --video /abs/path/demo.mp4 [--text "caption"]
```

## Flags

- `--chat` (required) — WeCom conversation id: the `chatid` for a group
  chat, or the peer's `userid` for a DM.
- `--text-file` — file containing the message body; preferred, since
  reply text with apostrophes/backticks/code is unsafe to interpolate
  into a shell command.
- `--text` — inline message body (simple strings only). WeCom markdown
  (`**bold**`, lists, links); chunked automatically at ~3800 UTF-8 bytes.
- `--image` — local path to an image to send. WeCom accepts jpg/jpeg,
  png, or gif up to 10MB (checked by content, not extension); oversized
  or other formats are rejected with an explanatory error — downscale or
  convert first, the adapter never transcodes. Mutually exclusive with
  `--video`.
- `--video` — local path to a video to send. WeCom accepts mp4 up to
  10MB; same rejection behavior as `--image`.
- With `--image`/`--video`, `--text`/`--text-file` becomes an optional
  caption sent as a **follow-up markdown message** right after the media
  (WeCom image/video messages carry no caption field). Without media,
  exactly one of `--text`/`--text-file` is required.
- `--kind` — `dm` or `room`, only consulted for the transcript record of
  a media send when the adapter has never seen inbound traffic from this
  chat (it learns dm/room from inbound frames; replies never need this).
- `--session` — gc session id credited for the media send's transcript
  entry; defaults to `$GC_SESSION_ID`. The session must own the
  conversation's (agent-)binding for recording to succeed — the message
  itself is delivered regardless, and a recording miss is reported as a
  `note:` on stderr.
- `--idempotency-key` — reuse a previous invocation's key to RESUME a
  failed or ambiguous media send without duplicating whatever already
  reached the chat. Media sends generate one automatically per
  invocation (echoed in the response, and in the retry hint printed on
  failure); pass the echoed key back here when retrying by hand. The
  key must be paired with the same chat/file/caption — the adapter
  answers a mismatched reuse with HTTP 409.

## Transcript

Media sends are recorded in the gc extmsg conversation transcript
(outbound entry naming the file, mime type, size, sha256, source path,
and caption) by way of gc `/extmsg/outbound`; the adapter's idempotency
state guarantees the recording pass never re-sends the media. Text sends
through this command are currently *not* transcript-recorded (they post
straight to the adapter).

## Environment

- `GC_CITY_NAME` (required) — city whose wecom service to use.
- `GC_API_BASE_URL` — gc API base (default `http://127.0.0.1:9443`).
- `GC_SESSION_ID` — default for `--session`.
- `WECOM_ADAPTER_URL` — direct adapter URL override for local testing
  (points at `/publish`; the media endpoint is derived from it).
