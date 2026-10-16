Record or summarize a normal worker's typed close trail (Beads 1.3.1+).

Usage:
  gc <binding> provenance record BEAD --outcome shipped --commit FULL_SHA [--pr URL] [--reviews FILE] [--evidence-dir DIR]
  gc <binding> provenance record BEAD --outcome no-op|blocked|abandoned
  gc <binding> provenance summary BEAD

Use the city-level Gas City pack binding from gc import list (for example gascity).
Reviews are a JSON file containing an array of objects with stage (codex|gate|read), positive
round, verdict, full reviewed sha, and ref (immutable record path or URL).
For no reviews, supply a file containing []. BEAD must match its resolved ID exactly.
Record validates inputs, appends known facts, and writes compatibility keys,
clearing omitted commit/PR keys. GC_BIN overrides the default gc executable.
It does not close the bead, detach the work branch, or send a handoff.
Summary reads typed records only; missing facts and stale reviews are unknown.
See the pack README, Worker close provenance, for the schema and close recipe.
