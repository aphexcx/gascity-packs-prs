# Changelog — wecom pack

## 0.0.1 (unreleased)

- Initial skeleton (gp-86d / jg-1yx phase 1): long-connection adapter on
  Tencent's official `@wecom/aibot-node-sdk` — inbound text/voice/mixed →
  gc extmsg addressed to the mayor; outbound `/publish` (UDS proxy_process)
  → WeCom markdown with chunking; adapter self-registration; `gc wecom
  publish` operator verb. No public listener — the long connection is
  outbound-only.
