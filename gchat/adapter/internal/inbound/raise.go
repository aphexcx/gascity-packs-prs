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

// raise reports what is due at the end of a poll pass: every change in intent
// or pending that has waited StuckAfter since it was recorded, and every
// unresolved change with a cause other than withdrawn, each only while it was
// never raised. An unresolved row from before causes has none and is never
// raised. raise runs outside the gate and the ledger lock, makes at most
// RaisesPerPass calls, and marks a change raised only after its call
// succeeded. A change whose call failed waits raiseRetry before the next try.
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
		if c := l.Changes[id]; c == nil || !c.Raised.IsZero() || c.Status == "committed" {
			delete(p.raiseTried, id)
		}
	}
	var due []*ledger.ChangeState
	for _, c := range l.Changes {
		if !c.Raised.IsZero() {
			continue
		}
		switch c.Status {
		case "intent", "pending":
			if now.Sub(c.Created) < p.opts.StuckAfter {
				continue
			}
		case "unresolved":
			if c.Cause == "" || c.Cause == causeWithdrawn {
				continue
			}
		default:
			continue
		}
		if tried, ok := p.raiseTried[c.ID]; ok && now.Sub(tried) < raiseRetry {
			continue
		}
		due = append(due, c)
	}
	sort.Slice(due, func(i, j int) bool { return earlier(due[i], due[j]) })
	if len(due) > RaisesPerPass {
		due = due[:RaisesPerPass]
	}
	for _, c := range due {
		if ctx.Err() != nil {
			return
		}
		if err := p.opts.Raise(ctx, p.report(l, c, now)); err != nil {
			p.raiseTried[c.ID] = now
			p.opts.Logger.Warn("inbound delivery raise failed; retrying in a minute", "change", c.ID)
			continue
		}
		at := p.opts.Now().UTC()
		err := p.update(func(l *ledger.Ledger) {
			// resolve copies the Unresolved record back into Changes: mark both.
			for _, records := range []map[string]*ledger.ChangeState{l.Changes, l.Unresolved} {
				if current := records[c.ID]; current != nil && current.Raised.IsZero() {
					current.Raised = at
				}
			}
		})
		if err != nil {
			// The mail went out; gc's dedup key keeps a second send to one mail.
			p.raiseTried[c.ID] = now
			p.opts.Logger.Warn("inbound delivery raise not recorded", "change", c.ID)
			continue
		}
		delete(p.raiseTried, c.ID)
	}
}

func hours(d time.Duration) string {
	if d%time.Hour == 0 {
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
	stop := c.Created.Add(p.opts.RetryBound).UTC().Format(time.RFC3339)
	var b strings.Builder
	line := func(key, value string) { fmt.Fprintf(&b, "%s: %s\n", key, value) }
	if waiting {
		b.WriteString("A Google Chat message has not reached the agent yet. The gchat adapter keeps it and keeps trying.\n\n")
	} else {
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
	if c.Cause == "" {
		line("Cause", "none recorded")
	} else {
		line("Cause", c.Cause+" ("+causeMeaning[c.Cause]+")")
	}
	if !waiting {
		line("Reason", c.Reason)
	}
	line("Age", now.Sub(c.Created).Round(time.Second).String())
	if waiting {
		line("POSTs made", strconv.Itoa(c.Attempts))
		bound := stop + " (" + hours(p.opts.RetryBound) + " after it was recorded)"
		if c.Status == "pending" {
			line("Receipt awaited until", sentAt(c).Add(p.opts.PendingDeadline).UTC().Format(time.RFC3339))
			bound += ", if gc then answers that it was not delivered"
		}
		line("Retrying until", bound)
	}
	read := "gc gchat read --space " + c.Space
	if thread != "" {
		read += " --thread " + thread
	}
	read += " --since " + c.Created.Add(-time.Hour).UTC().Format(time.RFC3339)
	b.WriteString("\nRead the message (from an hour before it was recorded): " + read + "\n")
	b.WriteString("Every delivery the adapter holds: gc gchat status\n")
	if waiting {
		b.WriteString("This is the only mail for this message. If it has not landed by " + stop + ", the adapter stops trying and gc gchat status lists it for gc gchat resolve.\n")
	} else {
		b.WriteString("After checking whether the agent has the message:\n")
		b.WriteString("  gc gchat resolve " + c.ID + " --accept marks it delivered.\n")
		b.WriteString("  gc gchat resolve " + c.ID + " --replay posts it again. A replay can duplicate a turn whose first outcome is unknown.\n")
	}
	b.WriteString("\nThis mail never carries the message's text or its sender; read them in Chat with the command above.\n")
	return Raise{ID: c.ID, Space: c.Space, Thread: thread, Message: c.Message, Body: b.String()}
}
