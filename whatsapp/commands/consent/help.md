# gc whatsapp consent

```
gc whatsapp consent <conversation_id> --off
gc whatsapp consent <conversation_id> --on
```

Turns the window-keeper off or on for one owner's conversation. Run `--off` the
moment the owner withdraws consent: the keeper then raises no event for that
conversation (no daily update, no close ladder). `--on` resumes from the next
mark.

The conversation id is the one gc shows (`whatsapp/<conversation_id>`): the
owner's phone number, or a WhatsApp user id (`US.1349...`) for an owner who
joined without one. It may be typed in any form (`+55 11 99999-0001`); it must
be a conversation that has joined. The verb prints the last four digits of the
conversation the adapter matched; an unknown one is refused and nothing is
recorded.
