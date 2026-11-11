package catchup

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
)

// settleClock is the processor's clock in these tests; the runner keeps
// frozen. The processor must see time pass for Poll to reach a retry.
type settleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settleClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *settleClock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

// withClock rebuilds the fixture's processor and runner over a movable clock.
func (f *fixture) withClock(t *testing.T) *settleClock {
	t.Helper()
	clock := &settleClock{now: frozen}
	p, err := inbound.New(inbound.Options{Config: f.opts.Config, Ledger: f.opts.Ledger, LedgerMu: f.opts.LedgerMu, LedgerPath: f.opts.LedgerPath, GC: gcclient.New(f.gc.URL(), "examplecity"), Chat: f.opts.Chat, IdentityUserID: "users/123", Logger: slog.New(slog.NewTextHandler(&f.logs, nil)), Now: clock.Now, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	f.p, f.opts.Processor = p, p
	f.newRunner(t)
	return clock
}

// TestHandledItemSettlesWhenThePollOwnsItsChange is the table of which handled
// catch-up items settle (gp-dl26, ruling A). Once HandleChange acknowledged an
// item, a change that is retrying after a definite "not delivered", or that is
// held back behind an earlier change of its space, belongs to Poll: the ledger
// record is the obligation, Poll ends it committed or unresolved and raised.
// Such an item settles, the cursor moves and a later pass writes nothing. A
// pending receipt still holds its item (for at most an hour, as before), and
// an item whose POST gc could not take is not handled and holds.
func TestHandledItemSettlesWhenThePollOwnsItsChange(t *testing.T) {
	for _, row := range []struct {
		name string
		// answer is gc's delivery status for every POST until the poll step.
		answer   string
		messages int
		down     bool
		// remaining is what the first two passes report; zero means settled.
		remaining int
		wantErr   bool
		// first and second are the states of the changes after the first pass.
		first, second string
	}{
		{name: "failed: the retrying head and the change behind it settle", answer: "failed", messages: 2, first: "intent/retry/4", second: "intent/behind_earlier/0"},
		{name: "held: the retrying head and the change behind it settle", answer: "held", messages: 2, first: "intent/retry/4", second: "intent/behind_earlier/0"},
		{name: "pending receipt holds its item", answer: "pending", messages: 1, remaining: 1, first: "pending//1"},
		{name: "gc down: the item is not handled and holds", messages: 1, down: true, remaining: 1, wantErr: true, first: "intent/retry/0"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := setup(t)
			clock := f.withClock(t)
			f.addSpace("spaces/room", true)
			var names []string
			for i := 1; i <= row.messages; i++ {
				names = append(names, f.addMessage("spaces/room", i, frozen.Add(time.Duration(i-3)*time.Minute)).Name)
			}
			if row.answer != "" {
				// The fake consumes one answer per POST.
				f.gc.Script = slices.Repeat([]wire.Delivery{{Status: row.answer}}, 8)
			}
			f.gc.CityNotRunning = row.down
			f.freeze(t)
			for pass := 1; pass <= 2; pass++ {
				writes := 0
				f.r.opts.Save = func(l *ledger.Ledger) error {
					writes++
					return l.Save(f.opts.LedgerPath)
				}
				delivered, remaining, err := f.r.Pass(context.Background())
				if (err != nil) != row.wantErr || delivered != 0 || remaining != row.remaining {
					t.Fatalf("pass %d = (%d, %d, %v), want (0, %d, error %v)", pass, delivered, remaining, err, row.remaining, row.wantErr)
				}
				s := f.opts.Ledger.Spaces["spaces/room"]
				if settled := row.remaining == 0; settled != s.MessageCursor.Equal(frozen) || settled != s.FrozenUpper.IsZero() {
					t.Fatalf("pass %d: cursor %s frozen upper %s, settled %v", pass, s.MessageCursor, s.FrozenUpper, settled)
				}
				if pass == 2 && row.remaining == 0 && writes != 0 {
					t.Fatalf("a settled interval was saved again: %d writes", writes)
				}
			}
			for i, want := range []string{row.first, row.second}[:row.messages] {
				c := f.opts.Ledger.Changes[names[i]]
				if c == nil {
					t.Fatalf("%s not recorded", names[i])
				}
				cause := c.Reason
				if c.Status == "intent" && c.Cause == "behind_earlier" {
					cause = c.Cause
				}
				if got := c.Status + "/" + cause + "/" + strconv.Itoa(c.Attempts); got != want {
					t.Fatalf("%s = %s, want %s", names[i], got, want)
				}
			}
			if row.remaining != 0 {
				return
			}
			// Poll owns the settled changes: they land when gc takes them, in
			// order, without catch-up.
			posts := len(f.gc.Inbound())
			f.gc.Script = nil
			clock.Add(time.Minute)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := f.gc.Inbound()[posts:]
			if len(got) != 2 || got[0].ProviderMessageID != names[0] || got[1].ProviderMessageID != names[1] {
				t.Fatalf("poll POSTs after settling = %d, want the two changes in order", len(got))
			}
			for _, name := range names {
				if c := f.opts.Ledger.Changes[name]; c.Status != "committed" {
					t.Fatalf("%s = %s after the poll", name, c.Status)
				}
			}
		})
	}
}
