---
name: follow-through
description: Coming back once to what the owner left open, and keeping the reminders the owner asks for. Read it before you open an item, before any follow-up, and whenever a follow-through mail arrives.
---

# Follow-through

Read this before you open an item, before you write any follow-up, and
whenever a mail with the subject "follow-through: follow-up due",
"follow-through: reminder due" or "follow-through: no daytime hours" reaches
you.

The follow-through pack keeps two lists for you: the things the owner
started and left waiting on them, and the reminders the owner asked for. It
sends you a mail when something is due. It never writes to the owner. You
do, and this skill says when.

Most of this skill is about restraint. A message nobody asked for is the
easiest way to lose the owner's trust. When you are unsure, send nothing.

A verb that takes a conversation names it the way gc does: the provider, a
slash, and the conversation id, as in `slack/C123`. That is CONVERSATION
below.

## When to open an item

Open an item when the owner started something that now waits on them, such
as a decision, a file or a yes, and the conversation moved on or went quiet.

- Never open one for work you owe. That is yours to finish, not the owner's
  to be asked about.
- Never open one for small talk.
- Open one item for one thing. Its summary is one line that a stranger could
  read: what the owner started, and what it waits on.

```
gc follow-through add --conversation CONVERSATION --summary "Choose one of the two logo drafts"
```

The verb prints the item's id. `gc follow-through list --conversation CONVERSATION`
shows the items still open in one conversation, with their ids.

Close the item the moment it no longer waits on the owner:

- When the owner picks it up anywhere, here or in another conversation,
  close it with `gc follow-through done ID`.
- When the owner says they no longer want it, close it with
  `gc follow-through drop ID`.

## The one follow-up

A mail with the subject "follow-through: follow-up due" lists the items of
one conversation that are due their follow-up. If it says more are due than
it shows, run the command it gives, `gc follow-through list --wake WAKE_ID`,
and read them all before you decide.

The mail comes from the pack, not from the owner. Never reply to it.

For each item, decide whether one line from you still helps the owner:

- If the owner already did it, here or anywhere else, close it with done and
  leave it out.
- If the owner said they no longer want it, close it with drop and leave it
  out.
- If it still waits on the owner and a line would help them, it goes in.
- If you are unsure, it stays out.

When at least one item goes in, send one short message in that conversation
that covers every item that goes in, the way that channel's skill says. Never
write about those items again. When no item goes in, send nothing.

There is no second follow-up, ever. The pack will not ask again about an
item it raised. If a mail names an item you already handled, with a
follow-up or by leaving it out, it is the same ask arriving a second time.
Send nothing for it. Look at the conversation before you write: a follow-up
you already sent is there.

A follow-up mail can come long after its items fell due, because a mail
that could not reach you waits for the owner's next allowed wake. Check each
item before you write.

## What a follow-up sounds like

Write one or two plain sentences that name the thing and offer the next
step.

- Write nothing that blames, such as "you forgot" or "still waiting on you".
- Never write "just checking in".
- Add no offer, greeting or news to it.

These two examples show the shape only:

> Example: The two logo drafts are ready whenever you want to choose. Tell
> me which one, and I will send it to the printer.

> Example: Two things from Tuesday are still open: the date for the staff
> training, and the signed lease. Send either one when it suits you, and I
> will take it from there.

## When you may not write

- Write a follow-up only because a follow-through mail asked for it. Never
  write one because you happen to be awake, or because you saw an open item
  in the list.
- Never write a follow-up to an owner whose consent is off. If the owner
  said stop after the mail was sent, send nothing.
- Never write a follow-up late in the owner's evening or early in their
  morning. Write one only between 08:00 and 20:00 in the owner's time, and
  inside the hours they gave you.

The pack checks consent and the owner's hours before it sends the mail, but
the mail can reach you later than that. Before you write, look at the
conversation for a stop, and check the owner's clock with the zone named in
the mail's times. If it is now outside those hours, send nothing for those
items.

## Consent, hours and zone

The pack raises a follow-up only for an owner whose consent is on and whose
time zone is recorded. Consent that was never recorded counts as off.

Ask the owner once, in a conversation they started, whether they want you to
come back to things they leave open. Record the answer:

```
gc follow-through consent CONVERSATION --on
gc follow-through consent CONVERSATION --off
```

When the owner says stop, record off at once, before anything else in that
turn.

Record the owner's hours and time zone when they give them:

```
gc follow-through hours CONVERSATION --hours "Mon-Fri 09:00-18:00" --tz America/Chicago
gc follow-through hours CONVERSATION --tz America/Chicago
gc follow-through hours CONVERSATION --clear
```

The second form changes only the zone and keeps the hours. The third removes
the hours and keeps the zone. Ask the owner for their time zone. Never guess
it, and never use the box's own.

Follow-ups come only inside the owner's hours and only between 08:00 and
20:00 in their time. When the owner's hours run late into the night,
follow-ups may come only once a day. When the owner's hours are all at
night, none come at all, and `gc follow-through status` shows that owner with
no next wake and says why. Tell such an owner, when they give you those
hours, that you will not come back to things they leave open, because you
send no follow-ups at night.

## No daytime hours

A mail with the subject "follow-through: no daytime hours" names one
conversation whose recorded hours leave no time between 08:00 and 20:00. No
follow-up will come in that conversation, and the owner's reminders are not
affected. The mail can come at any hour, whether the owner's consent is on,
off or never recorded. It comes from the pack, not from the owner, and you
never reply to it.

Never write to the owner because of this mail, and bring it up only the next
time they write to you. When they do, look in the conversation the mail
names. If it shows you already told them what the section above says to
tell an owner whose hours are all at night, there is nothing more to say.
Otherwise tell them that, once.

This mail can reach you twice for one conversation, and the second is the
same notice arriving a second time. Send nothing for it.

## Reminders

When the owner asks you to remind them of something, set it with the pack:

```
gc follow-through remind --conversation CONVERSATION --in 2h --text "Call the supplier"
gc follow-through remind --conversation CONVERSATION --at 2027-01-15T09:00:00-06:00 --text "Send the invoice to the landlord"
```

Never keep a reminder only in your own session. A session ends, and a
reminder kept there ends with it.

The verb prints the reminder's id and when it is due, in the owner's zone
when one is recorded. Only then tell the owner it is set: react with ⏰ where
the channel lets you react, and otherwise answer with one short line that
says when.

A reminder comes at any hour and needs no consent and no zone. It comes soon
after the time the owner chose, unless its mail delivery stopped. The Health
section below says what you do then. When a mail with the subject
"follow-through: reminder due" arrives, look at the conversation it names. If
the owner cancelled that reminder there, send nothing. The same mail can also
reach you twice for one reminder. Look in that conversation, and on any other
channel the WhatsApp section below may have sent you to, not in what you
remember, because a session ends. If you already gave the owner this
reminder there after the time it was due, naming that time, it is the same
reminder arriving a second time, even if this mail says it is late. Send
nothing for it, as for a cancelled reminder. Otherwise tell the owner now,
in your own words, in that conversation. Name the time the reminder was for,
because two reminders can have the same words and you give both. If the
mail says the reminder is late, say so plainly with the time they asked for,
then give the reminder. Do not explain why it is late unless they ask.

When the owner cancels a reminder, cancel it:

```
gc follow-through remind --cancel ID
```

Tell the owner it is cancelled only when the verb says so. If it answers
that the reminder has already been raised, the pack had already tried to
send you that reminder. Its mail may already have reached you, may still
come, or may never come if its delivery stopped. Tell the owner it was
already on its way to you and that you will not pass it on, and keep to that
in every case, a stopped reminder included.

`gc follow-through remind --list` shows every reminder with its id and state.

## WhatsApp

On WhatsApp you may write freely only inside the owner's 24-hour window, as
the WhatsApp skill explains. A follow-up or a reminder that comes due after
that window has closed is not sent there as free text. It is never sent as
the update template either: by Afik's rule that template is only for an
actual update, and a follow-up or a reminder is not one. Say it on another
channel the owner uses with you, or hold it and say it when the owner next
writes.

## News about yourself

Never write to the owner, unasked, about an update to yourself. A note about
a new ability goes out only when you were given one for a major feature. Not
every update gets one, and when nobody gave you a note, you send none.

## Health

`gc follow-through status` shows the pack's health and never shows a summary
or a reminder's text. Its count of failing raises also counts a mail planned
a moment ago and not yet tried, so a count above zero is not a fault by
itself. If the same raise is still there later, with attempts and an error,
tell the operator.

A `Mail delivery stopped` line counts mails the pack will never send again,
and the pack will not tell you about them, so tell the operator at once.
Each such mail also has a line of its own, with its kind, its conversation
and the word stopped. What you do depends on its kind:

- For a stopped `follow-up`, send nothing, and never make up for it by
  writing to the owner yourself. Its items stay open, and the pack will not
  raise them again.
- For a stopped `hours-notice`, do what the No daytime hours section above
  says, as if its mail had come.
- For a stopped `reminder`, you still give it, because the owner was told it
  is set. Find it with `gc follow-through remind --list`: it is a reminder of
  that conversation still shown as set after its time. Then do what the
  Reminders section says for a reminder mail that says it is late, as if
  that mail had just come.

## The verbs

- `gc follow-through add`: Keep an open item for one follow-up
- `gc follow-through list`: List retained open items
- `gc follow-through done`: Mark an item done
- `gc follow-through drop`: Drop an item
- `gc follow-through remind`: Set, list or cancel durable reminders
- `gc follow-through consent`: Record follow-up consent for a conversation
- `gc follow-through hours`: Record hours and time zone for follow-ups
- `gc follow-through status`: Show follow-through health without message text
