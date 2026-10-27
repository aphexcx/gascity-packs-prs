# gp-zbq3: readable shared URLs

The two reported live posts now pass through unchanged. The shared Python/Go
base64 rule exempts HTTPS x.com paths and queries and requires five distinct
characters outside an exception. No other redaction rule changed.

## Measurement

The installed helper snapshot is `7e3a7ea9044e70b7`: a content digest, not a
Git revision. Its full SHA-256 is
`7e3a7ea9044e70b722b4f9bf295e69739ff34b68e4fda5d75dfd245c8688a31a`.
Its three files match source commit
`8e6d75e51b5d9749e5a14d558bc19a466a289599` byte for byte (pin-provenance.log).

measure.py made read-only bot-token reads using the founder-gap-sweep channel
union and ten newest thread roots per channel, limited to seven days: 92 read
scopes. It selected the 50 newest founder posts retrieved, timestamp range
1792969017.427239–1793066920.465569. This is the sweep's watched scope, not an
exhaustive Slack search. baseline.json and after.json contain only IDs, hosts,
lengths, match offsets and counts; no message bodies, paths or query values.
Repeated URLs in Slack link labels count as separate occurrences.

| Host | URL occurrences | Redacted before | Redacted after |
| --- | ---: | ---: | ---: |
| instinct.com | 1 | 0 | 0 |
| hark.com | 1 | 0 | 0 |
| gist.github.com | 2 | 2 | 2 |
| angelmatch.io | 2 | 0 | 0 |
| x.com | 2 | 2 | 0 |

The two incident posts were replayed separately as well. Post
1792810032.204479 has two base64 matches at offsets 71–112 and 123–164
(41 characters, 25 distinct). Post 1793017482.720239 has two at 11–55 and
71–115 (44 characters, 21 distinct). Both entire posts are now unchanged.
Fixtures preserve their URL structure with synthetic usernames and status IDs.

Only x.com was added. Twitter, LinkedIn and github.com were absent from this
sample. The measured gist links also match the separate 32+ hex rule, which
is explicitly outside scope; a base64 exception would not make them readable.
The pre-existing canonical Slack permalink exception and its secret-query
negative remain unchanged. Ordinary Slack thread queries already pass.

## Diversity and secret protection

Five is the highest distinct-character floor that preserves every original
negative. Existing base64-only fixtures include a five-character alphabet in
hex-prefix-base64, a five-character JWT signature, and a five-character
prepended-token case; the AWS slash-key fixture has eight. Padding does not
count. Digit-only candidates are ignored by base64, while 32+ digit strings
still redact under the unchanged hex rule. This is a character-diversity
heuristic, not a Shannon-entropy or credential-validity check.

All 51 original shared fixtures are unchanged; 23 new fixtures cover both
incident shapes, URL query/host boundaries, nested and lookalike URLs, ASCII
whitespace parity, diversity/padding, JWTs inside exempt URLs, Slack tokens,
client-secret tokens, and a synthetic PEM body. Any other rule overlapping
an allowed URL invalidates its base64 exception. Generic base64-shaped values
in an exempt x.com URL are intentionally readable; recognized credential
shapes still redact. Idempotence remains checked for every fixture.

## Validation

checks.jsonl records command, working directory, exit code and log for each
verification. The expected pre-fix regression run failed eight new cases.

- From slack-full: `uv run --with pytest python -m pytest tests -q`:
  **878 passed, 2 skipped**, two existing os.fork deprecation warnings.
  Python 3.13.13; pytest was imported and its installed path verified first.
- From slack-full/adapter: `go test ./...`: **2 packages passed**.
  After the final explicit-whitespace parity adjustment,
  `go test -race -v ./redact`: **74 shared fixtures plus clipping passed**.
  Go 1.27.3 darwin/arm64.
- Live replay: **50 sampled posts plus both incident posts** read successfully;
  both incident posts unchanged and the host counts above confirmed.
- `git diff --check`: passed.
- `pnpm exec node --version`: **v24.21.0**. No Node tests apply and no
  unsupported-engine warnings occurred.

## Handoff

Draft PR against aphexcx/gascity-packs-prs main, the repository to which the
city's aphexcx/gascity-packs import resolves. Base is the pinned commit above.
The mayor owns the quiet-minute rollout after review and merge: update the
city's pack.toml pin, run gc import install and gc reload --soft, watch the
slack-full:binaries doctor row (pc_f5395a185018), and install/repoint the
separately pinned slack-tail helper snapshot. This worker changed no city
pin or running service.
