# codex-gate review: base 148e4a0f9d39a9c83b7d9dcaea151b6d884a8ab0 (merge base of --base origin/main = 148e4a0f9d39a9c83b7d9dcaea151b6d884a8ab0 and the head) head 2d6715dd1e38457e1e4e31556741fe0dc9a704df; 4 file(s); class QUICK; model gpt-6.2-astra; 2026-11-18T19:14:54Z
# codex route: the hub (CODEX_HOME <hub home>; the hub rotates its Codex seats)
# reviewed in a pristine detached worktree at the head (--head 2d6715dd; the working tree of <lane worktree> not consulted)
# verdict rule: CLEAN with no CRITICAL or MAJOR finding (MINORs are reported, deferred by the lane, never blocking)
#  gascity/README.md                                        |  5 ++-
#  gascity/template-fragments/gc-standing-notes.template.md | 28 +++++++++++++++++
#  tests/test_gc_role_prompt_integration.py                 | 47 +++++++++++++++++++++++++++++
#  tests/test_gc_standing_notes_fragment.py                 | 92 +++++++++++++++++++++++++++++++++++++++++++++++++++-----
#  4 files changed, 164 insertions(+), 8 deletions(-)

1. **MAJOR · NEW — The testing line can publish credentials.** **CLASS** Verbatim command disclosure on PR creation and body updates. **SEQUENCE** Use the documented Slack `auth.test` command with a sentinel bearer token, then write the `Testing:` line as instructed. The token appears in the PR body. Require workers to replace secret argument and environment values with placeholders while preserving the command, counts, and exit code. [gc-standing-notes.template.md:45](gascity/template-fragments/gc-standing-notes.template.md#L45) · [Slack command:154](slack-full/adapter/SETUP.md#L154)

2. **MAJOR · NEW — The revert instruction asks for a commit before it exists.** **CLASS** Both initial PR bodies and pre-merge updates depend on the future merge or squash commit. **SEQUENCE** Open a two-commit PR, write its body, then squash-merge it after review. At body-writing time the final SHA is unavailable, and reverting the recorded branch tip would undo only the second commit. Specify a command that resolves the merged commit from the PR at rollback time. For a two-parent merge, the command also needs the base parent as mainline. The static test currently pins the faulty wording. [gc-standing-notes.template.md:55](gascity/template-fragments/gc-standing-notes.template.md#L55) · [test_gc_standing_notes_fragment.py:161](tests/test_gc_standing_notes_fragment.py#L161)

3. **MINOR — The risk rule leaves an unmatched path undefined.** If a city tiers only `deploy/**` and a PR changes only `README.md`, the list exists, yet no touched path has a tier. Extend the fallback to cover zero matches and test that case. [gc-standing-notes.template.md:51](gascity/template-fragments/gc-standing-notes.template.md#L51)

4. **MINOR — Hidden instructions can pass the content tests.** Move the `none: <why>` clause into a Go template comment. The static check still finds it in the source, and the render check still finds all three labels, although workers lose the instruction. Extract each note from `renderable(fragment())` before checking its clauses. [test_gc_standing_notes_fragment.py:51](tests/test_gc_standing_notes_fragment.py#L51)

Tests not run (sandbox). No test evidence for this delta under `assets/pr-evidence/` at the specified head.

VERDICT: BLOCK
