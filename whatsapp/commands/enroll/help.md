# gc whatsapp enroll

```
gc whatsapp enroll --code <enrollment code> --name "<city name>"
```

Enrolls this city with the Dynasty WhatsApp gateway once. The operator mints
the enrollment code at the gateway (one per city). The gateway answers with a
per-city credential, which this verb writes into the adapter's env file as
`WHATSAPP_GATEWAY_CREDENTIAL` (mode 0600) and never prints. Enrollment,
startup and the doctor all select `$GC_WHATSAPP_ADAPTER_ENV` when non-empty,
else `$GC_SERVICE_SECRETS_DIR/env` when that directory variable is non-empty,
else `$HOME/.config/gc-whatsapp-adapter/env`, with relative paths based at
the pack's adapter directory and `GC_CITY_PATH` never changing the selection.
`WHATSAPP_GATEWAY_URL` defaults to
`https://whatsapp.dynasty.computer`. Restart the whatsapp service afterwards.

A city already holding a credential is refused: ask the operator to revoke it
first.
