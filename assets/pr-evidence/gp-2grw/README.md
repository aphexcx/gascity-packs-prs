# Follow-through delivery recovery

Base: `c546802da5d42fb5d073b7d0bab6716b2e48b6f5`.
All fixtures use invented owners and messages. No live mail was sent.

## Reproduction before implementation

The three requested classes still reproduced at the base. With the new
regression tests and unchanged production code, `pnpm exec node --test`
from `follow-through/test` on Node `v24.21.0` reported **91 tests: 84 passed,
7 failed**, exit 1. The failures include the parent test and four subtests
for uncertain follow-up/reminder delivery, plus night-only hours and
unbounded retries.

- After either a crash following send or a send reporting an uncertain
  failure, replay sent a second mail despite the first being archived.
- Night-only hours made doctor fail; the mayor received no explanatory notice.
- A failing reminder retried every minute instead of stopping after backoff.

## Verification after implementation

`pnpm exec node --test`, from `follow-through/test`, Node `v24.21.0`:
**95 tests passed, 0 failed, 0 skipped**, exit 0.

The suite includes real child-process SIGKILL at every persisted delivery
boundary, archived-mail replay over a week, a history service that fails,
consent and hours enforcement, cancellation, concurrent ticks, and bounded
mail bodies. The fake mail service only suppresses live inbox duplicates;
archived messages are discoverable solely through the history query.

With the city Node toolchain first on PATH,
`uv run --with pytest python -m pytest tests -q` from `follow-through`:
**5 passed**, exit 0. This includes the complete Node suite and the shipped
command, order, and doctor wrappers. The pytest module was resolved before
the run. No unsupported-engine warning occurred.

`git diff --check -- follow-through`: exit 0.

## Result and limits

Unknown deliveries are persisted and reconciled against the city message
store, including closed and ephemeral mail. A failed or malformed lookup
does not authorize another send. This relies on retained mail history; it
does not recover proof that an operator has deleted from that store.

Night-only hours appear as information in doctor and status. Each such owner
gets one durable notice to the local mayor. Reminders still work regardless
of hours or consent. The existing hours rule is unchanged.

Delivery attempts back off by 1, 2, 4 and 8 minutes and stop at five attempts.
An uncertain final crash may be reconciled once more without another send.
Stopped records stay visible in doctor and in one status summary line for
operator action. The pack continues to mail only the local mayor.

The requested review is STANDARD. The draft PR's review trail records its
verdict against the committed head; mayor review and merge approval remain
separate gates.
