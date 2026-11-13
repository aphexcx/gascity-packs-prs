# Test-first evidence

All network dependencies were local fakes. Part A tests were committed in `518635ae`; Part B contract/tests in `baa6bb6b`. Base: `205d0aecedf944792f1e3b3fbc420b9e5ad910d8`.

## Reactions

From `whatsapp/adapter/test`: `pnpm exec node --test react.test.js`.

| Test | Before implementation | After implementation |
| --- | --- | --- |
| reaction placement: wire, receipt, ledger and redacted logs | FAIL | PASS |
| reaction removal: wire, receipt, ledger and redacted logs | FAIL | PASS |
| reaction rejects session before sending | FAIL | PASS |
| reaction rejects conversation before sending | FAIL | PASS |
| reaction rejects id-0 before sending | FAIL | PASS |
| reaction rejects id-1 before sending | FAIL | PASS |
| reaction rejects id-2 before sending | FAIL | PASS |
| reaction rejects id-3 before sending | FAIL | PASS |
| reaction rejects id-4 before sending | FAIL | PASS |
| reaction rejects id-5 before sending | FAIL | PASS |
| reaction rejects id-6 before sending | FAIL | PASS |
| reaction rejects emoji-0 before sending | FAIL | PASS |
| reaction rejects emoji-1 before sending | FAIL | PASS |
| reaction rejects emoji-2 before sending | FAIL | PASS |
| reaction rejects emoji-3 before sending | FAIL | PASS |
| reaction rejects emoji-4 before sending | FAIL | PASS |
| reaction rejects emoji-5 before sending | FAIL | PASS |
| reaction accepts metadata session attribution and the listener route | FAIL | PASS |
| reaction window refusal records closure without suggesting a template | FAIL | PASS |
| reaction refusal gateway_refused is permanent | FAIL | PASS |
| reaction refusal unknown_message is permanent | FAIL | PASS |
| reaction refuses an unbound conversation without a send | FAIL | PASS |
| reaction settled receipt survives restart; changed target or emoji conflicts | FAIL | PASS |
| reaction retries a 500 and a lost answer under the same gateway key | FAIL | PASS |
| reaction shares the publish chain for its owner | FAIL | PASS |
| CLI reaction placement records one transcript and counts as an answer | FAIL | PASS |
| CLI reaction removal records one transcript and counts as an answer | FAIL | PASS |

## Review regressions

From `whatsapp/adapter/test`: `pnpm exec node --test --test-name-pattern 'in-flight key ownership|unsent attempt' react.test.js`.

Tests were committed in `c83027ff9234422e88425732f2798df32d2f3cdc`, before the correction. Six failed before the fix and six passed afterward.

| Test | Before correction | After correction |
| --- | --- | --- |
| in-flight key ownership: rejected text never poisons a reaction receipt | FAIL | PASS |
| in-flight key ownership: rejected file never poisons a reaction receipt | FAIL | PASS |
| in-flight key ownership: rejected update never poisons a reaction receipt | FAIL | PASS |
| unsent attempt: text retry attributes the actual sending session | FAIL | PASS |
| unsent attempt: reaction retry attributes the actual sending session | FAIL | PASS |
| unsent attempt: file retry attributes the actual sending session | FAIL | PASS |

## Adapter quotes

From `whatsapp/adapter/test`: `pnpm exec node --test quote.test.js`.

| Test | Before implementation | After implementation |
| --- | --- | --- |
| quote goes on the first chunk only and reports the first answer | FAIL | PASS |
| quote rides on a file message | FAIL | PASS |
| invalid quote id "{message_ts}" sends plain | FAIL | PASS |
| invalid quote id "two words" sends plain | FAIL | PASS |
| invalid quote id "a\nb" sends plain | FAIL | PASS |
| invalid quote id "a\u0000b" sends plain | FAIL | PASS |
| invalid quote id "ab" sends plain | FAIL | PASS |
| invalid quote id "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx sends plain | FAIL | PASS |
| invalid quote id null sends plain | FAIL | PASS |
| invalid quote id 1 sends plain | FAIL | PASS |
| quote receipt reports applied | FAIL | PASS |
| quote receipt reports unknown_message | FAIL | PASS |
| quote receipt reports meta_refused | FAIL | PASS |
| quote receipt reports replayed | FAIL | PASS |
| quote receipt reports gateway_without_quotes | FAIL | PASS |
| quote on an update is accepted but omitted with the template reason | FAIL | PASS |
| quote remains the first request after failure/restart (omitted) | FAIL | PASS |
| quote remains the first request after failure/restart (wamid.different) | FAIL | PASS |
| pin: no quote keeps the exact existing wire and receipt | PASS (compatibility pin) | PASS |
| synthetic reply target keeper:fixture:1 never blocks delivery | FAIL | PASS |
| synthetic reply target wamid.failed:status:failed never blocks delivery | FAIL | PASS |
| registered reply template renders the inbound message id in the fake gc paste | FAIL | PASS |

## Gateway quotes

From `whatsapp/gateway`: `pnpm exec vitest run test -t 'quoted replies|quote wire'`.
Before: 18 failed, 7 passed, 308 unrelated tests skipped. After: all 25 selected tests passed.

Each of these failed before implementation and passed after:

- quote wire: text and every media type carry context beside their content
- accepts context on text and preserves the message-id-only body
- accepts context on image and preserves the message-id-only body
- accepts context on audio and preserves the message-id-only body
- accepts context on video and preserves the message-id-only body
- accepts context on document and preserves the message-id-only body
- refuses malformed context null before sending
- refuses malformed context [] before sending
- refuses malformed context "id" before sending
- refuses malformed context {} before sending
- refuses malformed context {"message_id":""} before sending
- refuses malformed context {"message_id":1} before sending
- refuses malformed context {"message_id":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx before sending
- refuses malformed context {"message_id":"id","extra":true} before sending
- recognizes acknowledged inbound, joined and own-send ids
- unknown, foreign, synthetic and expired targets send plain; reactions refuse without spending a key
- falls back once after a definite refusal and replays without another send
- returns the fallback attempt's error when both attempts fail

The seven tests already passing at the base are compatibility pins: context remains ignored on templates/reactions; each of window code 131047, HTTP 429, a lost answer, HTTP 500 and a success without an id never makes a fallback send; a request without context preserves its body and omits quote headers.

## CLI and skill

Part A: `make test PYTEST_DIRS='tests whatsapp/tests' PYTEST_ARGS='-q -k "react or whatsapp_explains_message"'`: 16 failed, 54 passed, 565 deselected before implementation. With command discovery included after implementation: 71 passed, 564 deselected.

Part B: `make test PYTEST_DIRS='whatsapp/tests' PYTEST_ARGS='-q -k quote'`: 13 failed, 94 deselected before; all 13 passed after.

The failing cases before implementation, all passing afterward:

- `tests/test_texting_rules.py::test_outside_the_tables_a_text_shows_only_the_reactions_named_here[whatsapp]`
- `tests/test_texting_rules.py::test_whatsapp_explains_message_ids_removal_and_the_window`
- `tests/test_texting_rules.py::test_a_reaction_that_is_not_placed_is_said_in_words[whatsapp-When 'gc whatsapp react' fails,]`
- `tests/test_texting_rules.py::test_whatsapp_skill_explains_the_react_command`
- `whatsapp/tests/test_pack.py::test_react_posts_characters_and_cli_session[\U0001f440]`
- `whatsapp/tests/test_pack.py::test_react_posts_characters_and_cli_session[]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args0]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args1]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args2]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args3]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args4]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args5]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args6]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args7]`
- `whatsapp/tests/test_pack.py::test_react_usage_errors_send_nothing[args8]`
- `whatsapp/tests/test_pack.py::test_react_refused_receipt_prints_reason_and_hint`
- `whatsapp/tests/test_pack.py::test_publish_quote_flag_reaches_adapter[--reply-to-text]`
- `whatsapp/tests/test_pack.py::test_publish_quote_flag_reaches_adapter[--reply-to-file]`
- `whatsapp/tests/test_pack.py::test_publish_quote_flag_reaches_adapter[--reply-to-update]`
- `whatsapp/tests/test_pack.py::test_publish_quote_flag_reaches_adapter[--reply-to=wamid.example-text]`
- `whatsapp/tests/test_pack.py::test_publish_quote_flag_reaches_adapter[--reply-to=wamid.example-file]`
- `whatsapp/tests/test_pack.py::test_publish_quote_flag_reaches_adapter[--reply-to=wamid.example-update]`
- `whatsapp/tests/test_pack.py::test_publish_quote_warning_still_exits_zero[false-unknown_message-sent without the quote (unknown_message)]`
- `whatsapp/tests/test_pack.py::test_publish_quote_warning_still_exits_zero[false-gateway_without_quotes-sent without the quote (gateway_without_quotes)]`
- `whatsapp/tests/test_pack.py::test_publish_quote_warning_still_exits_zero[false-invalid_id-sent without the quote (invalid_id)]`
- `whatsapp/tests/test_pack.py::test_publish_quote_warning_still_exits_zero[false-template-sent without the quote (template)]`
- `whatsapp/tests/test_pack.py::test_publish_quote_warning_still_exits_zero[false-meta_refused-sent without the quote (meta_refused)]`
- `whatsapp/tests/test_pack.py::test_publish_quote_warning_still_exits_zero[unknown-replayed-sent; the gateway did not say whether the quote was applied]`
- `whatsapp/tests/test_pack.py::test_publish_quote_empty_or_absent_is_omitted_and_missing_value_is_usage_error`
