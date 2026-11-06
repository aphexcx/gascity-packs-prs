# gc whatsapp publish

Send text to an owner's WhatsApp chat through the running whatsapp adapter.

## Usage

```
gc whatsapp publish --to <conversation_id> --text-file <path>
gc whatsapp publish --to <conversation_id> --text "message"
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
- `--update`: send the ONE approved utility template instead (`dynasty_update`,
  "Your city has an update: {{1}}", the text as its one parameter). Only for an
  actual update, and only when the 24-hour window is closed: the adapter refuses
  it with HTTP 409 while the window is open (a free reply is cheaper) and with
  HTTP 400 on empty text. Every template send is journaled with its text and
  time to `<city>/.gc/whatsapp/updates.jsonl`. A template never reopens the
  window: only the owner's next message does.
- `--idempotency-key`: rerun a send under the key the first run printed; the
  adapter resumes after the chunks already delivered and never repeats one.

## The window

WhatsApp lets the business write freely only within 24 hours of the owner's
last message (minus a 120-second margin). Outside it the gateway refuses
free-form text; the adapter answers gc's permanent publish error with
`metadata.reason window_closed` and a hint naming `--update`, and this verb
prints the hint and exits 1. Text over 4096 characters is split into several
messages, preferring a line break.
