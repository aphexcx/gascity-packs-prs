# gc whatsapp hours

```
gc whatsapp hours <wa_id> --hours "Mon-Fri 09:00-18:00" --tz America/Sao_Paulo
gc whatsapp hours <wa_id> --hours "Mon-Sat 18:00-02:00" --tz America/Chicago
gc whatsapp hours <wa_id> --hours "Mon-Fri 09:00-18:00, Sat 10:00-14:00" --tz America/Sao_Paulo
```

Records a business's hours and time zone for the window-keeper. The daily
update lands two hours before the close of business; when the window would
close outside the hours, the four nudges move before the close of business,
and a mark that falls outside every span is dropped, never deferred.
