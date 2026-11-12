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
else `$GC_CITY_PATH/.gc/services/whatsapp/secrets/env` when the city path is
non-empty, else `$HOME/.config/gc-whatsapp-adapter/env`, so a command that gets
the city path and a supervised service that gets its secrets directory use
the same file.
Relative paths are based at the pack's adapter directory; a missing selected
file never causes a fallback to another file.
`WHATSAPP_GATEWAY_URL` defaults to
`https://whatsapp.dynasty.computer`. Restart the whatsapp service afterwards.

A city already holding a credential is refused: ask the operator to revoke it
first.
