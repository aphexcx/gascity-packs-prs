# gp-urx1: PR 103's four MINORs

Base: `4d14c7c5d52a468a2af7e387175ff4fb20366ec5` (main, the PR 103 squash).
Every cited line of the review (whatsapp/gateway/README.md, wake-client.ts,
gateway.ts, inbound.js, link.js, index.js at the base) was read at this base;
none had moved.

| Commit | What |
| --- | --- |
| `86118d6a` | the class tests, alone, red at the base |
| `82dca5ab` | the code, the README and the changelog |
| `45fdf881` | the self-review's two findings as tests, red at `82dca5ab` |
| `c595dc47` | the self-review's fixes |
| (this commit) | this evidence only |

All runs used local fakes, local D1 and a real local Durable Object in
workerd. No deployment, no Wrangler command other than the package's dry-run
build, and no request to a live gateway, Cloudflare's API or Meta.

## Commands

The city pnpm wrapper reported `pnpm exec node --version` = `v24.21.0`.

| Suite | Command |
| --- | --- |
| Gateway | `pnpm test`, `pnpm typecheck`, `pnpm build` in `whatsapp/gateway` |
| Adapter | `pnpm test` in `whatsapp/adapter` |
| Pack | `env -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest whatsapp/tests -q` from the root |
| Class tests, gateway | `pnpm exec vitest run test/wake.test.ts -t gp-urx1` in `whatsapp/gateway` |
| Class tests, adapter | `node --test --test-concurrency=1 --test-name-pattern='gp-urx1' test/link.test.js test/link-engine.test.js` in `whatsapp/adapter` |

## Counts

| Suite | Base `4d14c7c5` | `82dca5ab` | Tip `c595dc47` |
| --- | --- | --- | --- |
| Gateway `pnpm test` | 306 passed | 321 passed | 324 passed (306 + 18) |
| Gateway `pnpm typecheck` | passed | passed | passed |
| Gateway `pnpm build` (dry run) | passed | passed | passed |
| Adapter `pnpm test` | 269 passed | 279 passed | 280 passed (269 + 11), on the second run; see below |
| Pack pytest | 78 passed | 78 passed | 78 passed |

The dry run lists `env.CITY_WAKE (CityWake)` and `env.WAKE ("on")`, then
`--dry-run: exiting now.` No existing test was changed or weakened. The fake
gateway gained two options, off by default: `closeOnOpen` on its upgrade
handler and `inboxFailures` on its inbox.

The first adapter run at `c595dc47` passed 279 and failed 1:
`conversation.test.js`'s "the recipient (C1)" timed out after 20.9 s
waiting for an ack, with the held link off (that file's own setting). That
test touches no code this branch changed. Run alone, the file passed 11 of 11
three times at the base and three times at the tip, alternating. The whole
adapter suite then passed 280 of 280 at the tip.

## Red and green

| Class test | Red | Green |
| --- | --- | --- |
| Gateway WAKE table, 15 string values | at the base: 5 failed (`OFF`, `Off`, `" off"`, `"off "`, `"\tOFF\n"`: route 426, expected 404) | 15 passed |
| Gateway WAKE table, 3 JSON values (`false`, `0`, `true`) | at `82dca5ab`: 3 failed, `TypeError: (mode ?? "").trim is not a function` | 3 passed |
| Adapter, 10 class tests | at the base: 8 failed, 2 passed (the two guards below) | 10 passed |
| Adapter, the failed wake read | at `82dca5ab`: the link read `poll (up; unannounced_delivery)` | passed |

The adapter's eight red reasons at the base:

- Item 1, both recoveries: "the next read came 300000 ms after the last one, not 2000 ms".
- Item 1, late wakes (two): the link's reason was `''`, not `unannounced_delivery`.
- Item 2, config table: `OFF`, `Off`, `" off"`, `"off "`, `"\tOFF\n"` read on.
- Item 2, the adapter process with `" Off "`: the link came up (state `up`, not `disabled`).
- Item 4, one clock-minute: 60 upgrades, not 4.
- Item 4, the ladder unit test: a third socket at 4,999 ms after the second flap.

The two guards pass at the base and at the tip: a delivery the gateway
returns again (its ack keeps failing) keeps the safety interval; a wake that
lands while the safety read that finds its delivery is in flight keeps the
safety interval.

## Item 1: a safety read that finds an unannounced delivery

A timed read on an up link that finds a new delivery, with no wake or connect
asking for it (a read they asked for stays asked for across a failed attempt)
and none arriving while it ran, marks the link. The box then reads at
`WHATSAPP_POLL_MS` (2 s in the test) until the next wake frame or reconnect,
and is back on the 300 s safety interval after either (both measured to the
millisecond on the test clock). The status uses its existing seven fields;
while marked it reads:

    Gateway link:  poll (up; unannounced_delivery); since <the read that found it>; ...

A wake that lands after the safety read that already took its delivery trips
the rule once. Measured extra short reads before that wake: late by 1 s, 0;
late by 3 s, 1. That is floor(lateness / `WHATSAPP_POLL_MS`). The wake's own
read is one the base makes too.

## Item 2: the switches

One parse each: `createWake` in the gateway, `loadConfig` in the adapter.

| Value | Gateway `WAKE` | Adapter `WHATSAPP_HELD_CONNECTION` |
| --- | --- | --- |
| absent, `""`, `"   "` | on | on |
| `on`, `ON`, `" On "` | on | on |
| `off`, `OFF`, `Off`, `" off"`, `"off "`, `"\tOFF\n"` | off | off |
| `false`, `0`, `no` | on | on, named once in the log at start |
| JSON `false`, `0`, `true` (unquoted in wrangler.toml) | on | (no such form in an env file) |
| `disabled` | (not in the gateway table) | on, named once in the log at start |

Gateway rows run through `worker.fetch` with the real object: "on" answers 426
on `/v1/wake` without an upgrade and leaves an open socket open at the next
inbox read (a ping barrier returns `pong` only); "off" answers 404 and closes
the open socket with 4503 at the next inbox read, with no frame.

## Item 4: a socket closed at once

The fake gateway accepts the upgrade and sends close 1008 at once. One
clock-minute at the defaults, counted from the box's start:

| | Upgrades | Inbox reads |
| --- | --- | --- |
| Base | 60 (every second, 0 to 59 s) | 61 (the review measured 60) |
| Tip | 4, at 0, 1, 6 and 21 s | 33 |
| A polling box | 0 | 30 |

A link that answered its first ping and then drops reconnects after 1 s: the
ladder starts over (unit test with a fake socket).

## Review

One self-review by a fresh Claude Opus 5.5 subagent on `82dca5ab`; no Codex
round (both Codex seats were spent). Every finding and its outcome:

| # | Severity | Finding | Outcome |
| --- | --- | --- | --- |
| 1 | MAJOR | `WAKE` written unquoted in wrangler.toml is bound as JSON; `trim` threw on every request | FIXED in `c595dc47`, red first in `45fdf881` |
| 2 | MINOR | a failed read that a wake asked for left its timed retry unannounced, so it marked the link | FIXED in `c595dc47`, red first in `45fdf881` |
| 3 | NOTE | the box switch's README row said every value other than `off` is logged; `on` is not | FIXED in `c595dc47` |
| 4 | NOTE | the `WHATSAPP_HELD_POLL_MS` row said "while the link is up"; it applies while held | FIXED in `c595dc47` |
| 5 | NOTE | the 3 s late-wake test left little real time for the read due at 2 s | FIXED in `45fdf881`: it waits for that read in real time |
