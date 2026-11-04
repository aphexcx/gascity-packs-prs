DEEP review of a security-critical component: whatsapp/gateway/ (everything else in the diff is unchanged). It is a Cloudflare Worker with D1, and it is the ONLY holder of the Meta WhatsApp Cloud API access token and app secret for one shared business number used by many customer "cities" (each city is one server box). A city must never be able to read, acknowledge, send to, mark read, fetch media of, or learn anything about another city's phones or messages.

The requirements of record are in the bead spec /Users/tailor512/city/assets/ops/beads/whatsapp-gateway-20261103.md (SCOPE item 3 = the frozen contract v1, item 4 = the required tests, ACCEPTANCE at the end). The 11/4 additions are three. (a) A delivery is HELD, body and all, until POST /v1/inbox/ack names its id, and every poll returns it again. (b) An ack of an id the cron already deleted is a no-op 200. (c) A repeated ack of an already-acknowledged id is the same no-op. The implementer's readings of what the contract leaves unstated are in whatsapp/gateway/README.md ("Readings of what the contract leaves unstated"); a reading that contradicts the contract is a finding.

Review adversarially for:
1. Signature verification over the RAW bytes, constant-time compares (HMAC, verify token, admin token), and credential lookup by sha256.
2. Routing isolation between cities on every route, including statuses (routed by the wamid of the city's own sends) and media (scoped by the media table).
3. Durability. Meta must get 200 only after the rows commit. A redelivery must never make a second delivery. A message must never be lost or misrouted, including under concurrent webhooks and partial batch commits.
4. The invite-code bind: races, a redelivery re-binding or re-welcoming, brute force, and a bound phone texting another city's code.
5. Send idempotency: concurrent requests with one key, a Worker death mid-call, a stale claim taken over, and a Graph failure.
6. The 24-hour window: the arithmetic, the 120 s margin, which events move it, a template never moving it, Meta's 131047.
7. Retention: the cron, body-less dedup rows, the 5,000 cap.
8. Logging: any path that could log a phone number, a message body, a profile name, a code, a token, a secret, or an error message that quotes input.
9. SQLite/D1 semantics: upserts with a WHERE, conditional INSERT ... SELECT, json_each, partial indexes, meta.changes in batch results, and the per-batch statement chunking.
10. Tests that would still pass if the behaviour they name were broken.

For each finding give severity (BLOCKER / MAJOR / MINOR / NIT), file:line, the defect, and a concrete failure scenario. Report only defects you can tie to the code; say CLEAN if you find none at MAJOR or above.
