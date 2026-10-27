Shared x.com status links currently lose their paths to the generic base64
redactor. This change preserves HTTPS x.com paths and queries in both Python
and Go, with exact-host and ASCII URL-boundary checks. JWTs and recognized
token shapes inside those URLs still redact. The PR stays draft for the
mayor's Codex/Fable gates and founder checkpoint.

The base64 rule requires five distinct characters, excluding padding, and
skips digit-only candidates. The measured URL false positives have 21 and 25
distinct characters, so a floor alone cannot fix them. Five is the highest
floor preserving the 51 original fixtures: at six, hex-prefix-base64 and
slack-permalink-prepended-base64 fail in both engines. This is a diversity
heuristic, not Shannon entropy. The independent hex rule still redacts long
numeric strings. Only the measured x.com host was added; the existing Slack
permalink exception and every other secret rule remain unchanged.

## Round 2

Appends to 6d977249977ddbdf4be5ec9c4c4631d13183552c on gp-zbq3; no history rewrite.
The mayor's gate and Fable findings are addressed as follows:

- Explicit ASCII scheme/host classes remove Go's long-s Unicode case-fold
  exemption. A positive RFC 3986 ASCII character class ends the span at
  non-ASCII, controls and the documented delimiters, including caret/backtick.
- The narrower leading class permits text start, ASCII whitespace or Slack
  `<` only. Nested URLs behind parentheses, brackets, quotes and `|` remain
  redacted. Neither Markdown parentheses nor a Slack label separator grants
  an exception. **Both incident Slack targets remain readable; their repeated
  URL labels redact.** The two incident fixture expectations and Markdown
  pair were revised to that binding requirement; the 51 originals are intact.
- Replay requires the exact ordered baseline `(channel, ts)` pairs and reports
  missing IDs before writing results. One-second read margins recover the two
  fleet replies omitted by the prior 10-microsecond windows. Extra roots and
  replies cannot replace a sampled post.
- The current sample contains eight URL occurrences: **four redacted before,
  three after in message context**, comprising two gist hex matches plus the
  repeated x.com label. The old isolated-URL metric remains four to two, now
  explicitly named in after.json. All 50 baseline identities and both separate
  incidents match; both actual incident targets are readable. Evidence stores
  IDs, hosts, lengths, offsets, counts and boolean results, never message bodies,
  paths, labels or query values. All Slack access was read-only.

There are 100 shared fixtures, including 26 new cases and three revised
round-1 expectations. New scheme/host/path Unicode, separators, controls,
nested/wrapped URLs and label cases are exercised through both engines.
Against round 1, Python fails 23 and Go 24 cases (the extra failure is long-s).
At this head all 100 outputs are byte-identical and idempotent. The 29-row
new/revised parity table includes each engine's complete synthetic output.

Validation at the round-2 implementation:

- From slack-full: `uv run --offline --no-project --with pytest python -m pytest
  tests -q -p no:cacheprovider`: **904 passed, 2 skipped**, two existing fork warnings.
- From slack-full/adapter: `go test -count=1 -json ./...` and
  `go test -race -count=1 -json ./...`: **2 packages, 1,404 top-level tests /
  12,786 including subtests passed**, one opt-in skip in each run.
- From slack-full/cli: `go test -race -count=1 -json ./...`: **8 packages,
  199 top-level tests / 237 including subtests passed**, no skips.
- Python 3.13.13; Go 1.27.3 darwin/arm64; `pnpm exec node --version` v24.21.0.
  No Node tests apply and no unsupported-engine warning appeared.

Evidence: `assets/pr-evidence/gp-zbq3/round-2/README.md`, checks.jsonl,
regression-summary.json, parity.json, suite summaries and live-replay-grouped.log;
the corrected replay is `assets/pr-evidence/gp-zbq3/after.json`.

The nine Fable notes are answered explicitly:

1. Round-2 commits carry the required trailers. The eventual squash takes this
   PR body and its final trailer block; round 1's commit is preserved.
2. Corrected eight to nine original-base failures, reproduced through both engines.
3. Kept the digit-only exclusion as explicit generic-rule policy. It changes no
   output while the independent hex rule exists; that limitation is documented.
4. Expanded Go's byte-indexed set from 128 to 256 entries.
5. No change: generic base64 inside an eligible x.com path/query is intentionally
   readable; recognized credentials still disable the exception.
6. No change: HTTP, www/mobile variants, other hosts and unsupported leading
   punctuation remain outside the measured allowlist.
7. Corrected the floor-pinning claim: the two base64 fixtures pin five, while
   JWT signatures still redact independently at six.
8. No change: another rule overlapping any part of the URL invalidates its
   entire exception, preserving the existing containment/idempotence contract.
9. No change: metadata-only evidence and gc Slack reads satisfy the data boundary.

The gpt-6-astra immutable-delta review is CLEAN for 6d977249..648f887e
(self-review.out). Its minor evidence finding is fixed: identical URL strings
now have grouped occurrence/redaction counts, avoiding attribution to the wrong
duplicate's position. Both duplicate orderings are tested and the final replay
retains the same identities and 4-to-3 contextual total. A follow-up review
covers that evidence-only adjustment; production redaction code and fixtures
remain exactly as tested at 648f887e.
The mayor owns the fresh cross-model gate, founder checkpoint, merge and later
city pin roll plus separate tail-helper snapshot installation. No readiness
transition, city configuration change or deployment was performed.

Papercuts addressed: pc_192a75a6463d, pc_4fe82b567b32.

Bead: gp-zbq3
Codex-Reviewed-By: OpenAI Codex gpt-6-astra (worker self-review; immutable-delta gate recorded separately)
Co-Authored-By: OpenAI Codex <noreply@openai.com>
