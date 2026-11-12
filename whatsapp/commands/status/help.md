# gc whatsapp status

```
gc whatsapp status [--json]
```

Shows the WhatsApp door's state without secrets, message text or full phone
numbers: whether the city is enrolled (a 401 from the gateway stops polling),
the last poll, the inbound entries in flight by state and the acks owed, the
dead letters, every terminal hold by rid (resolve with `gc whatsapp inbound
resolve`), every restore gap by conversation (clear with `gc whatsapp inbound
clear-gap`), and per bound conversation (last four digits) the window's close time and
the window-keeper's state: consent, the day's update raised or not, the marks
fired, the lapses.

Also shows never-silent enabled state, health age or unknown reason, open
outages and their mail sent marks, and the owner-line count for seven days.
Use `--json` for the same adapter status as structured JSON.
