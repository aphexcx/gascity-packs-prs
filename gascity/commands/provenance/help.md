Record or summarize a normal worker's typed close trail (Beads 1.3.1+).

Usage:
  gc <binding> provenance record BEAD --outcome shipped --commit FULL_SHA [--pr URL] [--reviews FILE] [--evidence-dir DIR]
  gc <binding> provenance record BEAD --outcome shipped --artifact PATH --sha256 HEX [--pr URL] [--reviews FILE] [--evidence-dir DIR]
  gc <binding> provenance record BEAD --outcome ran|no-op|blocked|abandoned [--pr URL] [--reviews FILE] [--evidence-dir DIR]
  gc <binding> provenance summary BEAD

Use the binding the worker close discovery selects (README, Worker close provenance).
Optional --city PATH / --city=PATH and --rig NAME / --rig=NAME go before
record or summary and are preserved for every nested gc bd call.
An explicit --city alone selects that city's own store, as gc --city PATH bd
does, disabling rig auto-detection by bead prefix, GC_RIG, and working directory.
A rig bead also needs --rig NAME; for example, for a bead in the repo rig:
  gc <binding> provenance --city /path/to/city --rig repo summary BEAD
Without explicit flags, the dispatcher's city environment and normal rig routing apply.
A shipped artifact requires an absolute path to a regular file and the SHA-256
of its contents in 64 lowercase hex digits. For a staged directory, supply its
manifest file. Files inside a Git work tree must be ignored to use this form.
The recorder stores the verified path in gc.work_commit. Use ran for a
completed run with no deliverable.
Reviews are a JSON file containing an array of objects with stage (codex|gate|read), positive
round, verdict, full reviewed sha, and ref (immutable record path or URL).
For no reviews, supply a file containing []. BEAD must match its resolved ID exactly.
Record validates inputs, appends known facts, and writes compatibility keys,
clearing omitted commit/PR keys. GC_BIN overrides the default gc executable.
It does not close the bead, detach the work branch, or send a handoff.
Beads 1.3.1 retains provenance only for rows of the issues table, not wisps or
no-history beads. Record checks JSON acknowledgements and re-reads every intended
fact before writing compatibility keys. Missing records fail with "store did not
retain provenance for BEAD" and explain the constraint: close such work by its
workflow-control contract or promote it first. The close recipe stops before
metadata updates, branch release, and close.
Summary reads typed records only; missing facts and stale reviews are unknown.
For a read ticket with no commit, the summary names the reviewed SHA and the
latest gate and read verdicts. Multiple reviewed SHAs are reported as conflicting.
Older commitless records show their historical verdicts with the PR association
marked unknown. A new record uses reviews explicitly scoped to its PR.
See the pack README, Worker close provenance, for the schema and close recipe.
