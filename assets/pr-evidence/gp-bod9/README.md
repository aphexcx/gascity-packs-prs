# WhatsApp reactions and quoted replies

Base: `205d0aecedf944792f1e3b3fbc420b9e5ad910d8`. Its WhatsApp sources and shared texting-rules test are identical to the specification's earlier base; no referenced source lines moved before this work.

Part A adds attributed hand reactions, removal, retry recovery, transcript recording and the skill instructions. Part B quotes the inbound message on a reply's first chunk or file, reports when the quote was not applied, and adds gateway ownership checks and safe plain-send fallback. The adapter and gateway may ship in either order. Gateway deployment is separate and requires migration 0005 first. Nothing automatic is enabled; a box needs its pack updated and adapter restarted.

PART C: STOPPED, waiting on PR 104. No acknowledgement module or model route was copied or added.

## Acceptance

| Item | Result |
| --- | --- |
| A1 | DONE: react command, required flags, session attribution, receipt/error exits. |
| A2 | DONE: listener route, validation, recipient resolution, shared owner queue, reaction key namespace. |
| A3 | DONE: placement/removal, window and Meta refusals, restart recovery and conflict handling. |
| A4 | DONE: send ledger, never-silent activity, one CLI transcript and redacted logs. |
| A5 | DONE: skill, prompt fragment, README, changelog and only the specified shared-test changes. |
| B1 | DONE: both reply-to flag forms, text/file/update bodies, empty and missing values. |
| B2 | DONE: invalid-id plain send, first chunk only, file context, first-call quote retained on retry. |
| B3 | DONE: string receipt fields, all quote outcomes, stderr notices without a failed send. |
| B4 | DONE: reply template uses the inbound id; fake gc renders the registered template; synthetic targets remain deliverable. |
| B5 | DONE: additive context schema and documented contract; both required contract comments accepted before gateway code. |
| B6 | DONE: retained inbound/joined/own-send ids recognized; foreign, expired and unknown targets sent plain. |
| B7 | DONE: exact Graph context shape, one fallback only on definite refusal, uncertainty preserved. |
| B8 | DONE: message-id-only response body, quote headers and unknown replay result, unchanged no-context responses. |
| B9 | DONE: unknown reaction targets refused with 422 without consuming a key; adapter gives a permanent receipt. |
| C0 | DONE: dependency gate observed. |
| C1 | STOPPED: waiting on PR 104. |
| C2 | STOPPED: waiting on PR 104. |
| C3 | STOPPED: no chooser authorization and PR 104 absent. |
| C4 | STOPPED: waiting on PR 104. |

## Verification

Runtime: Node `v24.21.0` from `pnpm exec node --version`. The make runner selected Python `3.13.13` through uv; the shell's Python `3.15.0` was not the pytest runtime. No unsupported-engine warning occurred.

Final code verification used `b981c9803b990cc505da73ebaae89b2e87366a3b`. [verification.json](verification.json) records each command, its exit status, and matching clean tracked state at its starting and ending SHA. Later commits contain evidence only.

| Location | Invocation | Result |
| --- | --- | --- |
| adapter | `pnpm test` | 417 tests: 416 passed, one existing timing timeout; that case passed in isolation below. |
| adapter/test | `pnpm exec node --test react.test.js` | 33 passed: 27 original cases and six review regressions; each failed before its implementation or correction. |
| adapter/test | `pnpm exec node --test quote.test.js` | 22 passed; 21 failed before implementation and one no-quote pin already passed. |
| gateway | `pnpm exec vitest run test -t 'quoted replies\|quote wire'` | 25 passed; 18 failed before implementation and seven compatibility pins passed. |
| gateway | `pnpm test` | 333 passed across six files. |
| gateway | `pnpm typecheck` | exit 0. |
| gateway | `pnpm build` | exit 0; dry-run only. |
| repository root | scoped `make test`, with the inherited make/test overrides cleared as below | 521 passed, 127 skipped, 161 subtests passed. |
| commands | `sh -n react.sh publish.sh` and `shellcheck -x react.sh publish.sh` | exit 0. |

The required scoped make command exposed an existing nested-make test issue: command-line `PYTEST_DIRS` is exported into pytest and then into the test's `make -n`, which expects the default directory list. No repository test or Makefile was changed for this. The passing invocation preserves the full requested scope:

```sh
make test PYTEST_DIRS='tests whatsapp/tests' \
  PYTEST_RUNNER="echo 'env -u MAKEFLAGS -u MAKEOVERRIDES -u MFLAGS -u PYTEST_DIRS -u PYTEST_ARGS -u PYTEST_RUNNER uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest'"
```

The initial full adapter run passed every tracked test but also ran a preexisting untracked config test (eight failures). That file was preserved separately and excluded from delivery. A later run under concurrent test load hit five existing timing-sensitive cases. All five passed in isolation, with unchanged code, deadlines and assertions:

- audio survives SIGKILL at after-media-send with one upload and one send;
- held: wake, reconnect and safety reads of a held row cause one post;
- never-silent integration: captioned reply activity survives primary receipt SIGKILL without replay;
- never-silent integration: terminal whole-paste pending remains delivered across restart;
- M2: an inbox page computed before the ack never brings an acknowledged delivery back to the no-entry branch.

The isolated command ran from `whatsapp/adapter/test`:

```sh
pnpm exec node --test --test-concurrency=1 --test-name-pattern \
  'audio survives SIGKILL at after-media-send|held: wake, reconnect|never-silent integration: (captioned reply|terminal whole-paste)|M2: an inbox page'
```

It reported 24 passing runner entries: the five selected cases and 19 empty file wrappers. An intermediate rerun also hit `held: a wake during an in-flight read queues exactly one further read`.

The final stable-head adapter run passed 416 of 417 tests; only the existing M2 inbox-compaction case timed out waiting for a later poll. Both that case and the intermediate held-wake case passed unchanged at the same final code SHA, using:

```sh
pnpm exec node --test --test-concurrency=1 --test-name-pattern \
  'M2: an inbox page|held: a wake during an in-flight read'
```

This isolated run reported 23 passing runner entries: two selected cases and 21 empty file wrappers. The full command's exit status remains 1 in the evidence; it is not reported as an unqualified green run. Adapter tests need no installed dependencies despite pnpm's missing-node_modules warning. Gateway fixture tests emit existing binary-body text warnings.

## Existing tests changed

- `test_command_entries_resolve_to_nested_words`: add the react verb to discovery.
- `test_whatsapp_says_what_a_thanks_gets_until_the_mayor_can_react` became `test_whatsapp_explains_message_ids_removal_and_the_window`: replace the obsolete cannot-react premise with id/removal/window instructions.
- `test_a_reaction_that_is_not_placed_is_said_in_words`: add the WhatsApp failure sentence.
- `test_whatsapp_skill_says_the_mayor_cannot_react_there_yet` became `test_whatsapp_skill_explains_the_react_command`: pin the new command and retain the adapter sentence.
- Shared `REACT_COMMANDS` and `OUTSIDE_THE_TABLES` data: add WhatsApp target flags and the single new reaction example, respectively. No other shared test changed.
- Adapter registration test: expect the reply-to placeholder.
- Gateway BSUID send test and window-clock send test: replace nonexistent reaction targets with each fixture's existing joined-message id; preserve every assertion.

## Evidence and limits

- [Test-first cases](tests.md) records each new failing case and compatibility pin.
- [Operator-facing changes](operator-surfaces.md) records old/new instructions, receipt fields, hints and documentation.
- [Self-review outcomes](review.md) records the DEEP review, all finding dispositions and the explicit rate-limit ruling. HTTP 400/code 131056 remains a follow-up gap under that ruling.
- The [Quoted replies contract](../../../whatsapp/gateway/README.md#quoted-replies) was posted unchanged to both required contract records before gateway implementation; both comments were accepted.
- [Meta sources](../../../whatsapp/gateway/test/fixtures/SOURCES.md#quoted-replies-and-reactions-read-2026-11-13) names both URLs and the read date. Deleted/archived targets may have no quote bubble; invalid reaction targets may fail later with 131009. The pages do not establish replacement by a second reaction or empty-emoji removal.
- Tests prove local request/receipt shapes, retention checks and retry behavior. The documentation read establishes the published wire shape and listed limits. No real Meta call, live gateway/adapter call, real WhatsApp number or model call was made. No deployment or migration was run. Real delivery, removal and visual quote rendering remain unverified.
