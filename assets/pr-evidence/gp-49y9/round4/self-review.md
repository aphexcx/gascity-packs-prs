# Round 4 DEEP self-review

This is one fresh same-model review of `52e91dbe5902fb98daeb8e3ff3150cb80b8d9527..8d0987d30d026edc170caf75e5206cb8e7b83899` for PR 107, bead `gp-49y9`. The reviewer inspected that committed delta and its surrounding call paths. The source lane remained read-only; additional tests use a separate archive of the reviewed commit and fakes. Mayor gate and second-model read verdicts remain pending.

Verdict: CLEAN for this round's reviewed code delta. No blocking or non-blocking product finding was reproduced. All prepared scratch checks held on their first Go execution. This is the worker's own-model review, not the mayor's gate or second-model read.

Break attempts and outcomes:

- R1, another member path: a three-member turn lost its ambient owner and left the enrolled message a non-owner of the promoted survivor; `no_route` and cold reconciliation preserved its outcome. HELD, executed in `TestRound4DeepNoticePromotionStillMember`.
- R1, another outcome path: an unrecognized delivery status produced the same uncertain terminal stop for the member and control owner. HELD, executed in `TestRound4DeepNoticeUnrecognizedOutcome`.
- R1, another engine observation: `Act` from the target session suppressed the member's notice, while an unrelated session did not; withdrawing every message and reconciling left the enrolled record removed. HELD, executed in `TestRound4DeepNoticeActTargets` and `TestRound4DeepNoticeAllMembersWithdrawn`.
- R1, ambient enrollment: every saved ambient notice record in the promotion, unknown-outcome, withdrawal and `Act` cases was absent or Removed. HELD, executed assertions in all four notice tests; no live ambient record appeared.
- R2, another position and combination: mixed Cc, Cf, Mn, variation selectors, Hangul filler and Braille blank at multiple marker gaps, after the colon and space, and within both header forms preserved the delivered bytes with the quote prefix. HELD, executed in `TestRound4DeepMixedDisplayCallers` through both merged and retried-singleton callers.
- R2, characters outside the current tables and list: a CoreText glyph-bounds probe held over U+0000 through U+FFFF in five actual local fonts. It found no additional empty glyph among present candidates outside Cc, Cf, Mn, Me, Z, the named blanks, unassigned code points and surrogate code points. This is a font-specific probe without shaping, not a Google Chat rendering claim or proof for supplementary planes.

The glyph probe used Python Unicode 17.0.0 and 20-point fonts. It ignored missing glyphs and required both reported ink width and height to be zero. Actual PostScript fonts and examined candidate-glyph counts were:

| Actual font | Present candidate glyphs | Additional empty glyphs |
| --- | ---: | ---: |
| Helvetica | 1,903 | 0 |
| Times-Roman | 1,234 | 0 |
| AppleSymbols | 2,794 | 0 |
| ArialUnicodeMS | 38,539 | 0 |
| AppleBraille | 255 | 0 |

The single scratch Go run used an archive of the reviewed SHA with only review test additions or modifications:

```text
env -u GC_BIN -u NEVER_SILENT go test ./internal/inbound -run '^TestRound4Deep' -count=1 -p=1 -timeout=10m -json
```

It exited 0 in 4.651 seconds: one package passed, five top-level tests passed, 138 subtests passed, no skips, no failed tests. The mixed-display test accounts for 136 subtests, including its two form groups; the Act test accounts for two. The completion manifest was read back. No failed or calibration Go runs preceded it, and there was no rerun. The font probe ran twice, both successfully with the same no-additional-candidate result; the second execution added actual font names and coverage counts to its output. All probe sources and raw results were retained outside the source lane.

Additional read-only checks held: all production owner Result/Stop/withdrawal observations use the fan-out helper; explicit deletion remains scoped to the deleted message; operator resolution persists on members before forgetting them; member reconciliation follows the ledger delivery and preserves a member's own retirement; Act remains conversation/session scoped; the comparison preserves original output bytes; both comparison callers retain their own first frame; Render, Strip, IsFrame and engine source are unchanged by this round. No claim is made for the explicitly excluded mid-line separator or look-alike cases.
