# gc whatsapp publish

Send text or one file to an owner's WhatsApp chat through the running adapter.

## Usage

```
gc whatsapp publish --to <conversation_id> --text-file <path>
gc whatsapp publish --to <conversation_id> --text "message"
gc whatsapp publish --to <conversation_id> --file <path> [--caption "text" | --caption-file <path>]
gc whatsapp publish --update --to <conversation_id> --text "today's update"
```

## Flags

- `--to` (required): the conversation id gc shows in the paste
  (`whatsapp/<conversation_id>`): the owner's WhatsApp user id, or, for an
  owner bound before user ids (PR 73), the phone number it joined from. It
  stays the same when the owner changes numbers. A phone number that is not
  the conversation id is refused: the send names nobody by a number.
- `--text-file`: a file holding the message; preferred, since reply text
  with quotes or code is unsafe to put on a command line.
- `--text`: an inline message (simple strings only).
- `--file`: one file, with its type checked from its bytes against its extension.
  Images (JPEG/PNG/static WebP) cap at 5 MB; audio (AAC/AMR/MP3/M4A/mono Opus
  Ogg) and video (MP4/3GP) at 16 MB; documents (PDF, UTF-8 text, Office) at
  100 MB. MB here is 1,000,000 bytes. Files cannot accompany `--update`,
  `--text` or `--text-file` (use `--caption-file` instead).
- `--caption` / `--caption-file`: optional caption, up to 1,024 characters.
  Audio has no captions. Caption files follow the same path rule as media.
  Paths are resolved by the adapter and must be inside `WHATSAPP_OUTBOUND_ROOTS`
  (default city directory and inbound media store). `.gc/services`, `.gc/keys`,
  `secrets`, `.ssh`, `.config`, and names ending `.env`/`.pem` or starting `id_`
  are always forbidden, including through symlinks. Never send secrets or
  unexpected files, or pass a file between people's chats without their request.
- `--update`: send the ONE approved utility template instead (`dynasty_update`,
  "Your city has an update: {{1}}", the text as its one parameter). Only for an
  actual update, and only when the 24-hour window is closed: the adapter refuses
  it with HTTP 409 while the window is open (a free reply is cheaper) and with
  HTTP 400 on empty text. Every template send is journaled with its text and
  time to `<city>/.gc/whatsapp/updates.jsonl`. A template never reopens the
  window: only the owner's next message does.
- `--idempotency-key`: rerun a send under the key the first run printed; the
  adapter resumes after the chunks already delivered and never repeats one.
  For a file, the upload handle is journaled and both upload and send recover
  under that same operation key. Keep the original file and caption unchanged.

## The window

WhatsApp lets the business write freely only within 24 hours of the owner's
last message (minus a 120-second margin). Outside it the gateway refuses
free-form text; the adapter answers gc's permanent publish error with
`metadata.reason window_closed` and a hint naming `--update`, and this verb
prints the hint and exits 1. Text over 4096 characters is split into several
messages, preferring a line break.

Files use that same window and never extend it. They cannot use the update
template: wait for the owner to write again. A failed upload sends nothing.
The transcript records the filename, byte count, SHA256 and caption, never
the file bytes. If an upload/send outcome is unknown, check it before using
a new key; a retry under the original key never makes a second Meta call.
