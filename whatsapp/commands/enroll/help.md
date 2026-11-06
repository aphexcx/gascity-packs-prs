# gc whatsapp enroll

```
gc whatsapp enroll --code <enrollment code> --name "<city name>"
```

Enrolls this city with the Dynasty WhatsApp gateway once. The operator mints
the enrollment code at the gateway (one per city). The gateway answers with a
per-city credential, which this verb writes into the adapter's env file as
`WHATSAPP_GATEWAY_CREDENTIAL` (mode 0600) and never prints. The env file is
`$GC_WHATSAPP_ADAPTER_ENV`, else `<city>/.gc/services/whatsapp/secrets/env`,
else `~/.config/gc-whatsapp-adapter/env`; `WHATSAPP_GATEWAY_URL` defaults to
`https://whatsapp.dynasty.computer`. Restart the whatsapp service afterwards.

A city already holding a credential is refused: ask the operator to revoke it
first.
