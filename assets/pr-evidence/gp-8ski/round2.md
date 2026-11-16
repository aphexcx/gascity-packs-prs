# Round 2: exact approval and usable permission

Round base: `275618ec36161c48133998569f44aa512d125326`.
Google acceptance remains **UNPROVEN**. All requests use local fakes.
The branch and draft PR continue from round 1 without a rebase.

| Requirement | State | Change |
| --- | --- | --- |
| A. The owner's whole reply is the code | DONE | Trim surrounding whitespace and compare without letter case. Refusals, questions, quotations, extra words, repeated codes, embedded codes and punctuation refuse without a permission request or write; approval remains unused. |
| B. No usable token, no spent yes | DONE | Prepare and validate the create before saving approval as used. The prepared call has no second local token-expiry check after consumption. A short-lived token leaves the durable request unused; the same yes succeeds with a good token. Consumption is still durable before the POST. |
| C. Plain skill wording | DONE | Explain that Google's acceptance has not been proven and tell the owner what a refusal said. No internal testing instructions in the invitation section. |
| D. Room name and id in the ask | DONE | The command's ask includes the display name and exact Google resource id; the skill and help require both. |
| E. No guessed cure for Google's refusal | DONE | HTTP 403 PERMISSION_DENIED and 400 FAILED_PRECONDITION report the actual status and an unknown cause. The README and answers table make no promise about organization settings. |

## Verification

The regression tests were committed first in `c291ce3b`. Against round 1
production, the focused reproduction exits 1: 4 failing top-level tests,
15 failing and 22 passing subtests, no skips. The unchanged exact-code
success row remains green; the lowercase row becomes green with the fix.

After the fix, the focused join suite exits 0: 23 top-level tests and
69 subtests pass, no skips:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^TestJoin|^TestMissingPermissionChangesNothing$' -count=1 -json
```

Full-suite and individual regression receipts are recorded after completion.

## Changed command text

- Invalid approval: "The owner must reply with this room's code alone, nothing else."
- Unusable token: "The permission could not be used to join. Nothing changed; run it again with the same owner's yes."
- Ask: "Ask the owner in a direct message about room [quoted name] ([exact id]). To say yes, reply with this room's code alone, nothing else, before it expires in 24 hours."
- Generic requested result: "Ask the owner in a direct message to reply with this room's code alone, nothing else, before it expires in 24 hours."
- Still invited: "An invitation exists, but Google did not join this identity. The cause is not known yet."
- Refused create: "An invitation exists, but Google refused the join. Google answered HTTP [status] [status word]. The cause is not known yet."

The final help, skill invitation section and README setup paragraph carry
the same code-alone rule, ignoring letter case and surrounding whitespace.
The skill's owner ask names the room and its exact id, inviter, outside
participants and the effect of joining, then asks for the code alone.

## Review

The prior gate's one MAJOR and the independent read's matching MAJOR share
one class: mistaking a code mention for affirmative approval. Every form
is covered in `TestJoinRefusesWithoutTheOwnersYes`, including exact-code
success, with permission count, write count and durable consumption checked.
The read's MINOR 2 and 3 and NOTE 4 and 12 are addressed by B through E.
The other prior notes retain their recorded dispositions and are unchanged.
This round's DEEP review is pending; no pending verdict is treated as clean.

Ordinary outgoing and incoming message retention can still contain approval
codes. Approval records store only a digest, and the join audit has no code.
The prior startup-state fix and the permission-refusal classification remain.
