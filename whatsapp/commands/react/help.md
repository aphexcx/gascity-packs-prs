# gc whatsapp react

Place a reaction on one message:

```sh
gc whatsapp react --to <conversation_id> --message <message_id> --emoji 👀
```

All three flags are required. Use the id on the inbound message's "Message id:"
line. `--emoji` takes the emoji character itself, not a name such as `eyes`.
An empty value (`--emoji ""`) removes the reaction. A reaction needs the open
24-hour window; it cannot use an update template or reopen the window.

`--session <session_id>` overrides `GC_SESSION_ID`. A session is required.
The command prints its resume key on stderr before sending. Reuse it with
`--idempotency-key <key>` after a transport failure; do not change the target
or emoji under that key. Exit 0 prints the receipt on stdout; exit 1 explains
a refusal on stderr; exit 2 means a usage error. When a reaction fails, say
the same thing in words by the window rule.
