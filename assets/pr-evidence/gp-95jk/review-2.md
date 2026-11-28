# STANDARD review, round 2

The Codex gate reviewed `ebc58be18c1665509d7fac4c5a5b2149ff336ee2..33df5ef933708f09effda09955316af0948e8365`. Verdict **BLOCK**.

The reviewer found one major issue with PR attribution. A review recorded alongside a commit without a PR could be displayed as a PASS for an unrelated PR introduced by a later no-op. Conversely, a no-op returning to a previously reviewed PR could lose its verdict after an intervening PR.

All new review facts now carry their PR scope, including an explicitly empty PR. For legacy facts, the commitless summary checks both the recorded PRs and the outcome history before attributing a verdict. The transition tests cover both spellings of the original record and verify the legacy case separately.
