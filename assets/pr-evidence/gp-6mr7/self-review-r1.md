# Self-review r1, gp-6mr7 (the lane's own model, before READY)

Afik's 10/24 policy: the lane reviews itself on its own model before READY.
A fresh read-only reviewer subagent (Opus 5.5, the lane's model) read the
whole uncommitted diff against `origin/feat/gp-9hf1-whatsapp-pack-adapter`
(185a6098), 2026-11-04 19:49Z-19:57Z, against the body of record, the
bead's 11/4 deltas, bead A's help.md files and adapter source, the design at
bc2fd143 (window-keeper lines 116-132, round 7 lines 263-272), the gateway
branch (README, wrangler.toml) and the Meta pages fetched that day. It ran
`verbs.py` (rc 0) and `fragparse.go` (also with discord-v0) itself, and
re-hashed all 16 Meta copies against `meta-pages-read.md`.

Result: **no BLOCKER, one MAJOR by its ranking plus two raised as MAJOR,
six MINOR, nine NIT.** Every finding the lane acted on was first checked
against its cited source by the lane. All were folded in this head.

## MAJOR

| # | Finding | Lane's check | Fix |
| --- | --- | --- | --- |
| M1 | meta-setup step 5 gave the system user the app and the WhatsApp account only; the token also needs the Messaging account (where templates live), and the Business Suite panel does not cascade; "must not expire" was not on the cited page. | access-tokens.md: "admin system users have full access to all WhatsApp accounts (WAACs), Messaging accounts"; "you must grant ... **both** accounts"; "The panel does not cascade."; "choose a token expiration preference". | Step 5 now creates an **Admin** system user (full access to both account types by default), names the Employee alternative with both accounts, the error `200` symptom, and "an expiration preference that never expires (the gateway holds this one token)"; cites Access tokens. |
| M2 | The owner page's consent text said "a gentle reminder" and "once a day ... an update"; the keeper raises up to four reminders (keeper.js `MARKS = [60, 30, 15, 5]`) and the update goes only on a day with something true to say. | keeper.js:46; the design (c), (e); SKILL.md's own rule. | Both halves: "On days with something to report, it sends one short update", "up to four short reminders in the last hour". This departs from the 11/3 spec's singular "a gentle reminder" on purpose: a consent page must say what the owner consents to. |

## MINOR

| # | Finding | Fix |
| --- | --- | --- |
| 1 | SKILL.md: "hours or time zone, record them"; hours.sh:27 refuses unless wa_id, --hours and --tz are all given. | "The verb takes both together, so ask for whichever one is missing." |
| 2 | meta-setup: "App settings > Basic", "under the From number" and WhatsApp Manager as an add path are not on the cited pages. | App secret: "the app's **Basic settings** page in the App Dashboard"; the phone number id: "shown on **API Setup** with the test number" with the page's own words; the WhatsApp Manager add path dropped; the use-case path for API Setup named with the page's path beside it. |
| 3 | "the price changes, the route does not": a template filed as marketing is refused to an owner who turned marketing off (131050). | Step 9 says so, citing Error codes. |
| 4 | The real line comes with a new Messaging account, and templates live there: switching the gateway's number id alone would lose `dynasty_update`. | whatsapp-business-accounts.md confirms. New step 12: make the template again on the new Messaging account and wait for the approval before the switch. |
| 5 | Display name timing: checked only at a higher messaging limit; after an approved change the number must be registered again. | Step 13 says both, with the 14-day window from display-names.md, and that the owner's saved "Dynasty" covers the gap. |
| 6 | Owner page length (362 / 414 words) against "reads in a minute"; the retention line was half right (the mayor keeps memory, the adapter journals envelopes). | Trimmed to 289 / 338 words; the retention claim dropped ("Your messages reach it through Dynasty's own server."). |

## NIT

| Finding | Disposition |
| --- | --- |
| The fragment quoted only the unshifted nudge. | Added the after-hours shape. |
| SKILL.md "After the business's hours it reads" read as "arrives after hours". | Rewritten: when the window would close after hours, the nudges come before close of business and read .... |
| SKILL.md "nobody from the business has written yet" vs "Never message a number that has not written to you". | The Never line now says a bound phone wrote at least its invite code. |
| SKILL.md skipped the `<kind> not saved: <reason>` line (media.js). | Added: say so and ask them to send it again. |
| meta-setup "You do every step here" vs the mayor making the register call. | Intro rewritten. |
| meta-setup step 2 skipped the publishing requirements screen. | Added. |
| meta-setup used WhatsApp > API Setup for a use-case app. | Both paths named (step 10). |
| Privacy policy and data deletion URLs not supplied. | Step 8 says to ask the mayor for them and names a missing page as the open item; the READY mail carries it. |
| The pt_BR template reaches an owner only if the city sets `WHATSAPP_UPDATE_LANGUAGE=pt_BR`. | Step 9 says a Brazilian city sets `pt_BR`. |

## Checked clean by the reviewer

All 16 quoted verb forms byte for byte against A's help.md; Afik's rule
quoted verbatim in the skill and the fragment; the keeper rules against
design lines 116-132 and keeper.js (actor, both daily-update forms, both
nudge forms, never reply to a keeper event, consent --off, a refused nudge
never a template); the operator paragraph against A's help.md and
doctor/restore (the bead's "watermark reads behind gc's store" is inverted;
the skill follows A); no phone number, token or secret anywhere; every
other Meta quote on its cited copy; whatsapp.dynasty.computer and
`/meta/webhook` match the gateway (gateway.ts:951); the fragment is valid
and composes; relative links resolve.
