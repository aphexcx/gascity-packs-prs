# Fresh same-model review, round 2 pass 1

Reviewer: gpt-6-astra subagent, read-only. Base: 986cbad3604f0be455b8c29677be3b6760cff4ae.
Final reviewer response:

CHANGES REQUIRED — two P2 findings in the round 2 diff against `986cbad3604f0be455b8c29677be3b6760cff4ae`:

1. **Larger pages exceed the unchanged response limit.** `thread_context.go:558` requests 500 messages, but line 627 reads at most 1 MiB before decoding JSON. A valid page containing 401 messages with 3,000-byte bodies exceeds that limit, causing decoding to fail and discarding the entire first page. Increase the bounded response allowance appropriately and test a page larger than 1 MiB.

2. **An oversized recovery permanently blocks later recoveries.** `injection_mode.go:170` restores the remaining queue and returns when its first candidate exceeds the budget. With the supported 700-byte budget and a failed 1,000-byte post, that entry cannot fit beside any future reminder; every smaller recovery behind it also remains blocked. Add budget-relative clipping or explicit handling for individually oversized entries, with a regression covering a large entry followed by a small one.

Otherwise, static inspection found consistent restoration of included recoveries on channel/batch transport failures, separation from ambient eviction, bounded pagination independent of retained-window size, and no watermark advancement for partial fetches. Existing golden fixtures are unchanged.

Read-only review only: no files modified and no builds or tests executed.
