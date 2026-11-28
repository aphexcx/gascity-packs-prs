# STANDARD review, round 3

The Codex gate reviewed `ebc58be18c1665509d7fac4c5a5b2149ff336ee2..c8d2012330b5d4e472b87d74bd05197fc85afd84`. Verdict **BLOCK**.

The reviewer found one major case in the same attribution class. A legacy writer could persist a gate PASS and fail before appending its outcome. A later no-op introducing a PR could then claim the orphaned PASS.

New commitless records now use only review facts with their explicit PR scope. The base recorder wrote an empty commit field on legacy commitless outcomes. When rendering those outcomes, the reader can still show the historical verdicts, with their PR association identified as unknown. The regression test simulates a failed legacy append followed by a successful new no-op.
