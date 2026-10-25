Codex R1 reviewed 80c7277937b55a11e93515e8cc555a1815679a44 and returned BLOCK.
All four findings are addressed:

- Company mention-only file descriptions now use the protected files argument
  rather than the trimmable caption. An integration regression delivers an
  oversized caption with a file to an outside company session.
- Reaction lookup uses its original single-page request. The regression
  provides a target on page one and an unavailable second page, and asserts
  one request plus the retained target.
- Minimal parent excerpts are invalidated by observed edit/delete events.
  Subsequent replies retain the thread coordinate without the stale quote.
- Minimal reactions with no known root use a channel-only read pointer rather
  than a reaction event timestamp as a message timestamp.

Focused race regression run passed 19 top-level tests, 25 with subtests.
The first run failed only because the new cache test expected gc's reply-to
footer inside the adapter's raw Text; the assertion now checks the adapter's
actual thread anchor. Both run manifests are retained. An accidental initial
invocation from the repository root reported no matching tests; it is not
counted as adapter validation. Subsequent runs use the adapter module cwd.
