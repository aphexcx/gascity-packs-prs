# gp-i9fr: PR 91 round 3 (bring current, bead gp-adh3)

Main moved under PR 91 when PR 92 merged. Each PR adds the first bullet
under Unreleased in the Google Chat changelog, so PR 91 no longer merged
onto main. This round merges main into the branch as one merge commit and
keeps both bullets. It also rewords round 1's copy of the codex record, so
the copy carries no detail of the gate tool's own setup and no local path.
No text, test or command changes.

- Started from: `06a00bdbda99cb3854562be41123a78ce1b73f46` (PR 91's head
  after round 2)
- Main merged: `e84b685a177b109877c4d47d36953e4ad85539ed` (the squash of
  PR 92)
- Merge base: `28918336555abbeb4f52af3c05029e801c59cb6f`
- Commits:
  - `e385cffb` the merge, first parent `06a00bdb`, second parent
    `e84b685a`. It is the code head: the commits after it touch only
    `assets/pr-evidence/gp-adh3/`.
  - `c1046555` round 1's codex record copy
  - this directory

## The merge

`git merge --no-ff origin/main` stopped at one file and one region:
`gchat/CHANGELOG.md:5`. Git merged `gchat/skills/google-chat/SKILL.md` by
itself. This matches the mayor's measurement with `git merge-tree`.

The changelog as resolved, from the heading Unreleased down to the first
line of PR 89's bullet:

```
## Unreleased

- The skill gives five reactions fixed meanings, with the same table and
  rules as the Slack fragment and the WhatsApp skill, in place of any emoji
  that fits (gp-adh3).

- Answer a message with `--to` on reply-current, publish or upload. The adapter
  takes the space and thread from the message and refuses conflicting ids.
  Reply-current no longer guesses a destination and requires `--to`; publish
  with a space starts a new topic. Results and identifier-only delivery logs
  show where each send landed. The ledger retains the answered message.

- Resolve reports whether a replay landed or is waiting and why. Each replay
```

This PR's three lines come first, byte for byte. Then one blank line. Then
main's six lines byte for byte: PR 92's bullet and the blank line after it.
Then comes the rest of the file as main has it. That is the file's own
order: the newest change is the first bullet under Unreleased, and each
merged change ends with one blank line. PR 91 merges after PR 92.

- Against main, the merged file adds only this PR's three lines and one
  blank line.
- Against the round's start, it adds only main's six lines and one blank
  line.
- No merge marker is left.

## The skill text

`gchat/skills/google-chat/SKILL.md` was merged by git, read whole, and not
edited.

- The table of five and its rules stand under "When to speak". The table
  starts at line 47.
- PR 92's reply lines stand under "How to reply", at line 79.
- Every line either side added is in the merged file: 32 from this PR and
  17 from main.
- The merged file differs from each parent by exactly the other side's
  change.

## Nothing else in the merge

With M the merge commit:

- `git diff --name-only 'M^1' M` lists 20 files.
- `git diff --name-only 'M^2' M` lists 34 files.
- Two files are on both lists: `gchat/CHANGELOG.md` and
  `gchat/skills/google-chat/SKILL.md`.
- `git show --remerge-diff M` shows only the changelog's resolution.

## Round 1's codex record copy

In `../codex-round1-6ca512e1.md`:

- Line 2, the route line, carried a detail of the gate tool's own setup. It
  now reads "the hub", as round 2's copy does.
- Line 3 carried a local path. The path is now "the lane worktree".

In `../round2/README.md`, two places described that line by its content:

- Line 248 now names it by kind: "the gate tool's route line".
- Lines 258 to 259 now say only that the line carried a detail of the gate
  tool's own setup.

No other line of either file changes. The old commit stays in the branch.

## The search

The search covered this PR's own evidence directory,
`assets/pr-evidence/gp-adh3/`, at the head. Main's evidence directories
arrive with the merge and are main's. It looked for a home path, a temp
path, and any sentence about the gate tool's setup, keys or tokens.

- No home path is left.
- The words key and token stand on ten lines in four files. Each line is
  about the Slack tokens the pack itself uses, or is round 2's list of what
  its check covered. That is how the pack works, so they stay.
- `../suite-make-test-7a7faffd.txt:45` carries the path of a temp clone. It
  names no user and is not a home path, so it stays.
- The test transcripts of this round write the clone's path as `<clone>`.

## Tests

At M, from a fresh clone, under `env -i HOME PATH`, as round 2 ran them.
Each transcript holds the commit, the command, pytest's output and the
exit code.

- `uv run --offline --with pytest python -m pytest -p no:cacheprovider tests/test_texting_rules.py slack-full/tests/test_slack_prompt_contract.py -q -rA`:
  **54 passed**, exit 0 (`green-at-e385cffb.txt`).
  `test_every_react_command_names_its_message` prints PASSED for gchat,
  slack and whatsapp.
- `make test PYTEST_DIRS=gchat/tests`: **48 passed, 89 subtests passed**,
  exit 0 (`suite-make-test-gchat-e385cffb.txt`).
- `make test PYTEST_DIRS=whatsapp/tests`: **57 passed**, exit 0
  (`suite-make-test-whatsapp-e385cffb.txt`).
- `make test`, run 1: **1 failed, 2747 passed, 126 skipped**, 10734
  subtests passed, exit 2 (`suite-make-test-e385cffb-run1.txt`).
- `make test`, run 2: **2748 passed, 126 skipped**, 10734 subtests
  passed, zero failures, exit 0 (`suite-make-test-e385cffb-run2.txt`).

Round 2 counted 2744 passed and 126 skipped. Main added tests since, so the
count grew.

### The one failure in run 1

`gascity/tests/test_toolchain_shims.py::PnpmShimTests::test_concurrent_callers_wait_for_one_install_instead_of_racing_it`
starts three callers of the pnpm wrapper at once against a fake install
that takes two seconds. It expects one caller to install and the other two
to print that they waited. In run 1, all three callers exited 0, there was
one install, and all three commands ran. Only the count of callers that
waited was 1, not 2. The third caller printed nothing: it started after the
install had finished, found the dependencies in sync, and had nothing to
wait for. That is a caller that started late on a loaded machine, not a
fault in the merge.

- Neither side of the merge changes anything under `gascity/` since the
  merge base.
- The test alone, ten times at M and ten at main, interleaved
  (`flake-ab-e385cffb-e84b685a.txt`): **10 of 10 passed at M; 9 of 10
  passed at main**. Main's one miss shows that the flake was there before
  this round.
- Each run of the test alone was: `uv run --offline --with pytest --with pyyaml --with jsonschema python3 -m pytest -p no:cacheprovider -q <the test id>`,
  from each fresh clone, under `env -i HOME PATH`.
- No text, test or command was changed. Run 2 is the whole suite again
  from the same clone.

In the transcripts, the clone's path is written `<clone>` and pytest's
random temp directory is written `<tmp>`. Trailing spaces are trimmed from
two lines of run 1's traceback, so `git diff --check` passes.

## Review

### Self-review

A fresh reviewer on the same model reads the round's whole delta against
the bead's contract and the public-repository rule. Its counts and each
finding's outcome are added after it reports.

No codex round ran in the lane. The mayor's gate reads the whole delta
against main.

## Public repository

Before the push I read the round's whole delta against PR 91's head after
round 2, and every commit message of the round, against the
public-repository rule. The delta is the merge's resolution, the evidence
lines and this directory. The merge brings in main's own files, which are
main's. The pattern covered home and temp paths, the customer names on the
rule's list, Slack, phone, user and message ids, email addresses, keys and
tokens, hosts and ports, and the gate tool's setup.

The result is added after the self-review.
