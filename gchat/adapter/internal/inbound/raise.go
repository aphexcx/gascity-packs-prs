package inbound

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
)

// raise reports what is due at the end of a poll pass (mailDue), oldest
// first. It runs outside the gate and the ledger lock, makes at most
// RaisesPerPass calls, and marks a change only after its call succeeded. A
// change whose call failed waits raiseRetry before the next try.
func (p *Processor) raise(ctx context.Context) {
	if p.opts.Raise == nil || ctx.Err() != nil || !p.raiseMu.TryLock() {
		return
	}
	defer p.raiseMu.Unlock()
	l, err := p.snapshot()
	if err != nil {
		return
	}
	now := p.opts.Now()
	for id := range p.raiseTried {
		if c := l.Changes[id]; c == nil || !p.mailDue(l, c, now) {
			delete(p.raiseTried, id)
		}
	}
	var due []*ledger.ChangeState
	for _, c := range l.Changes {
		if p.dueRaise(l, c, now) {
			due = append(due, c)
		}
	}
	sort.Slice(due, func(i, j int) bool { return earlier(due[i], due[j]) })
	if len(due) > RaisesPerPass {
		due = due[:RaisesPerPass]
	}
	for _, c := range due {
		if ctx.Err() != nil {
			return
		}
		// The calls can take seconds each: report what the change is now.
		current, err := p.snapshot()
		if err != nil {
			return
		}
		now := p.opts.Now()
		c = current.Changes[c.ID]
		if c == nil || !p.dueRaise(current, c, now) {
			continue
		}
		if err := p.opts.Raise(ctx, p.report(current, c, now)); err != nil {
			p.raiseTried[c.ID] = p.opts.Now()
			p.opts.Logger.Warn("inbound delivery raise failed; retrying in a minute", "change", c.ID)
			continue
		}
		at := p.opts.Now().UTC()
		ended := c.Status == "unresolved"
		marked := false
		err = p.update(func(l *ledger.Ledger) {
			// resolve copies the Unresolved record back into Changes: mark both.
			for _, records := range []map[string]*ledger.ChangeState{l.Changes, l.Unresolved} {
				record := records[c.ID]
				// Only the life the mail was written for. A replay while the
				// mail was in flight gave the record a fresh recorded time: a
				// mark there would read as a mail of the replay's life and
				// cover the messages behind it (gp-t542).
				if record == nil || !record.Created.Equal(c.Created) {
					continue
				}
				marked = true
				if record.Raised.IsZero() {
					record.Raised = at
				}
				if ended && record.RaisedEnd.IsZero() {
					record.RaisedEnd = at
				}
			}
		})
		if err != nil {
			// The mail went out; gc's dedup key keeps a second send to one mail.
			p.raiseTried[c.ID] = p.opts.Now()
			p.opts.Logger.Warn("inbound delivery raise not recorded", "change", c.ID)
			continue
		}
		if !marked {
			p.opts.Logger.Info("inbound delivery raise sent for an earlier life of the change; the replayed record keeps no mark of it", "change", c.ID)
		}
		delete(p.raiseTried, c.ID)
	}
}

// mailDue reports whether a change is due a mail, the throttle aside. A
// message is mailed at most twice in one life (gp-dl26, rulings D and E; a
// replay starts a new life, gp-t542):
//   - Once while it waits: in intent or pending for StuckAfter since
//     WaitingSince, never mailed in this life. A change in intent that waits behind an
//     earlier one is named in the mail of the first of them whose mail
//     covers it (covers), not mailed on its own; the live order test decides
//     it, never the stored cause word. A pending change holds nothing back
//     and is mailed on its own.
//   - Once when it ends unresolved with a cause, unless its waiting mail, in
//     this life, named the retry bound that ended it. A withdrawn message is
//     mailed only to end a waiting mail of this life, which said it would
//     still arrive. An unresolved row from before causes is never mailed.
func (p *Processor) mailDue(l *ledger.Ledger, c *ledger.ChangeState, now time.Time) bool {
	if isHeld(c) {
		return false
	}
	switch c.Status {
	case "intent", "pending":
		if !c.Raised.IsZero() || now.Sub(WaitingSince(c)) < p.opts.StuckAfter {
			return false
		}
		return c.Status == "pending" || !coveredAhead(l, c)
	case "unresolved":
		if c.Cause == "" || !c.RaisedEnd.IsZero() {
			return false
		}
		if c.Cause == causeWithdrawn {
			return waitingMailed(c)
		}
		return !waitingMailed(c) || c.Reason != reasonRetryBound
	}
	return false
}

// waitingMailed reports a waiting mail sent in the change's current life. A
// replay gives a change a fresh recorded time and clears its marks. Older
// ledgers can retain a mark from an earlier life, naming an earlier bound.
func waitingMailed(c *ledger.ChangeState) bool {
	return !c.Raised.IsZero() && !c.Raised.Before(c.Created)
}

// WaitingSince is the start of the wait that can trigger a waiting mail.
// While gc has a pending delivery, time before its latest POST does not count.
// Created is also the fallback for ledgers that predate Posted.
func WaitingSince(c *ledger.ChangeState) time.Time {
	if c.Status == "pending" && c.Posted.After(c.Created) {
		return c.Posted
	}
	return c.Created
}

// coveredAhead reports whether an earlier change of c's space in intent will
// name c in its mail, or did: one never mailed (it is older than c, so it is
// due first) or one mailed in its current life. A replayed change mailed only
// in an earlier life covers nothing, so the changes behind it are mailed.
func coveredAhead(l *ledger.Ledger, c *ledger.ChangeState) bool {
	for _, other := range l.Changes {
		if other.Space == c.Space && other.ID != c.ID && other.Status == "intent" && earlier(other, c) && (other.Raised.IsZero() || waitingMailed(other)) {
			return true
		}
	}
	return false
}

// dueRaise is mailDue and not tried in the last minute.
func (p *Processor) dueRaise(l *ledger.Ledger, c *ledger.ChangeState, now time.Time) bool {
	if !p.mailDue(l, c, now) {
		return false
	}
	tried, ok := p.raiseTried[c.ID]
	return !ok || now.Sub(tried) >= raiseRetry
}

// behind counts the changes of c's space that are in intent and later than
// c: the messages that wait for c to land.
func behind(l *ledger.Ledger, c *ledger.ChangeState) int {
	n := 0
	for _, other := range l.Changes {
		if other.Space == c.Space && other.ID != c.ID && other.Status == "intent" && earlier(c, other) {
			n++
		}
	}
	return n
}

func hours(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "1 hour"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + " hours"
	}
	return d.String()
}

var causeMeaning = map[string]string{
	causeDraftDeferred:      "text in the agent's input box held the message back",
	causePendingInteraction: "the agent's session was waiting on a pending interaction",
	causeNotDelivered:       "gc answered that nothing was delivered",
	causeBehindEarlier:      "an earlier message of the same space has not landed yet",
	causeHeld:               "gc holds the delivery and needs an operator; check the agent's session before a replay",
	causeWithdrawn:          "the sender deleted or edited the message in Chat before it was delivered",
	causeUnknown:            "the outcome is not known; the reason says why",
}

// report writes the mail for one change. It names ids, times and commands
// only: never the message's text, the sender or gc's error text, because a
// Chat message reaches the agent only as a framed turn.
func (p *Processor) report(l *ledger.Ledger, c *ledger.ChangeState, now time.Time) Raise {
	thread := ""
	if m := l.Messages[c.Message]; m != nil {
		thread = m.Thread
	}
	waiting := c.Status != "unresolved"
	// The one mail that follows a waiting mail, when the message ended.
	second := !waiting && !c.Raised.IsZero()
	// A head released from behind an earlier message keeps that word until
	// gc answers its POST; it no longer waits behind anything.
	staleBehind := c.Status == "intent" && c.Cause == causeBehindEarlier && !heldBack(l, c)
	stop := c.Created.Add(p.opts.RetryBound).UTC().Format(time.RFC3339)
	var b strings.Builder
	line := func(key, value string) { fmt.Fprintf(&b, "%s: %s\n", key, value) }
	switch {
	case waiting:
		b.WriteString("A Google Chat message has not reached the agent yet. The gchat adapter keeps it and keeps trying.\n\n")
	case second && c.Cause == causeWithdrawn:
		b.WriteString("An earlier mail said this Google Chat message was waiting and would still be delivered. Its sender withdrew it: the message was deleted or edited in Chat, and the gchat adapter will not deliver it. An edit arrives as its own message.\n\n")
	case second:
		ended := "It has ended unresolved"
		if !waitingMailed(c) {
			// The waiting mail was of the life before a replay.
			ended = "It was replayed since (gc gchat resolve --replay), and the replay has ended unresolved"
		}
		b.WriteString("An earlier mail said this Google Chat message was waiting and would still be delivered. " + ended + ": the gchat adapter has stopped trying and will not deliver it again on its own. Whether it reached the agent is in the cause and reason below. It waits for an operator.\n\n")
	default:
		b.WriteString("A Google Chat message did not reach the agent, and the gchat adapter has stopped trying. It waits for an operator.\n\n")
	}
	line("Space", c.Space)
	if thread != "" {
		line("Thread", thread)
	}
	line("Message", c.Message)
	line("Delivery ID", c.ID)
	line("Recorded", c.Created.UTC().Format(time.RFC3339))
	switch {
	case c.Status == "pending":
		line("State", "waiting for gc's receipt (pending)")
	case waiting:
		line("State", "waiting to be posted to gc (intent)")
	default:
		line("State", "unresolved")
	}
	switch {
	case c.Cause == "" && c.Status == "pending":
		line("Cause", "none (gc has the message and has not concluded its delivery)")
	case c.Cause == "" || staleBehind:
		line("Cause", "none recorded")
	case c.Reason == reasonExhausted && c.Cause == causeUnknown:
		// Only a record written by adapter 0.5.0 ends this way (resume).
		line("Cause", c.Cause+" (an older version of the adapter stopped after four POSTs and did not record gc's last answer)")
	case c.Reason == reasonExhausted:
		// The word is gc's last answer. Its meaning is told where it cannot
		// contradict the fragment: "not_delivered" would read as nothing in
		// the terminal.
		why := ""
		if c.Cause == causeDraftDeferred || c.Cause == causePendingInteraction {
			why = causeMeaning[c.Cause] + "; "
		}
		line("Cause", c.Cause+" ("+why+"gc answered one of the message's POSTs that a fragment of it is in the terminal; the adapter stops retrying such a message at its fourth POST, or at that answer when it comes later)")
	default:
		line("Cause", c.Cause+" ("+causeMeaning[c.Cause]+")")
	}
	if !waiting {
		line("Reason", c.Reason)
	}
	line("Age", now.Sub(c.Created).Round(time.Second).String())
	if notice := p.opts.NeverSilent.OwnerLine(c.Space); notice != "" {
		fmt.Fprintf(&b, "The owner was told: %q\n", notice)
	}
	if waiting {
		line("POSTs made", strconv.Itoa(c.Attempts))
		bound := stop + " (" + hours(p.opts.RetryBound) + " after it was recorded)"
		if c.Status == "pending" {
			line("Receipt awaited until", sentAt(c).Add(p.opts.PendingDeadline).UTC().Format(time.RFC3339))
			bound += ", if gc then answers that it was not delivered"
		}
		line("Retrying until", bound)
	}
	if c.Status == "intent" {
		// The head of its space: the later messages of the space wait for it.
		switch n := behind(l, c); n {
		case 0:
			line("Waiting behind it", "none yet; later messages of this space queue behind it until it lands, and gc gchat status lists them")
		case 1:
			line("Waiting behind it", "1 later message of this space, the count when this mail was sent; later ones queue behind it too, and gc gchat status lists them")
		default:
			line("Waiting behind it", fmt.Sprintf("%d later messages of this space, the count when this mail was sent; later ones queue behind it too, and gc gchat status lists them", n))
		}
	}
	// Created is when the adapter recorded the change; a catch-up record or a
	// replay is younger than its message, whose revision may be older.
	since := c.Created
	if m := l.Messages[c.Message]; m != nil && !m.Revision.IsZero() && m.Revision.Before(since) {
		since = m.Revision
	}
	read := "gc gchat read --space " + c.Space
	if thread != "" {
		read += " --thread " + thread
	}
	read += " --since " + since.Add(-time.Hour).UTC().Format(time.RFC3339)
	b.WriteString("\nRead the message: " + read + "\n")
	b.WriteString("If it is not there (an edit or a reaction on an older message), run the same command without --since.\n")
	b.WriteString("Every delivery the adapter holds: gc gchat status\n")
	if waiting {
		if c.Status == "pending" {
			b.WriteString("gc has the message, and it may already be in the agent's session. If gc reports that it was not delivered, the adapter tries again, and the message ")
		} else {
			b.WriteString("The message ")
		}
		b.WriteString("will still be delivered to the agent when its session clears, with the same delivery ID. An agent that acts on it now from Chat history must not act on it again when it arrives.\n")
		b.WriteString("No further mail is sent while it waits. If it ends unresolved for a reason other than this bound, one more mail says so. If it has not landed by " + stop + ", gc gchat status lists it for gc gchat resolve.\n")
	} else if c.Cause == causeWithdrawn {
		b.WriteString("Nothing to deliver. gc gchat resolve " + c.ID + " --accept clears it from gc gchat status.\n")
	} else {
		b.WriteString("After checking whether the agent has the message:\n")
		b.WriteString("  gc gchat resolve " + c.ID + " --accept marks it delivered.\n")
		if c.Fragment {
			b.WriteString("  Before a replay, clear the fragment first from the agent's input box, then run:\n")
		}
		b.WriteString("  gc gchat resolve " + c.ID + " --replay posts it again. A replay can duplicate a turn whose first outcome is unknown.\n")
	}
	b.WriteString("\nThis mail never carries the message's text or its sender; read them in Chat with the command above.\n")
	return Raise{ID: c.ID, Space: c.Space, Thread: thread, Message: c.Message, Body: b.String(), Ended: !waiting, Recorded: c.Created}
}
