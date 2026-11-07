# Round 2 self-review

## m4 — directory deadline

Reviewed the resolver's call and cache paths. The child context bounds both
request and response reads, preserves earlier caller cancellation, and records
the existing failure-cache entry without caching an empty successful address.
The stalled lookup test waits for the actual 30-second production deadline,
checks external classification, and checks the next delivery uses the failure
cache. `go test ./internal/principal -count=1 -timeout=2m` passed (30.270s).
