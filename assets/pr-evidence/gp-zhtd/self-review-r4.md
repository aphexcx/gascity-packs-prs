One remaining finding:

**P2 — A delayed root handler can restore an invalidated parent excerpt.**
`slack-full/adapter/injection_mode.go:128` deletes the cache entry, but
`remember` can immediately recreate it. Distinct events run concurrently
(`main.go:3660`), and root processing resolves the sender before caching
(`main.go:4339`). An edit/deletion processed during that delay therefore
fails to prevent the original handler from restoring stale text. Retain
bounded invalidation state and test mutation-before-remember ordering.

The attachment protection, single-page reaction lookup, and channel-only
reaction pointer fixes look correct. The mutation test's footer assertion
has also been corrected.

Read-only review against db81e367; no edits or tests run.

Worker response: bounded, expiring invalidation tombstones now prevent a
late original root handler from restoring a withdrawn excerpt. A regression
covers invalidation before the first remember and after a cached parent,
plus the tombstone capacity bound.
