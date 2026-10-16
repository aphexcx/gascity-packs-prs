Record or summarize a normal worker's typed close trail (Beads 1.3.1+).

Usage:
  gc <alias> provenance record BEAD --outcome shipped --commit FULL_SHA [--pr URL] [--reviews FILE] [--evidence-dir DIR]
  gc <alias> provenance record BEAD --outcome no-op|blocked|abandoned
  gc <alias> provenance summary BEAD

Use the city-level Gas City pack import alias (for example gc or gascity).
Reviews are a JSON array of objects with stage (codex|gate|read), positive
round, verdict, full reviewed sha, and ref (immutable record path or URL).
Record validates inputs, appends known facts, and writes compatibility keys.
It does not close the bead, detach the work branch, or send a handoff.
Summary reads typed records only; missing facts and stale reviews are unknown.
See the pack README, Worker close provenance, for the schema and close recipe.
