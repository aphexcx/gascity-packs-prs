# gc whatsapp consent

```
gc whatsapp consent <wa_id> --off
gc whatsapp consent <wa_id> --on
```

Turns the window-keeper off or on for one owner's phone. Run `--off` the
moment the owner withdraws consent: the keeper then raises no event for that
conversation (no daily update, no close ladder). `--on` resumes from the next
mark.
