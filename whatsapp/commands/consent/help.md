# gc whatsapp consent

```
gc whatsapp consent <wa_id> --off
gc whatsapp consent <wa_id> --on
```

Turns the window-keeper off or on for one owner's phone. Run `--off` the
moment the owner withdraws consent: the keeper then raises no event for that
conversation (no daily update, no close ladder). `--on` resumes from the next
mark.

The wa_id may be typed in any form (`+55 11 99999-0001`); it must be a phone
that has joined. The verb prints the last four digits of the conversation
the adapter matched; an unknown phone is refused and nothing is recorded.
