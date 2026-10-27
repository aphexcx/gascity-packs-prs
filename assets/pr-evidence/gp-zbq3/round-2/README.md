# gp-zbq3 round 2

This round appends to `6d977249977ddbdf4be5ec9c4c4631d13183552c` on the same
`gp-zbq3` branch for draft PR 66. The two mayor reviews were read in full.

## Boundary and parity fixes

The x.com allowlist entry spells scheme/host case using ASCII classes, so Go
cannot accept the Unicode long-s that Python rejects. Its positive path/query
class contains ASCII RFC 3986 characters minus the existing delimiters. It
also excludes caret and backtick, which are not RFC 3986 URL characters.
Every non-ASCII character and control ends the span.

The narrower start class permits text start, ASCII whitespace, and Slack's
`<` bracket only. Parentheses, brackets, quotes and `|` no longer introduce an
exception. This closes nested URLs behind all of those characters. It also
means Markdown-parenthesized URLs and directly repeated Slack label URLs can
redact. The actual `<https://x.com/...|label>` target still works. Three round-1
expectations were updated to that binding contract: both incident shapes and
the Markdown pair. The 51 pre-bead fixtures remain unchanged.

There are 26 new cases, including Unicode in the scheme, host and path, NBSP,
U+3000, U+2028, U+2029, SOH, NUL, DEL, nested and standalone wrappers, and label
versus target boundaries. `parity.json` records both complete synthetic outputs
for every new/revised case: 29 rows, byte-identical. The harness additionally
checks all 100 fixtures through both engines and their idempotence.

`prove.py` copies production files into temporary directories, without making
a Git worktree. Against round 1, the new expectations fail 23 Python cases and
24 Go cases; long-s accounts for the difference. `regression-summary.json`
records their names. Against the original base, all 74 round-1 expectations
reproduce nine failures in each engine, correcting the stale eight-case claim.
The current 100 cases have zero failures and zero parity differences.

## Replay and measurement

`measure.py --replay` selects the exact ordered `(channel, ts)` pairs from
baseline.json, including the separate incidents, and rejects missing pairs
before writing after.json. Extra thread roots cannot substitute for a post.
The first run failed as intended on the two missing fleet replies; its record
is `live-replay.log`. One-second timestamp margins recovered both replies
where the original 10-microsecond windows omitted them. Decimal arithmetic
keeps the timestamps exact, and identity filtering makes the wider read safe.

The final read-only replay is `live-replay-grouped.log` and `../after.json`:
all 50 baseline identities and both incident identities match, with none
missing. Both incident link targets are readable; their repeated URL labels
are redacted. No Slack post or other Slack mutation was made. Records contain
IDs, hosts, offsets, lengths, counts and boolean results, never message text,
link paths, labels or query values.

The 50-post sample has eight URL occurrences. In message context, four redact
before and three after: the two gist occurrences still match hex, and the
repeated x.com label now redacts as required. Isolated URL strings still show
four before and two after; `working_standalone_redacted` records that old
metric explicitly. `slack_links` records target readability separately from
label changes. Identical URLs are grouped with occurrence/redaction counts;
the evidence does not attribute survival to a particular duplicate's position.
Synthetic replay checks cover exact identities, missing-ID errors, extra roots,
and both label-first and target-first duplicate orderings.

## Review notes resolved

1. Round-2 commits carry Bead, Codex-Reviewed-By and Co-Authored-By
   trailers. The eventual squash takes the updated PR body and its last block.
2. Nine failures on the original base are reproduced, replacing the stale eight.
3. Keep the explicit digit-only exclusion to express the generic rule's policy;
   it changes no output while the independent hex rule applies. Docs say so.
4. Go's byte-indexed diversity set now has 256 entries.
5. No change: generic base64 values inside an eligible x.com path/query are
   intentionally readable. Recognized credential rules still block exemption.
6. No change: host/scheme variants and other hosts stay outside the measured
   allowlist. No twitter.com, linkedin.com or github.com entry was added.
7. The ceiling is pinned by hex-prefix-base64 and
   slack-permalink-prepended-base64, both failing at six in both engines.
   JWT signatures do not pin the floor; their independent rule still redacts.
8. No change: a recognized-secret overlap invalidates the whole URL exception,
   retaining the original containment and idempotence behavior.
9. No change: evidence retains only the authorized metadata, with Slack reads
   through `gc slack read --no-names --json`.

The floor is five distinct non-padding characters: the observed false positives
had 21 and 25, so diversity alone cannot fix them. Five is the highest floor
that preserves the original secret fixture set; the harness proves the two
failures at six. This is a diversity heuristic, not an entropy calculation.

## Verification

`checks.jsonl` is the completion manifest. Go JSON logs are reduced by
`summarize-go.py` to counts and original package completion events, retaining
the raw SHA-256 and byte count. Python logs retain their suite summaries.

- Python from slack-full: `uv run --offline --no-project --with pytest python
  -m pytest tests -q -p no:cacheprovider`: 904 passed, 2 skipped, the same two
  fork deprecation warnings. Python 3.13.13 and the imported pytest path are
  recorded in python-version.log.
- Adapter from slack-full/adapter: `go test -count=1 -json ./...`: 2 packages,
  1,404 top-level tests / 12,786 including subtests passed; one opt-in skip.
  `go test -race -count=1 -json ./...` also passed with the same counts.
- CLI from slack-full/cli: `go test -race -count=1 -json ./...`: 8 packages,
  199 top-level tests / 237 including subtests passed, no skips.
- Go 1.27.3 darwin/arm64. `pnpm exec node --version`: v24.21.0. No Node tests
  apply; no unsupported-engine warning appeared.
  The standalone evidence/parity scripts use Python 3.15.0; the full pytest
  suite above runs on Python 3.13.13 through uv.

The gpt-6-astra helper review in self-review.out is CLEAN for the round delta
6d977249..648f887e (no critical/major findings). Its minor noted ambiguous
per-occurrence duplicate URL flags. The grouped-count representation above
resolves that issue, tested with the reviewer's label-first example and replayed
read-only. The follow-up gpt-6-astra review, self-review-followup.out, is CLEAN
for 648f887e..c612d94c with no findings at any severity. Together the two records
cover all round-2 implementation changes. Production redaction code and fixtures
are unchanged from the tested 648f887e; the final commit records review results
and documentation only. Both helper .rc files contain 0 and both .reason files
validate an anchored CLEAN verdict. The reviews ran read-only in this lane
through the installed helper's exec mode, without creating another worktree.

## Handoff

The PR stays draft. The mayor owns the fresh Codex/Fable delta reviews, founder
checkpoint, merge and later city pin/helper rollout. No city configuration,
installed helper, running service or PR readiness state was changed here.
