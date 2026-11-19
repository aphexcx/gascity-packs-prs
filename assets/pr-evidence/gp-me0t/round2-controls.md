# Verification controls

| Rule | Before | After |
| --- | --- | --- |
| R2-1. First failure | A fixture Go returned 17 for vet and 0 for test. The previous two-command root block returned 0. | Every published check returned 17 when its first operation failed. Actual Make recipes ran with local recording tools. |
| R2-2. Suite coverage | The previous skill omitted Python and JavaScript suites and provided only vet for Google Chat. | The tracked test inventory covers 70 directories. Node adapter file patterns have positive and negative controls. See [directory coverage](round2-directories.md). |
| R2-3. No installation | Make started with the previous wrapper when the Python import probe failed. | Missing Node, Bash, jq, ripgrep, GNU sed, mktemp, date or any of the three Python imports stops the relevant check. No fetching tool was invoked by the refusal fixtures. |
| R2-4. Commands and environment | The original tests accepted 31 of the 53 mutated inputs. | All 53 mutations were rejected. The ordinary and offline gateway children exposed exactly their allowlisted environment. A fake make target containing a literal hash and a root script invoked in the wrong directory were rejected. |
| R2-5. Credential and checkout guards | The old wrapper admitted a local gateway environment file. The first blanket PEM guard rejected a public certificate and overlooked a FIFO. | The current checks cover worktree credential filenames, private-key markers near the 64 KiB boundary, read failures, symlinks and FIFOs. A public certificate passes. Poisoned Git selectors and CDPATH cannot select a second checkout. |
| R2-6. Results and routing | The previous commands omitted wall times and the isolated archive control. | Durations appear beside the checks, CI-only and excluded suites have reasons, and the evidence retains the original failures and their isolated trials. |

The class suite passed **48 tests** (551 deselected). The original skill failed the new checks. The PEM-specific red control had two failures and four passes before the content guard was added. The absent-target controls check that the assertion names the incorrect target. The final case checks the exact tracked filenames passed to the WhatsApp Node runner with an untracked file present. All test fixtures for the published contract live in the committed pytest.

The 53 mutation cases use the first review's numbering. The unchanged input passed. Twenty-five mutations applied directly to their original anchors. Twenty-eight used the corresponding current anchor because this round removed the combined Make and local Slack full test commands and changed the guard implementation. Cases 34–40 exercised the mutated function with fixture tools. Invalid operational examples were checked as text and never executed.

- Direct mutations rejected: 4, 6, 9, 14, 15, 27–30, 34, 36, 38–51.
- Equivalent mutations rejected: 1–3, 5, 7–8, 10–13, 16–26, 31–33, 35, 37, 52–53.
