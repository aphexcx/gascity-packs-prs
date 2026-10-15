# gp-obim evidence

Fork base: d1348d0f2e3e504e1101515e2c409db6b7991ca3.
Reviewed and Go-tested source: 27e884fb993a2336cb9fdcb3177b9dee84707789.
Later evidence commits do not alter slack-full source.

- go-tests.txt: complete adapter suite, 2,149 passing cases (1,345 top-level), no failures/skips.
- go-race.txt: 129 focused passing cases with the race detector, no race reports. Both Go transcripts retain every result line and the exact invocation and Go version; application logs are omitted.
- python-tests.txt: complete Python suite at 771263ff, 655 passed, 2 skipped, 2 existing os.fork multithreading deprecation warnings. Scripts, Python tests, Makefile and adapter/run.sh are unchanged through the final source commit. GC_NO_INSTALL=1; GC_TEMPLATE unset; trailing TMPDIR slash normalized. No pnpm was used.
- go-tests-round1.txt and python-tests-initial.txt retain initial runs. The two initial Python failures were unchanged run.sh tests comparing T// paths to T/ paths; normalizing TMPDIR resolved them (pc_23466ec1b2cf).
- marker-before.txt is the pre-edit inventory, also copied verbatim into the PR body. marker-after.txt records zero sibling footer matches and zero footer-contract docs matches. The raw _ref pattern also matches existing turn_ref/body_ref protocol text; it is unrelated and preserved (pc_8018104dedca).
- posted-text-assertions.diff uses zero context and covers unchanged keyed text, ceiling, thread parent, exact timestamp, canonicalized text, Slack link/reference/email labels, original retry routing, and colon-adjacent emoji.

Review rounds are retained as review-roundN.txt (native STANDARD Codex review) and gate-roundN.txt (validated helper output). All reviews ran read-only. Read-only macOS git/cache and Go build restrictions prevented reviewer test execution; the worker ran the successful suites above outside that review sandbox.

| Round | Findings and resolution |
| --- | --- |
| 1 | Delayed acceptance ordering, redundant link labels, missing store directory status and orphan temp cleanup fixed by e8c1637b. |
| 2 | Native review found alias reload could change the retry target; b86cdf1f freezes the original target. Helper CLEAN. |
| 3 | Native reference/email label finding and helper original-routing bookkeeping finding fixed by 771263ff. |
| 4 | Native legacy www auto-link form fixed by 94c2f4ab; helper CLEAN. |
| 5 | Native colon-adjacent emoji finding fixed by 27e884fb with bounded matching at original literal boundaries; helper CLEAN. |
| 6 | Native STANDARD review: no actionable regressions. Validated helper: VERDICT: CLEAN. Both reviewed 27e884fb. |

The native invocation is codex -p city -m gpt-6-astra -a never -s read-only review --base origin/main -c 'sandbox_workspace_write.network_access=true', with GC_TEMPLATE unset and GC_NO_INSTALL=1. The helper uses its supported exec prompt-file mode pinned to immutable base/head: normal gc lane materialization leaves untracked paths, and the helper review mode rejects them (pc_2387046994d7). No lane contents were deleted to satisfy that guard.

The normative receipt contract lives in slack-full/adapter/publish_receipts.go and the new CHANGELOG entry. The historical marker entry is untouched. Receipts retain the flat publish-to-channel fields plus posted-text SHA-256, posted_at and readback. The store is evidence, not restart dedup; it never changes TTL. Raw text and caller keys are not persisted. A missing/expired receipt is unknown, never proof of non-delivery.

Retrieved-link forms were checked in official Slack documentation: https://docs.slack.dev/messaging/formatting-message-text/ and the legacy api.slack.com/docs/attachments example for www.example.com. Destination changes and literal code/URL characters remain mismatches. Existing emoji folding is deliberately lossy; the new fallback retains that class while preserving original non-emoji boundaries.

No Slack posts, manifest change, adapter deployment, merge, or ready-for-review transition were performed. The mayor owns the Fable read and merge/roll gates.
