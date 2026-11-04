# Changelog

## 0.0.1 (2026-11-04, gp-9hf1)

- The whatsapp pack: a `proxy_process` adapter that polls the Dynasty WhatsApp
  gateway, delivers each owner's messages to gc extmsg through a write-ahead
  journal, publishes the mayor's replies inside the 24-hour window and the one
  update template outside it, saves media and transcribes voice notes, and
  runs the window-keeper; nine doctor rows; the `gc whatsapp` verbs.
- The design gate r7's two recovery rules, built: every retained watermark is
  read before any plain recovery and on a schedule while the adapter runs (a
  gc-only restore while running is found before the next plain post); a dead
  journal entry is kept until the gateway confirms its ack.
