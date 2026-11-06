# gc whatsapp invite

Mint an invite code for an owner to join this city on the Dynasty WhatsApp
number.

## Usage

```
gc whatsapp invite
gc whatsapp invite --proactive-consent --hours "Mon-Fri 09:00-18:00" --tz America/Sao_Paulo
```

Prints a 6-digit code valid for 24 hours (one live code per city) and the two
lines the owner is told: save the Dynasty number as a contact named "Dynasty",
then text it the code. The text binds that phone to this city; the mayor sees
"<name> joined from <last four digits>".

## The window-keeper's onboarding fields

- `--proactive-consent`: the business agreed that the mayor may raise a daily
  update and short "please reply" nudges before the 24-hour window closes.
  Without it the keeper stays OFF for that phone.
- `--hours "<days> HH:MM-HH:MM[, ...]"`: business hours; a span may cross
  midnight (`18:00-02:00`). The daily update lands two hours before the close
  of business; nudges are kept inside the hours.
- `--tz <IANA zone>`: the business's time zone (required with `--hours`).

Change them after the join with `gc whatsapp consent` and `gc whatsapp hours`.
