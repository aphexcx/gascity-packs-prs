# Codex QUICK, round 1 (the one cross-model read of the delta), gp-6mr7

- Command: `/Users/tailor512/city/.gc/shims/cxswap/codex review --base origin/feat/gp-9hf1-whatsapp-pack-adapter -c 'sandbox_workspace_write.network_access=true'`
  from the lane worktree. This is the hub launcher: CODEX_HOME is the hub home,
  provider CLIProxyAPI.
- Header: model gpt-6-astra, provider hub, reasoning effort xhigh, session
  01a1a857-76cf-7683-8b2c-d18df2d38d27.
- Started 2026-11-04T20:54:25Z, ended 20:59:40Z, rc 0.
- Head 8062fa2c599095d9d58c72432663411334d62b6b. Base A's head
  ba9155312211ff87bf18a6b4c22a5f36f2481710, merged into this branch at
  7d6955f0, so the diff is this bead's files only.
- Two earlier attempts with bare `codex` answered "You've hit your usage
  limit ... try again at 6:31 PM": 20:02Z at 0a9394a8
  (`runs/codex-r1-usage-limit.out`) and 20:53Z at 8062fa2c. The mayor's
  20:52Z word named the hub route.

## Codex's verdict (verbatim)

> The pack tests and fragment composition checks pass, but the setup instructions permit a template category that cannot serve US recipients. The new guidance also misstates per-phone window behavior and excludes supported time-zone-only configuration.
>
> - [P1] Require utility approval for the US rollout — whatsapp/docs/meta-setup.md:121-125
>   If Meta approves `dynasty_update` as MARKETING, the planned US owners cannot receive it at all: [Meta's limits](https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/marketing-templates/per-user-limits#united-states-phone-numbers) prohibit delivery to US numbers regardless of consent. This is not merely a price increase. Since step 12 allows cutover after approval, these instructions can leave the only out-of-window messaging path unusable. Require utility approval for US recipients or explicitly mark reclassification as a setup blocker.
>
> - [P2] Allow recording a time zone without fixed business hours — whatsapp/skills/whatsapp/SKILL.md:146-148
>   For an owner without fixed hours who supplies only their time zone, this instruction unnecessarily requires another field and prevents saving a supported configuration. `commands/hours.sh` already accepts `--clear --tz <zone>`, and the keeper uses that zone for the daily update's schedule. Document this case rather than always asking for hours; otherwise the keeper continues using the city's time zone despite the owner's supplied preference.
>
> - [P2] Describe the window as belonging to this person's chat — whatsapp/docs/owner-onboarding.md:26-29
>   When two people from the same business join using separate phones, one person's reply does not extend the other's window or cancel their reminders. Both `ledger.js` and `keeper.js` track the clock separately by `wa_id`; this release supports one-to-one chats, not a shared business conversation. Replace the business-wide wording with replies from the recipient in this chat, in both languages, so owners do not rely on a colleague's acknowledgement to keep their own line open.

## The lane's check and fix

| Finding | Check | Fix |
| --- | --- | --- |
| P1 | The lane fetched Meta's per-user limits page (sha256[:16] 0add1eb271296926, 2026-11-04 21:0xZ): "WhatsApp does not currently deliver marketing template messages to WhatsApp users with United States phone numbers". Taylor's test phone and two customers' phones are US numbers. | meta-setup step 9: the template must be approved as Utility, and a Marketing approval is a blocker: change the wording and ask for a review within the 60 days; re-check the category before relying on it. Step 12: the switch waits for approval as Utility. Cites the per-user limits page. |
| P2 time zone | `hours.sh` at ba915531 sends `{wa_id, hours: "", tz}` for `--clear --tz`. Design (b): with a time zone alone, the update comes at the fixed hour of the business's own day. | The skill records hours with their zone, and quotes `gc whatsapp hours <wa_id> --clear [--tz America/Sao_Paulo]` byte for byte from A's help.md for a zone without hours. |
| P2 window | keeper.js and ledger.js key every clock by wa_id; this release has one-to-one chats only. | Owner page, both halves: "When you haven't written for almost a day", "Your reply keeps the line open another day", and a new line, "Each phone has its own chat and its own 24 hours: a colleague's reply doesn't keep your line open." Skill: "Every phone has its own window: one person's reply does not open another's." |

Codex also printed a `git diff --check` note: trailing whitespace in
`runs/whatsapp-tests.red-meta-names.txt`. That file is pytest's output kept
byte for byte, so it stays as captured.
