# gc wecom publish

Send a markdown message to a WeCom chat through the running wecom adapter.

## Usage

```
gc wecom publish --chat <chatid-or-userid> --text "message"
```

## Flags

- `--chat` (required) — WeCom conversation id: the `chatid` for a group
  chat, or the peer's `userid` for a DM.
- `--text` (required) — message body. WeCom markdown (`**bold**`, lists,
  links); chunked automatically at 3800 chars.

## Environment

- `GC_CITY_NAME` (required) — city whose wecom service to use.
- `GC_API_BASE_URL` — gc API base (default `http://127.0.0.1:9443`).
- `WECOM_ADAPTER_URL` — direct adapter URL override for local testing.
