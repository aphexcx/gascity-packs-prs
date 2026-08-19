# Changelog — wecom pack

## 0.0.1 (unreleased)

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
