# Test fixtures now use invented names

Base: `c546802da5d42fb5d073b7d0bab6716b2e48b6f5`.
Head results cover the test sources delivered with this record.

| Suite | Base | Head |
| --- | --- | --- |
| WhatsApp gateway | 273 passed in 5 files | 273 passed in 5 files |
| Slack Python | 975 passed, 3 skipped, 10 subtests passed | 975 passed, 3 skipped, 10 subtests passed |

Commands, run from each module directory:

```sh
# whatsapp/gateway; pnpm exec node --version: v24.21.0
env -u GC_TEMPLATE pnpm test

# slack-full; Python 3.13.13
env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python -m pytest tests -q
```

Both runs had the same existing warnings: the gateway's binary-body text
conversion warnings and two Python fork deprecation warnings. Neither gateway
run reported an unsupported engine or a localstorage-file warning.

A case-insensitive scan covered all tracked files, including the decompressed
contents of all 148 gzip files. The replacement values were unused at the base.
All in-scope occurrences were updated; the owner onboarding page is unchanged
under the bead's explicit exclusion. Test assertions retain their meaning.

Files changed:

- `assets/pr-evidence/gp-6mr7/portuguese-read.md`
- `assets/pr-evidence/gp-tu0b/README.md`
- `slack-full/CHANGELOG.md`
- `slack-full/tests/test_slack_chat_read.py`
- `whatsapp/CHANGELOG.md`
- `whatsapp/gateway/test/gateway.test.ts`
