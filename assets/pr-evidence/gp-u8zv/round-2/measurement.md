# gp-t1qq / PR 69 round 2 measurement

Measured in the assigned lane at round base `052ec10e2efdcaa38fae88285b9354cf0fe07f32`.
The bead explicitly requests continuing branch `gp-u8zv`; pre_start created
`gp-t1qq` at the base instead. After confirming the lane boundary, sole session
ownership and that no worktree held `gp-u8zv`, switched to that existing branch
without force and restamped only claimed bead `gp-t1qq`.

## Existing contract and rendering

- The original PR delta against `ec42f23490a123903fd284eec6d08752d6b29a92`
  is three files, 80 insertions, one deletion. The round starts at `052ec10`.
- `gascity/template-fragments/gc-role-worker.template.md` lines 50–68 contain
  BOUNDED WAIT, with `mayor` hard-coded for the initial notification. Lines
  193–195 defer the bead's escalation contract until after the wait. There is
  no separate Failure section; Close defines the default failure metadata.
- The full `escalat|coordinator|Mayor` search in `gascity/`, `tests/` and `docs/`
  is saved in `recipient-search.txt`. No named escalation-recipient metadata key
  or structured body field exists. The worker reads its claimed bead's
  description/result contract and metadata from `gc bd show --json` before
  execution. Recipient resolution must therefore follow the contract's text,
  including a recipient it names in the description or metadata; it must not
  invent a new field.
- The Close PR-handoff paragraph already uses the bead/formula's named recipient.
  `gascity/tests/test_formula_assets.py` explicitly rejects a hard-coded mayor
  for that separate handoff. The mayor skill defaults workflow launch routing to
  `gc.run-operator`, which is not an escalation-recipient default.
- The only Gas City fragment is `gc-role-worker`; there is no
  `gc-role-mayor` template in this checkout. `gastown/agents/mayor/prompt.template.md`
  calls the Mayor the global coordinator. The core binary's own mayor prompt
  also names its persona mayor; neither supplies a worker escalation field.
- All 14 `gascity/roles/agents/*/prompt.template.md` files render the shared
  fragment via `{{ template "gc-role-worker" . }}`. The fragment itself uses
  `.AgentName` and `.TemplateName`. The local core source's
  `cmd/gc/prompt.go` defines `PromptContext` and `buildTemplateData`: no SDK
  mayor-name or escalation-recipient field exists; arbitrary Agent.Env data
  can be merged into the context, but this fragment consumes no such variable.
- Real `gc prime --strict` render coverage spans eight role/import combinations.
  The untouched baseline run passed 11 tests with 398 deselected.

## Upstream follow-up

Read-only GitHub API measurement pinned upstream main to
`0b0d96250f661a1e4f22bd9c89227dcbe58e4adc`. Its fragment is saved as
`upstream-worker.md`: it still uses `gc gc claim`, has no BOUNDED WAIT and no
`gc.work_outcome` Close bullet. Porting the wait to upstream remains a follow-up,
as the bead requires.

## Chosen behavior

Notify the contract's named recipient before waiting, with `mayor` only as the
fallback. A failed send gets one retry after five seconds. If both attempts
fail, preserve recipient and errors in the eventual close reason and continue
the same wait and lease polling. The existing 20-minute bound starts at the
first send attempt even if mail fails, so the retry cannot extend the window.
The two-minute cadence, ownership checks, ruling handling and expiry close
remain in place. Close performs only the contract's remaining acts and does
not send the initial notification again.
