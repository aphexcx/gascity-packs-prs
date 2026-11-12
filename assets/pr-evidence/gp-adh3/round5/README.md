# gp-fi5m: PR 91 round 5 (bring current, bead gp-adh3)

Main moved under PR 91 when PR 99 merged. Each PR adds the first bullet
under Changed in the Slack changelog, so PR 91 no longer merged onto main.
This round merges main into the branch as one merge commit and keeps both
bullets. No text, catalog, test or adapter changes.

- Started from: `6d036dab176c32de0b2600099d0f269eddf80fb6` (PR 91's head
  after round 4)
- Main merged: `52d25900455a8c734ba4b899224b4b39d6a50247` (the squash of
  PR 99; PR 93 and PR 94 also landed on main after round 3's merge)
- Merge base: `e84b685a177b109877c4d47d36953e4ad85539ed`
- The merge: `ec479906a31cfad202ee9e345f2c2053fed6b308`, first parent
  `6d036dab`, second parent `52d25900`. The commit after it adds only this
  file.

## The conflicted path

`git merge --no-ff --no-commit fork2/main` stopped at one file and one region:
`slack-full/CHANGELOG.md:40`, the first bullet under `### Changed`. The
base had nothing there. Kept: main's two lines first (its bullet and the
blank line after it), then this PR's eight lines, both byte for byte, then
the rest of the file as main has it:

```
### Changed

- Test fixtures now use invented names (gp-tu0b).

- **Reactions with fixed meanings** (gp-adh3): the `slack-v0` prompt
  [six more lines of this PR's bullet]
  (gp-bis1).
- **Mention-only room holds** (gp-qgl3): `bind-room --mentions-only` now
```

- Against main, the merged file adds only this PR's eight lines.
- Against round 4's head, it adds only main's two lines.
- No merge marker is left.

Both sides also touched the other two changelogs. Git merged them by itself
and they are not edited:

- `gchat/CHANGELOG.md`: this PR's bullet stays first under Unreleased, and
  main's bullet is the last one under it.
- `whatsapp/CHANGELOG.md`: main's two new Unreleased sections come first,
  and this PR's bullet stays in its own section.

## No content change

- Main changed none of the three texts, the three `reactions.json` files
  or the two test files. `git diff 6d036dab ec479906` over those eight
  files is empty (0 bytes):
  - `gchat/skills/google-chat/SKILL.md`
  - `slack-full/template-fragments/slack-v0.template.md`
  - `whatsapp/skills/whatsapp/SKILL.md`
  - `gchat/reactions.json`, `slack-full/reactions.json`,
    `whatsapp/reactions.json`
  - `tests/test_reaction_catalog.py`, `tests/test_texting_rules.py`
- With M the merge, `git diff --name-only 'M^1' M` lists 103 files, the
  same list as main's change since the merge base. `git diff --name-only
  'M^2' M` lists 61 files, the same list as this PR's change since the
  merge base. Only the three changelogs are on both lists.
- Outside `slack-full/CHANGELOG.md`, M's tree is the tree of
  `git merge-tree --write-tree 6d036dab 52d25900`.

## Tests at the merge

From a fresh clone at `ec479906`, under `env -i`:

- `tests/test_reaction_catalog.py tests/test_texting_rules.py`: 100
  passed, rc 0. With `slack-full/tests/test_slack_prompt_contract.py` added,
  as round 4 ran it: 102 passed, round 4's count.
- `make test` (its suites now include `whatsapp/tests` and
  `follow-through/tests`): 2873 passed, 131 skipped, 2 warnings (the fork
  DeprecationWarning in `slack-full/tests/test_slack_company_outbound.py`,
  outside this PR), 10734 subtests passed, rc 0.

## Review

None this round. The merge changes no text, catalog, test or adapter.
Every line of it is either git's own merge or a changelog bullet kept word
for word from one side.
