---
name: whatsapp
description: Replying to a city owner on WhatsApp through the Dynasty number, and when the mayor may write first
---

# WhatsApp

Read this before you answer anyone on WhatsApp, and before you write first.

## Who is writing

Each WhatsApp conversation is one person: an owner of this city's business,
or someone the owner invited with an invite code. Nobody else can reach you.
A stranger who texts the number gets one canned line from the gateway and
never reaches the city. The conversation is `whatsapp/<wa_id>`, where the
wa_id is the person's WhatsApp id, and the message carries their WhatsApp
profile name.

They text the shared Dynasty number, saved on their phone as "Dynasty". You
are the same mayor they reach on Slack, with the same memory.

## How to reply

Your plain output stays in your session. Only the publish verb reaches the
phone. Write the reply to a file, then send it, as the reply line under each
message says:

```
gc whatsapp publish --to <wa_id> --text-file <path>
```

`gc whatsapp publish --to <wa_id> --text "message"` works for a simple
string. Use the file for anything with quotes or code.

Answer promptly, even if it is only "on it". The owner sees the blue ticks
and "typing…" as soon as the message reaches you, so silence after that
reads as being ignored. Answer in the language they write in.

## Write for a phone

- Short: a few lines, one question at a time.
- Plain words. No headings, no tables, no Markdown: they arrive as raw
  characters.
- WhatsApp has its own marks: `*bold*` with one asterisk on each side,
  `_italic_`, `~strike~`, and three backticks for monospace. Use them
  sparingly.
- A reply over 4,096 characters goes out as several messages. Stay far
  below that.
- Links are fine. WhatsApp makes them tappable.

## The 24-hour window

WhatsApp lets you write freely only within 24 hours of that person's last
message (the adapter keeps a two-minute margin). Each message from them
opens the window for another 24 hours. Every phone has its own window: one
person's reply does not open another's. Nothing you send opens it, the
update template included.

A send refused with `window_closed` means the window has closed. You then
have three choices:

1. Say it on Slack.
2. Wait for them to write.
3. Send the update template, and only for an actual update:

```
gc whatsapp publish --update --to <wa_id> --text "today's update"
```

The rule is Afik's, in his words: "only if there is an actual update". An
actual update is a finished task, a report that is ready, or a failure they
must know about. Never a nudge, never a reminder, never a check-in.

The template puts your text inside Meta's approved sentence ("Your city has
an update: …"), so write one or two plain sentences on one line. The adapter
refuses the template while the window is open (a free reply costs less) and
refuses empty text. Every template send is logged with its text, so the
founders can check it.

After a template the window is still closed. Wait for their reply before you
write anything free-form.

## A message marked as a redelivery

A message whose text starts with this line is the adapter's one recovery
copy after a crash or a lost response:

```
[redelivery <wamid>: a first copy may have reached you; if you already answered it, do nothing]
```

If you already answered the message with that id, do nothing. Otherwise
answer it as you would the original.

## A delivery that failed

A message reading

```
[delivery failed: <code> <title>]
```

is Meta's word that one of your sends to that phone never arrived: Meta took
it, then could not deliver it. The code and the title are Meta's. It comes
in that person's conversation, sent as their WhatsApp id (the number, not
their name), with the reply line beneath it. They did not write it, and it
does not open the window. Never answer it as their words.

Send it again once, by the window rule: free-form only while their window is
open, otherwise the update template and only for an actual update. If the
second send fails too, or the rule leaves no send, say it on Slack with the
code. Never resend a marketing refusal (`131049`, `131050`, or the template
to a US number while Meta classes it as marketing): say it on Slack instead.

## The window-keeper

For a business that agreed to it at the invite, the adapter watches the
window and tells you when to write. Its messages come from the actor
"Dynasty window-keeper". That is the adapter, not the owner. Its messages
come in two shapes.

**The daily update**, once a business day:

```
[window-keeper] <wa_id>: daily update due. Send today's short business update with one acknowledgement ask. Window open, closes <time>.
```

Send ONE short, true update from the business's own day, with ONE ask:
"reply ok so I can keep this line open". No marketing, no filler. If the day
holds nothing true to say, send nothing. The window may then lapse, and that
is by design.

When the event says `Window closed since <time>`, or that nobody from the
business has written yet, the only send is `gc whatsapp publish --update`,
and only for an actual update. Otherwise send nothing.

**The nudge**, as the window nears its close:

```
[window-keeper] <wa_id>: window closes in <N> minutes, nobody from the business has written since <time>. One short line asking anyone in the group to reply.
```

When the window would close after the business's hours, the nudges come
before close of business instead and read `window closes at <time>, after
your hours; last chance before close of business in <N> minutes.` Send
one short line asking them to reply. Never repeat the update, and never add a
nudge of your own: the keeper raises at most four a window. A nudge refused
with `window_closed` stays unsent; it never becomes a template.

Once the window has closed the keeper raises no nudge. The daily update
still comes, and it says the window is closed.

At most one update and four nudges a window.

**Never reply to a keeper event.** gc puts the reply line under every
message, keeper events included, and that line sends to the owner's phone.
Whatever you send is for the owner: the update, the nudge, or nothing. Never
"ok", "noted" or a question back to the keeper.

**The owner's word decides.** When they say they want no more updates or
reminders, turn the keeper off at once:

```
gc whatsapp consent <wa_id> --off
```

`gc whatsapp consent <wa_id> --on` turns it back on when they ask. When they
give their business hours, record them with their time zone; hours need a
zone, so ask for it if it is missing:

```
gc whatsapp hours <wa_id> --hours "Mon-Fri 09:00-18:00" --tz America/Sao_Paulo
```

When they keep no fixed hours, or no longer do, remove the hours; the update
then comes at a fixed hour of their day and the nudges run at any hour. Add
`--tz` to record their time zone with no hours (a time zone alone is
enough), or leave it off to keep the zone already recorded:

```
gc whatsapp hours <wa_id> --clear [--tz America/Sao_Paulo]
```

Both verbs take the phone in any form and print the last four digits they
matched. Check those digits against the conversation.

## Files, voice notes, reactions

Files arrive saved on this machine. The message ends with a block that
starts `[1 WhatsApp file attached]` (or `[N WhatsApp files attached]`) and
lists each file as `saved to <path>; Read that path to view it`. Read the
path before you answer about the file. A line `<kind> not saved: <reason>`
means the file never reached this machine: say so and ask them to send it
again.

A voice note carries its transcript under `[voice note transcript: <name>]`,
or a line saying where it was saved and that it was `not transcribed`, with
the reason. If it was not transcribed, say so and ask them to type it.

A reaction arrives as `[reaction <emoji> to <message id>]` and rarely needs
an answer.

You send text only. Files out come in a later round.

## When the door needs an operator

Two states are an operator's to clear, never yours. You see them on the
doctor rows and in `gc whatsapp status`.

- **A terminal hold** (the `held` row). gc took a message toward your
  terminal and cannot say whether you got it. The adapter holds it apart.
  Only an operator resolves it, after reading the envelope:

  ```
  gc whatsapp inbound resolve <rid> --delivered
  gc whatsapp inbound resolve <rid> --dead
  ```

  Never assume you got it.

- **A restore gap** (the `restore` row, naming a conversation). gc's store
  went back to an older copy: it no longer holds a message the adapter knows
  gc accepted. Until an operator runs

  ```
  gc whatsapp inbound clear-gap <wa_id>
  ```

  every in-doubt message of every conversation reaches you as the marked
  copy above. Never assume the store is intact, and never re-send anything
  on your own because the history looks short.

## Other verbs

```
gc whatsapp invite
gc whatsapp invite --proactive-consent --hours "Mon-Fri 09:00-18:00" --tz America/Sao_Paulo
gc whatsapp status
```

`gc whatsapp invite` prints a 6-digit code, good for 24 hours, and the two
lines to tell the new person. Add `--proactive-consent` only when the
business has agreed to the daily update and the reminders. `gc whatsapp
status` shows the door without secrets or message text.

## Never

- Never send a secret: no token, credential or password goes into a
  WhatsApp message.
- Never pass one person's words into another chat unless they asked you to
  relay them.
- Never message a number that has not written to you. A bound phone wrote
  at least its invite code; any other number is not yours to reach.
