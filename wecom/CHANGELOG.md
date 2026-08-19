# Changelog — wecom pack

## 0.0.1 (unreleased)

- Media ingestion hardening (jg-c7j codex round-1): per-message media
  items download concurrently under a global admission gate
  (`WECOM_MEDIA_MAX_CONCURRENT_DOWNLOADS`, default 3 — bounds worst-case
  buffer memory at slots × size cap) with a URL-expiry deadline
  (`WECOM_MEDIA_URL_TTL_MS`) for queued waiters; downloads get a true
  wall-clock abort (per-request AbortSignal + promise deadline — axios
  `timeout` never fires on a trickling body); items without a valid
  32-byte base64 aeskey fail before download (SDK would return raw
  ciphertext); the durable store gains a quota
  (`WECOM_MEDIA_QUOTA_BYTES`, 10GiB) and min-free-disk floor
  (`WECOM_MEDIA_MIN_FREE_BYTES`, 5GiB) — breaches reject the save with a
  note, never delete (append-only store); attachment URLs built with
  `pathToFileURL`; wire cap allows +32B PKCS#7 overhead over the
  plaintext cap; hydration replay map is capped + TTL-bounded. Inbound
  wiring extracted to `adapter/src/inbound.js` and covered by
  integration tests with a fake WS/gc (50 tests total).
- Media/file ingestion (jg-c7j): image/file/video messages (and mixed-message
  images) are downloaded + AES-256-CBC decrypted in the receive path (URLs
  expire in ~5 min), persisted to a durable per-conversation store
  (`<city>/.gc/wecom-media/inbound/...`), and surfaced as `file://`
  extmsg attachments plus a slack-style "[N WeCom file(s) attached] … Read
  that path to view it" block. Audio FILES (m4a/amr/mp3/…) get an inline
  ElevenLabs Scribe transcript (scribe_v1, diarize, auto language;
  `$ELEVENLABS_API_KEY` or `~/.config/elevenlabs/api-key`). Streaming
  200MB size cap (`WECOM_MEDIA_MAX_BYTES`) and 120s download timeout;
  every failure path (download, oversize, disk, transcription) downgrades
  to an in-message note — the message always delivers. Registration now
  advertises `SupportsAttachments: true`. Adapter test suite added
  (`pnpm test`, node --test, 30 tests incl. crypto fixtures).

- Initial skeleton (gp-86d / jg-1yx phase 1): long-connection adapter on
  Tencent's official `@wecom/aibot-node-sdk` — inbound text/voice/mixed →
  gc extmsg addressed to the mayor; outbound `/publish` (UDS proxy_process)
  → WeCom markdown with chunking; adapter self-registration; `gc wecom
  publish` operator verb. No public listener — the long connection is
  outbound-only.
