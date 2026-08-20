# gc wecom publish

Send a markdown message to a WeCom chat through the running wecom adapter.

## Usage

```
gc wecom publish --chat <chatid-or-userid> --text-file <path>
gc wecom publish --chat <chatid-or-userid> --text "message"
```

## Flags

- `--chat` (required) — WeCom conversation id: the `chatid` for a group
  chat, or the peer's `userid` for a DM.
- `--text-file` — file containing the message body; preferred, since
  reply text with apostrophes/backticks/code is unsafe to interpolate
  into a shell command.
- `--text` — inline message body (simple strings only). Exactly one of
  `--text` / `--text-file` is required. WeCom markdown (`**bold**`,
  lists, links); chunked automatically at ~3800 UTF-8 bytes.

## Environment

- `GC_CITY_NAME` (required) — city whose wecom service to use.
- `GC_API_BASE_URL` — gc API base (default `http://127.0.0.1:9443`).
- `WECOM_ADAPTER_URL` — direct adapter URL override for local testing.
