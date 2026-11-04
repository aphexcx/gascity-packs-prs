# Codex QUICK, round 2 (gp-awtb): the round-2 delta of PR 75

- Command: `/Users/tailor512/city/.gc/shims/cxswap/codex exec --sandbox read-only -`
  with the review prompt on stdin, from the lane worktree (the hub
  launcher). The prompt named the diff
  `git diff origin/feat/gp-6mr7-whatsapp-pack-surface..HEAD` (base
  `0e7bea6d`, head `20631a49`: e1da9eb7, 71c04ab0, 20631a49) and the five
  questions: the failed-delivery advice against render.js, inbound.js and
  the gateway on main; the page's self-consistency, step numbers and marks;
  the Meta quotes; secrets; the new verbs.py/red.py checks.
- A first attempt as `codex review --base ... -` stopped at once:
  "the argument '--base <BRANCH>' cannot be used with '[PROMPT]'" (rc 2,
  22:57:49Z). `exec` carries the prompt.
- Header: OpenAI Codex v0.153.3, model gpt-6-astra, provider hub, sandbox
  read-only, reasoning effort xhigh, session
  01a1a8c9-1807-7c22-80b2-455d2e19b800.
- Started 2026-11-04T22:58:31Z, ended 23:00:30Z, rc 0, 53,835 tokens.
- The Meta spot-check was skipped: the read-only sandbox could not resolve
  developers.facebook.com. The lane's own fetch at 22:55Z matched every
  quote (`meta-pages-read.md`, round 2 table).

## Codex's verdict (verbatim)

> - **P2 — meta-setup.md:265:** Step 13 gates gateway cutover on category Utility alone; a template can already have that category while still PENDING. **Fix:** require status **APPROVED** and category **UTILITY** for each required language before switching numbers.
> - **P3 — SKILL.md:105, also fragment:30:** “Under the owner’s name” contradicts the code: gateway status rows have `profile_name = NULL`, so `renderMessage` uses the recipient’s `wa_id` as the display name. **Fix:** say “under the recipient’s WhatsApp ID” in both places.
>
> No P1 findings. The retry/window/marketing-refusal advice is safe. No other setup contradictions, stale step references, secrets, or personal phone numbers found.
>
> `verbs.py` passes all 46 checks. Read-only, in-memory mutations reproduced exactly the three expected failures; adapter and fragment prefix mutations also fail their respective checks.
>
> Meta spot-check skipped: DNS could not resolve `developers.facebook.com`.

## The lane's check and fix

- P2, checked: true. A template's category is set at filing; its status
  moves separately (`dynasty_update` reads category MARKETING while status
  PENDING today). Step 13 now waits until both languages read status
  **APPROVED** with category **UTILITY**; step 9's closing line says the
  same.
- P3, checked: true. The gateway writes status rows with `profile_name`
  NULL (gateway.ts:621-622 and :815-816 on main at 54f53be7), the poll
  passes it through (:700-710), and render.js:123 falls back to the
  `wa_id` for the display name. The skill now says the line comes "in that
  person's conversation, sent as their WhatsApp id (the number, not their
  name)"; the fragment says "sent as the person's bare WhatsApp id".
- After the fold: verbs.py 46/46 PASS, red.py RED OK (three FAIL),
  fragparse `slack-v0` + `whatsapp-v0` OK, whatsapp/tests 42 passed
  (`runs/*.r2.*`).
