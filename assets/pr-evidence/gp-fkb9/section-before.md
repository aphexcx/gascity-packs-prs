## How to reply

Answer with the reply command printed by the turn you are answering:

```bash
gc gchat reply-current --to spaces/<id>/messages/<id> --body 'Short plain reply.'
```

`--body-file <path>` suits long or multi-line replies. Do not lead with your
handle: Chat already shows your name, and a leading `mayor:` is dropped.

The message reference is on the turn's Message id line or in the `gchat:`
item in its first line. The adapter takes the space and thread from that
message. Edit, deletion and reaction ids also work. `--to` is required even
when an older turn prints a command with only a space. Do not copy another
turn's command: you still choose which message your text answers.

Use `--top-level` to send in that message's space without its thread. If you
also supply a space or thread, it must agree with the reference.

To start a new topic that answers nothing:
`gc gchat publish --conversation-id spaces/<id> --body-file post.md`.
Publish and upload also accept `--to MESSAGE` to answer a message. Every send
prints its destination as `conversation_id`, `space_name` when known,
`thread`, and `in_reply_to`; check that it is the message you meant to answer.

Every send prints an idempotency key on stderr before sending. If the
command times out or fails with a retry hint, retry the same send with
`--idempotency-key <key>`; a new invocation without it is a new message.

React with the message id and the emoji character:

```bash
gc gchat react --message spaces/<id>/messages/<id> --emoji 👀
```

Keep replies short and plain; Chat is conversation, not a report. Markdown
(`**bold**`, `*italic*`, `` `code` ``, fenced blocks, `- lists`,
`[links](https://...)`) is converted to Chat's formatting; messages longer
than Chat allows are split at paragraph boundaries.
