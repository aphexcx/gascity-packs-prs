# no-AI-slop pass (detect mode), gp-6mr7

Run 2026-11-04 19:4xZ with the `no-ai-slop` skill in detect mode on
`whatsapp/skills/whatsapp/SKILL.md` and the English half of
`whatsapp/docs/owner-onboarding.md`. Readers: an AI mayor (the skill) and a
roofing contractor (the owner page). Banned words, em dashes,
throat-clearing openers, colon reveals, puffery and summary endings were
searched for; none appear in either file.

| File:line (at the pass) | Pattern | Decision |
| --- | --- | --- |
| SKILL.md:71 "Never a nudge, never a reminder, never a check-in." | negative listing | Kept: the rule's own terms from the spec and the design (Afik's rule), quoted so the mayor matches them. |
| SKILL.md:108 "No marketing, no filler." | negative listing | Kept: the design's words for the daily update (window-keeper (b)). |
| SKILL.md, the nudge paragraph: three sentences opening "Never ..." | robotic rhythm | Fixed: two merged ("Never repeat the update, and never add a nudge of your own: ..."; "... stays unsent; it never becomes a template."). |
| owner-onboarding.md:16 "Then just write." | often-empty "just" | Kept: here it means "no more steps". |
