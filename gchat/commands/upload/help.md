Usage: gc gchat upload --file PATH [--to MESSAGE] [--top-level] [--session ID] [--conversation-id spaces/ID] [--thread spaces/ID/threads/ID] [--body TEXT | --body-file PATH] [--idempotency-key KEY]
Answer with the command printed by the turn: reply-current requires --to MESSAGE.
Find MESSAGE on its Message id line or in the gchat: item of its first line.
The adapter takes the space and thread from the message. Edit, deletion and reaction ids also work.
publish and upload require --to or --conversation-id; both must agree. An additional --thread must agree too.
publish with a space starts a new topic and answers nothing. --top-level sends without a thread.
The result names conversation_id, space_name when known, thread and in_reply_to.

Use --help or -h to display this help.

Keep the printed idempotency key and pass --idempotency-key KEY when retrying. --body-file preserves newlines. Upload requires an absolute file path within the configured upload root.

Uploads allow one second per 256 KiB plus command overhead. The server budgets
against the configured maximum (up to Chat's 200 MiB cap); the command budgets
against the file size and adds 20 seconds for the proxy response.
