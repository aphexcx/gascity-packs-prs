Usage: gc gchat upload --file PATH [--session ID] [--conversation-id spaces/ID] [--thread spaces/ID/threads/ID] [--body TEXT | --body-file PATH] [--idempotency-key KEY]
publish and upload require --conversation-id; upload also requires --file PATH.
reply-current uses the latest inbound conversation, then its active binding; pass --thread to reply in a thread.

Use --help or -h to display this help.

Keep the printed idempotency key and pass --idempotency-key KEY when retrying. --body-file preserves newlines. Upload requires an absolute file path within the configured upload root.
