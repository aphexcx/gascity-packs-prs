# gc whatsapp hours

```
gc whatsapp hours <conversation_id> --hours "Mon-Fri 09:00-18:00" --tz America/Sao_Paulo
gc whatsapp hours <conversation_id> --hours "Mon-Sat 18:00-02:00" --tz America/Chicago
gc whatsapp hours <conversation_id> --hours "Mon-Fri 09:00-18:00, Sat 10:00-14:00" --tz America/Sao_Paulo
gc whatsapp hours <conversation_id> --clear [--tz America/Sao_Paulo]
```

Records a business's hours and time zone for the window-keeper. The daily
update lands two hours before the close of business; when the window would
close outside the hours, the four nudges move before the close of business,
and a mark that falls outside every span is dropped, never deferred.

`--clear` removes the recorded hours: the update then comes at the fixed
hour of the business's day and the nudges run at any hour. The recorded zone
stays unless `--tz` is given (`--tz ""` clears it too).

The conversation id is the one gc shows (`whatsapp/<conversation_id>`): the
owner's WhatsApp user id (`US.1349...`), or, for an owner bound before user ids
(PR 73), the phone number it joined from. A user id must be given whole,
exactly as gc shows it; a phone number may be typed in any form
(`+55 11 99999-0001`). It must be a conversation that has
joined. The verb prints the last four digits of the conversation the adapter
matched; an unknown one is refused and nothing is recorded.
