# gc whatsapp inbound clear-gap

```
gc whatsapp inbound clear-gap <conversation_id>
```

Name the conversation exactly as gc shows it (`whatsapp/<conversation_id>`):
a phone number's digits, or a WhatsApp user id whole.

The adapter keeps, per conversation, a WATERMARK: the highest transcript
sequence gc accepted from it. It reads the gc store at every watermark at
start, every minute, after every poll that brings a new message, and before
any plain repost. When the store no longer holds a watermark's record (a
beads store restored from an older backup, or rebuilt), a RESTORE GAP is
recorded and the doctor row `restore` turns yellow; while any gap stands,
every in-doubt message of every conversation is resent once with a marker
line, never as a plain duplicate.

Clearing a gap prints it and the store's head, removes it, and REBASES the
conversation's watermark to the store as it reads now, so the next read is
intact unless the store moves again. Clear only once you know why the store
went back.
