package inbound

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// These tests drive the retry, order and raise rules with an injected clock.
// The in-gate waits of the first four POSTs advance that clock instead of
// sleeping, so RetryDelay can be one second without a real wait.

var retryStart = time.Date(2026, 11, 10, 20, 22, 58, 0, time.UTC)

const otherSpace = "spaces/other"

// draftDeferred is the shape of gc's refusal when the session's input box
// holds text it cannot prove is the message being delivered.
const draftDeferred = `nudge: preserved draft deferred delivery: target "mayor" has no matching paste prompt`

type retryClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *retryClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *retryClock) Set(t time.Time)     { c.mu.Lock(); defer c.mu.Unlock(); c.now = t }
func (c *retryClock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type raiseLog struct {
	mu    sync.Mutex
	calls []Raise
	fail  error
}

func (r *raiseLog) raise(_ context.Context, x Raise) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, x)
	return r.fail
}
func (r *raiseLog) list() []Raise     { r.mu.Lock(); defer r.mu.Unlock(); return slices.Clone(r.calls) }
func (r *raiseLog) setFail(err error) { r.mu.Lock(); defer r.mu.Unlock(); r.fail = err }

type retryFixture struct {
	*fixture
	clock  *retryClock
	raises *raiseLog
	sleeps []time.Duration
}

func retrySetup(t *testing.T, edit ...func(*Options)) *retryFixture {
	t.Helper()
	rf := &retryFixture{fixture: setup(t), clock: &retryClock{now: retryStart}, raises: &raiseLog{}}
	rf.opts.Now = rf.clock.Now
	rf.opts.RetryDelay = time.Second
	rf.opts.Raise = rf.raises.raise
	for _, e := range edit {
		e(&rf.opts)
	}
	rf.rebuild(t)
	return rf
}

// rebuild makes a processor over rf.opts, as a restart does.
func (rf *retryFixture) rebuild(t *testing.T) {
	t.Helper()
	p, err := New(rf.opts)
	if err != nil {
		t.Fatal(err)
	}
	p.wait = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rf.sleeps = append(rf.sleeps, d)
		rf.clock.Add(d)
		return nil
	}
	rf.p = p
}

func (rf *retryFixture) restart(t *testing.T) {
	t.Helper()
	l, err := ledger.Load(rf.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	rf.l, rf.opts.Ledger = l, l
	rf.raises = &raiseLog{}
	rf.opts.Raise = rf.raises.raise
	rf.rebuild(t)
}

func (rf *retryFixture) poll(t *testing.T) {
	t.Helper()
	if err := rf.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (rf *retryFixture) change(t *testing.T, id string) ledger.ChangeState {
	t.Helper()
	rf.opts.LedgerMu.Lock()
	defer rf.opts.LedgerMu.Unlock()
	c := rf.l.Changes[id]
	if c == nil {
		t.Fatalf("change %s missing", id)
	}
	return *c
}

func (rf *retryFixture) script(answers ...wire.Delivery) {
	rf.gc.Configure(func(s *fakegc.Server) { s.Script = answers })
}

func failedWith(text string) wire.Delivery {
	return wire.Delivery{Status: "failed", ExpectedBytes: 120, Members: []wire.DeliveryMember{{Status: "failed", ExpectedBytes: 120, Error: text}}}
}

func repeat(n int, d wire.Delivery, then ...wire.Delivery) []wire.Delivery {
	out := make([]wire.Delivery, 0, n+len(then))
	for range n {
		out = append(out, d)
	}
	return append(out, then...)
}

func secondMessage() *api.Message {
	m := message()
	m.Name = space + "/messages/two"
	m.Text = "second"
	m.CreateTime = "2026-10-22T10:00:05Z"
	return m
}

func otherMessage() *api.Message {
	return &api.Message{Name: otherSpace + "/messages/three", Text: "elsewhere", CreateTime: "2026-10-22T10:00:06Z",
		Space: &api.Space{Name: otherSpace, SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", DisplayName: "Reader", Type: "HUMAN"}}
}

func otherEvent(id string) *puller.Event {
	return &puller.Event{PubSubID: id, Changes: []puller.Change{{Kind: "message.created", Message: otherMessage(), Space: otherSpace}}}
}

func postedIDs(f *fixture) []string {
	var ids []string
	for _, m := range f.gc.Inbound() {
		ids = append(ids, m.ProviderMessageID)
	}
	return ids
}

// (a) A definite "not delivered" is retried past the fourth POST until it lands.
func TestFailedDeliveryIsRetriedUntilItLands(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(6, failedWith(draftDeferred), wire.Delivery{Status: "delivered"})...)
	ev := event("late-success", "message.created", message())
	handle(t, rf.fixture, ev)
	if got := len(rf.gc.Inbound()); got != 4 {
		t.Fatalf("first phase posts=%d, want 4", got)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !slices.Equal(rf.sleeps, want) {
		t.Fatalf("in-gate waits %v, want %v", rf.sleeps, want)
	}
	c := rf.change(t, messageName)
	if c.Status != "intent" || c.Reason != "retry" || c.Attempts != 4 || c.NextAttempt.IsZero() || c.Cause != "draft_deferred" || rf.l.Unresolved[messageName] != nil {
		t.Fatalf("after four failed POSTs: %+v", c)
	}
	// Neither a poll nor a Pub/Sub redelivery POSTs before next_attempt.
	rf.poll(t)
	ev.PubSubID = "redelivered"
	handle(t, rf.fixture, ev)
	if got := len(rf.gc.Inbound()); got != 4 {
		t.Fatalf("POST before next_attempt: posts=%d", got)
	}
	for post := 5; post <= 7; post++ {
		rf.clock.Set(rf.change(t, messageName).NextAttempt)
		rf.poll(t)
		if got := len(rf.gc.Inbound()); got != post {
			t.Fatalf("due poll made posts=%d, want %d", got, post)
		}
		rf.poll(t)
		if got := len(rf.gc.Inbound()); got != post {
			t.Fatalf("a second poll at the same instant posted again: %d", got)
		}
	}
	c = rf.change(t, messageName)
	if c.Status != "committed" || c.Reason != "delivered" || rf.l.Counters["turns:"+space] != 1 || rf.l.Unresolved[messageName] != nil {
		t.Fatalf("late success: %+v %v", c, rf.l.Counters)
	}
	for _, id := range postedIDs(rf.fixture) {
		if id != messageName {
			t.Fatalf("retry identity changed: %q", id)
		}
	}
}

// (b) The waits double from the fourth POST, cap at RetryCap, never overflow,
// and the retry bound ends a retrying change and one held back behind it.
func TestRetryWaitsDoubleToTheCapAndEndAtTheBound(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(12, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("waits", "message.created", message()))
	var waits []time.Duration
	for range 5 {
		c := rf.change(t, messageName)
		waits = append(waits, c.NextAttempt.Sub(rf.clock.Now()))
		rf.clock.Set(c.NextAttempt)
		rf.poll(t)
	}
	if want := []time.Duration{8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}; !slices.Equal(waits, want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	rf.opts.LedgerMu.Lock()
	rf.l.Changes[messageName].Attempts = 5000
	rf.opts.LedgerMu.Unlock()
	rf.clock.Set(rf.change(t, messageName).NextAttempt)
	rf.poll(t)
	if c := rf.change(t, messageName); c.Attempts != 5001 || c.NextAttempt.Sub(rf.clock.Now()) != time.Minute {
		t.Fatalf("5,000 attempts: %+v (wait %s)", c, c.NextAttempt.Sub(rf.clock.Now()))
	}
	second := secondMessage()
	rf.clock.Add(time.Second)
	handle(t, rf.fixture, event("second", "message.created", second))
	if c := rf.change(t, second.Name); c.Status != "intent" || c.Attempts != 0 || c.Cause != "behind_earlier" {
		t.Fatalf("held back: %+v", c)
	}
	// Just inside the bound the first still retries.
	rf.clock.Set(retryStart.Add(24*time.Hour - time.Second))
	rf.poll(t)
	posts := len(rf.gc.Inbound())
	if c := rf.change(t, messageName); c.Status != "intent" || posts != 11 {
		t.Fatalf("inside the bound: posts=%d %+v", posts, c)
	}
	rf.clock.Set(rf.change(t, second.Name).Created.Add(24 * time.Hour))
	rf.poll(t)
	for name, cause := range map[string]string{messageName: "draft_deferred", second.Name: "behind_earlier"} {
		c := rf.change(t, name)
		if c.Status != "unresolved" || c.Reason != "not delivered within the retry bound" || c.Cause != cause || rf.l.Unresolved[name] == nil {
			t.Fatalf("bound for %s: %+v", name, c)
		}
	}
	rf.clock.Add(time.Hour)
	rf.poll(t)
	if got := postedIDs(rf.fixture); len(got) != posts || slices.Contains(got, second.Name) {
		t.Fatalf("POST after the bound or of the held-back message: %v", got)
	}
}

// (c) A later message of a space waits for an unsettled earlier one; another
// space flows; the later one follows in the same pass once the first lands.
func TestSpaceOrderHoldsBehindAnUnsettledMessage(t *testing.T) {
	for _, first := range []string{"intent", "pending"} {
		t.Run(first, func(t *testing.T) {
			rf := retrySetup(t)
			if first == "intent" {
				rf.script(repeat(4, failedWith(draftDeferred))...)
			} else {
				rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			}
			handle(t, rf.fixture, event("first", "message.created", message()))
			if c := rf.change(t, messageName); c.Status != first {
				t.Fatalf("first: %+v", c)
			}
			posts := len(rf.gc.Inbound())
			second := secondMessage()
			rf.clock.Add(time.Second)
			handle(t, rf.fixture, event("second", "message.created", second))
			if got := len(rf.gc.Inbound()); got != posts {
				t.Fatalf("a later message overtook an unsettled one: %v", postedIDs(rf.fixture))
			}
			if c := rf.change(t, second.Name); c.Status != "intent" || c.Attempts != 0 || c.Cause != "behind_earlier" {
				t.Fatalf("held back: %+v", c)
			}
			handle(t, rf.fixture, otherEvent("other"))
			if got := postedIDs(rf.fixture); len(got) != posts+1 || got[posts] != otherSpace+"/messages/three" {
				t.Fatalf("another space was held back: %v", got)
			}
			if first == "intent" {
				rf.clock.Set(rf.change(t, messageName).NextAttempt)
			} else {
				r := wire.ReceiptResponse{State: "concluded"}
				r.Delivery.Status = "delivered"
				rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, r)
			}
			rf.poll(t)
			got := postedIDs(rf.fixture)
			if got[len(got)-1] != second.Name || slices.Index(got, second.Name) != len(got)-1 {
				t.Fatalf("the second did not follow in the same pass: %v", got)
			}
			for _, name := range []string{messageName, second.Name} {
				if c := rf.change(t, name); c.Status != "committed" {
					t.Fatalf("%s: %+v", name, c)
				}
			}
		})
	}
}

// (d) A retry POST without a response is ambiguous and is never reposted.
func TestAmbiguousRetryPOSTIsNeverReposted(t *testing.T) {
	rf := retrySetup(t)
	target, _ := url.Parse(rf.gc.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	var lose atomic.Bool
	var lost atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lose.Load() && r.Method == http.MethodPost {
			lost.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer s.Close()
	rf.opts.GC = gcclient.New(s.URL, "examplecity")
	rf.rebuild(t)
	rf.script(repeat(4, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("ambiguous", "message.created", message()))
	if c := rf.change(t, messageName); c.Status != "intent" || c.Attempts != 4 {
		t.Fatalf("waiting: %+v", c)
	}
	lose.Store(true)
	rf.clock.Set(rf.change(t, messageName).NextAttempt)
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != "POST response unavailable; delivery unknown" || c.Cause != "unknown" {
		t.Fatalf("lost response: %+v", c)
	}
	for range 3 {
		rf.clock.Add(time.Hour)
		rf.poll(t)
	}
	if lost.Load() != 1 || len(rf.gc.Inbound()) != 4 {
		t.Fatalf("ambiguous POST reposted: lost=%d posts=%d", lost.Load(), len(rf.gc.Inbound()))
	}
}

// (e) A stuck change is raised once; the bound does not raise it again.
func TestStuckChangeIsRaisedOnce(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(200, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("stuck", "message.created", message()))
	rf.clock.Set(retryStart.Add(2*time.Minute - time.Second))
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 0 {
		t.Fatalf("raised before StuckAfter: %+v", got)
	}
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	got := rf.raises.list()
	if len(got) != 1 || got[0].ID != messageName || got[0].Space != space || got[0].Message != messageName || got[0].Thread != space+"/threads/topic" {
		t.Fatalf("raise: %+v", got)
	}
	if c := rf.change(t, messageName); !c.Raised.Equal(retryStart.Add(2 * time.Minute)) {
		t.Fatalf("raised not recorded: %+v", c)
	}
	body := got[0].Body
	for _, want := range []string{space, space + "/threads/topic", messageName, "2026-11-10T20:22:58Z", "draft_deferred", "waiting",
		"gc gchat read --space " + space + " --thread " + space + "/threads/topic --since 2026-11-10T19:22:58Z", "gc gchat status", "2026-11-11T20:22:58Z"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
	for _, private := range []string{"hello", "Reader", "users/456", "preserved draft", "no matching paste"} {
		if strings.Contains(body, private) {
			t.Fatalf("body carries %q:\n%s", private, body)
		}
	}
	for range 10 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	if got := rf.raises.list(); len(got) != 1 {
		t.Fatalf("raised again while waiting: %d", len(got))
	}
	rf.clock.Set(retryStart.Add(24 * time.Hour))
	rf.poll(t)
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || len(rf.raises.list()) != 1 {
		t.Fatalf("raised again at the bound: %d %+v", len(rf.raises.list()), c)
	}
}

// (e) A failed mail is tried again after a minute, then marked.
func TestFailedRaiseIsRetriedAfterAMinute(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(50, failedWith(""))...)
	handle(t, rf.fixture, event("refused", "message.created", message()))
	if c := rf.change(t, messageName); c.Cause != "not_delivered" {
		t.Fatalf("cause: %+v", c)
	}
	rf.raises.setFail(errors.New("mail refused"))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 1 || !rf.change(t, messageName).Raised.IsZero() {
		t.Fatalf("failed raise: %d %+v", len(got), rf.change(t, messageName))
	}
	rf.clock.Add(59 * time.Second)
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 1 {
		t.Fatalf("raise retried within a minute: %d", len(got))
	}
	rf.raises.setFail(nil)
	rf.clock.Add(time.Second)
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 2 || rf.change(t, messageName).Raised.IsZero() {
		t.Fatalf("raise not retried after a minute: %d %+v", len(got), rf.change(t, messageName))
	}
	rf.clock.Add(time.Hour)
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 2 {
		t.Fatalf("raised after marking: %d", len(got))
	}
}

// (e)(m) A change that becomes unresolved is raised once, by the next poll
// pass; a delivery status the adapter does not know is one such change.
func TestUnknownDeliveryStatusIsUnresolvedAndRaisedOnce(t *testing.T) {
	rf := retrySetup(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "surprise" })
	handle(t, rf.fixture, event("surprise", "message.created", message()))
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != "unrecognized delivery status" || c.Cause != "unknown" {
		t.Fatalf("unknown status: %+v", c)
	}
	if got := rf.raises.list(); len(got) != 0 {
		t.Fatal("Handle raised; raising belongs to the poll pass")
	}
	rf.poll(t)
	got := rf.raises.list()
	if len(got) != 1 || got[0].ID != messageName {
		t.Fatalf("unresolved raise: %+v", got)
	}
	for _, want := range []string{"unresolved", "unknown", "unrecognized delivery status", "gc gchat resolve " + messageName + " --accept", "gc gchat resolve " + messageName + " --replay", "duplicate"} {
		if !strings.Contains(got[0].Body, want) {
			t.Fatalf("body lacks %q:\n%s", want, got[0].Body)
		}
	}
	for range 3 {
		rf.clock.Add(time.Hour)
		rf.poll(t)
	}
	if got := rf.raises.list(); len(got) != 1 || len(rf.gc.Inbound()) != 1 {
		t.Fatalf("raised or posted again: raises=%d posts=%d", len(got), len(rf.gc.Inbound()))
	}
}

// (e) A withdrawn message and an unresolved row from before causes are not raised.
func TestWithdrawnAndOldUnresolvedChangesAreNotRaised(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(4, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("withdrawn", "message.created", message()))
	handle(t, rf.fixture, event("delete", "message.deleted", &api.Message{Name: messageName}))
	old := &ledger.ChangeState{ID: space + "/messages/old", Space: space, Message: space + "/messages/old", Kind: "message.created", Status: "unresolved",
		Reason: "delivery retries exhausted", Attempts: 4, Created: retryStart.Add(-time.Hour)}
	rf.opts.LedgerMu.Lock()
	oldCopy := *old
	rf.l.Changes[old.ID], rf.l.Unresolved[old.ID] = old, &oldCopy
	rf.opts.LedgerMu.Unlock()
	rf.clock.Set(rf.change(t, messageName).NextAttempt)
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != "message deleted or superseded before retry" || c.Cause != "withdrawn" {
		t.Fatalf("withdrawn: %+v", c)
	}
	for range 3 {
		rf.clock.Add(time.Hour)
		rf.poll(t)
	}
	if got := rf.raises.list(); len(got) != 0 {
		t.Fatalf("raised a withdrawn or old change: %+v", got)
	}
}

// (e) One poll pass makes at most four raise calls; the fifth waits.
func TestAtMostFourRaisesPerPollPass(t *testing.T) {
	rf := retrySetup(t)
	rf.opts.LedgerMu.Lock()
	for i := range 5 {
		id := space + "/messages/u" + string(rune('1'+i))
		c := &ledger.ChangeState{ID: id, Space: space, Message: id, Kind: "message.created", Status: "unresolved", Reason: "stored turn is malformed", Cause: "unknown", Created: retryStart.Add(time.Duration(i) * time.Second)}
		copy := *c
		rf.l.Changes[id], rf.l.Unresolved[id] = c, &copy
	}
	rf.opts.LedgerMu.Unlock()
	rf.poll(t)
	got := rf.raises.list()
	if len(got) != 4 || got[0].ID != space+"/messages/u1" || got[3].ID != space+"/messages/u4" {
		t.Fatalf("first pass: %+v", got)
	}
	rf.poll(t)
	rf.poll(t)
	got = rf.raises.list()
	if len(got) != 5 || got[4].ID != space+"/messages/u5" {
		t.Fatalf("fifth: %+v", got)
	}
	for _, r := range got {
		if c := rf.change(t, r.ID); c.Raised.IsZero() || rf.l.Unresolved[r.ID].Raised.IsZero() {
			t.Fatalf("raised not recorded on both records of %s", r.ID)
		}
	}
}

// (f) The retry and raise state survive a restart.
func TestRetryAndRaiseStateSurviveRestart(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(5, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("first", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 1 || len(rf.gc.Inbound()) != 5 {
		t.Fatalf("before restart: raises=%d posts=%d", len(got), len(rf.gc.Inbound()))
	}
	rf.script(wire.Delivery{Status: "surprise"})
	handle(t, rf.fixture, otherEvent("other"))
	other := otherSpace + "/messages/three"
	if c := rf.change(t, other); c.Status != "unresolved" || !c.Raised.IsZero() {
		t.Fatalf("unresolved just before restart: %+v", c)
	}
	before := rf.change(t, messageName)
	posts := len(rf.gc.Inbound())
	rf.restart(t)
	after := rf.change(t, messageName)
	if after.Attempts != before.Attempts || !after.NextAttempt.Equal(before.NextAttempt) || after.Cause != before.Cause || !after.Raised.Equal(before.Raised) || after.Raised.IsZero() {
		t.Fatalf("restart lost retry state:\nbefore %+v\nafter  %+v", before, after)
	}
	rf.poll(t)
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 1 || got[0].ID != other {
		t.Fatalf("after restart: %+v", got)
	}
	if len(rf.gc.Inbound()) != posts {
		t.Fatal("POST before next_attempt after restart")
	}
	// An attempt in flight when the process stopped is never reposted.
	rf.script()
	rf.opts.Save = func(l *ledger.Ledger) error {
		if c := l.Changes[messageName]; c != nil && c.Status == "committed" {
			return errors.New("crash before terminal persistence")
		}
		return l.Save(rf.opts.LedgerPath)
	}
	rf.rebuild(t)
	rf.clock.Set(after.NextAttempt)
	if err := rf.p.Poll(context.Background()); err == nil {
		t.Fatal("crash not reported")
	}
	rf.opts.Save = nil
	rf.restart(t)
	posts = len(rf.gc.Inbound())
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != "interrupted POST; delivery unknown" || len(rf.gc.Inbound()) != posts {
		t.Fatalf("interrupted: posts=%d %+v", len(rf.gc.Inbound()), c)
	}
}

// (g) A concluded receipt whose status is pending is judged by its members.
func TestConcludedPendingReceiptIsJudged(t *testing.T) {
	member := func(status string, delivered, expected int, terminal bool) wire.DeliveryMember {
		return wire.DeliveryMember{Status: status, DeliveredBytes: delivered, ExpectedBytes: expected, Terminal: terminal}
	}
	pending := func(terminal bool, members ...wire.DeliveryMember) wire.Delivery {
		return wire.Delivery{Status: "pending", Terminal: terminal, Members: members}
	}
	const held, landed, concluded = "gc holds the delivery; operator attention", "landed_unconfirmed", "receipt concluded with pending delivery"
	for _, tc := range []struct {
		name          string
		delivery      wire.Delivery
		status, cause string
		reason        string
	}{
		{"terminal delivery", pending(true, member("pending", 120, 120, true)), "unresolved", "held", held},
		{"terminal member only", pending(false, member("delivered", 80, 80, false), member("pending", 0, 120, true)), "unresolved", "held", held},
		{"pasted whole", pending(false, member("pending", 120, 120, false)), "committed", "", landed},
		{"whole across members", pending(false, member("delivered", 80, 80, false), member("pending", 100, 100, false)), "committed", "", landed},
		{"no member rows", pending(false), "unresolved", "unknown", concluded},
		{"fewer bytes than expected", pending(false, member("pending", 60, 120, false)), "unresolved", "unknown", concluded},
		{"no bytes at all", pending(false, member("pending", 0, 0, false)), "unresolved", "unknown", concluded},
		{"a failed member", pending(false, member("pending", 120, 120, false), member("failed", 0, 120, false)), "unresolved", "unknown", concluded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			rf := retrySetup(t, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
			rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, rf.fixture, event("judged", "message.created", message()))
			rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: tc.delivery})
			rf.poll(t)
			c := rf.change(t, messageName)
			if c.Status != tc.status || c.Reason != tc.reason || c.Cause != tc.cause {
				t.Fatalf("judged: %+v", c)
			}
			turns, unconfirmed, raises := int64(0), int64(0), 1
			if tc.status == "committed" {
				turns, unconfirmed, raises = 1, 1, 0
				if n := strings.Count(logs.String(), "landed"); n != 1 || !strings.Contains(logs.String(), messageName) {
					t.Fatalf("landed log lines=%d: %s", n, logs.String())
				}
			} else if rf.l.Unresolved[messageName] == nil {
				t.Fatal("not in Unresolved")
			}
			if rf.l.Counters["turns:"+space] != turns || rf.l.Counters["unconfirmed:"+space] != unconfirmed {
				t.Fatalf("counters: %v", rf.l.Counters)
			}
			if got := rf.raises.list(); len(got) != raises {
				t.Fatalf("raises=%d, want %d", len(got), raises)
			}
			rf.clock.Add(time.Hour)
			rf.poll(t)
			if len(rf.gc.Inbound()) != 1 || len(rf.raises.list()) != raises {
				t.Fatal("judged change reposted or raised again")
			}
		})
	}
}

// (h) The pending-receipt deadline counts from the latest POST.
func TestPendingReceiptDeadlineCountsFromThePOST(t *testing.T) {
	rf := retrySetup(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.CityNotRunning = true })
	if out, err := rf.p.Handle(context.Background(), event("late-post", "message.created", message())); out != puller.Nack || err == nil {
		t.Fatalf("gc down: %v %v", out, err)
	}
	rf.gc.Configure(func(s *fakegc.Server) { s.CityNotRunning = false; s.NextDelivery = "pending" })
	posted := retryStart.Add(3 * time.Hour)
	rf.clock.Set(posted)
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "pending" || !c.Posted.Equal(posted) {
		t.Fatalf("POST three hours late: %+v", c)
	}
	rf.clock.Set(posted.Add(time.Hour - time.Second))
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "pending" {
		t.Fatalf("fresh receipt given up early: %+v", c)
	}
	rf.clock.Set(posted.Add(time.Hour))
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != "receipt pending beyond deadline" || len(rf.gc.Inbound()) != 1 {
		t.Fatalf("deadline from the POST: %+v", c)
	}
}

// (m) "held" is retried exactly as "failed" is and never ends by count.
func TestHeldIsRetriedLikeFailed(t *testing.T) {
	held := wire.Delivery{Status: "held", ExpectedBytes: 120, Members: []wire.DeliveryMember{{Status: "held", ExpectedBytes: 120}}}
	rf := retrySetup(t)
	rf.script(repeat(6, held, wire.Delivery{Status: "delivered"})...)
	handle(t, rf.fixture, event("held", "message.created", message()))
	c := rf.change(t, messageName)
	if len(rf.gc.Inbound()) != 4 || c.Status != "intent" || c.Reason != "retry" || c.Cause != "draft_deferred" || c.NextAttempt.IsZero() {
		t.Fatalf("held after four POSTs: posts=%d %+v", len(rf.gc.Inbound()), c)
	}
	for post := 5; post <= 7; post++ {
		rf.clock.Set(rf.change(t, messageName).NextAttempt)
		rf.poll(t)
		if got := len(rf.gc.Inbound()); got != post {
			t.Fatalf("posts=%d, want %d", got, post)
		}
	}
	if c := rf.change(t, messageName); c.Status != "committed" || rf.l.Unresolved[messageName] != nil {
		t.Fatalf("held then delivered: %+v", c)
	}

	t.Run("concluded receipt", func(t *testing.T) {
		rf := retrySetup(t)
		rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
		handle(t, rf.fixture, event("held-receipt", "message.created", message()))
		rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: held})
		rf.script(repeat(3, held)...)
		rf.poll(t)
		c := rf.change(t, messageName)
		if c.Status != "intent" || c.Reason != "retry" || c.Cause != "draft_deferred" || c.Attempts != 4 || c.NextAttempt.IsZero() {
			t.Fatalf("held receipt: %+v", c)
		}
	})
}

// (n) A pending change holds its space back for StuckAfter from its POST.
func TestPendingChangeHoldsItsSpaceForABoundedTime(t *testing.T) {
	rf := retrySetup(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
	handle(t, rf.fixture, event("first", "message.created", message()))
	second := secondMessage()
	rf.clock.Add(time.Second)
	handle(t, rf.fixture, event("second", "message.created", second))
	rf.clock.Set(retryStart.Add(2*time.Minute - time.Second))
	rf.poll(t)
	if got := postedIDs(rf.fixture); len(got) != 1 || len(rf.raises.list()) != 0 {
		t.Fatalf("before StuckAfter: posts=%v raises=%d", got, len(rf.raises.list()))
	}
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	if got := postedIDs(rf.fixture); len(got) != 2 || got[1] != second.Name {
		t.Fatalf("the later message was not released: %v", got)
	}
	if got := rf.raises.list(); len(got) != 1 || got[0].ID != messageName {
		t.Fatalf("the pending change was not raised: %+v", got)
	}
	if c := rf.change(t, messageName); c.Status != "pending" {
		t.Fatalf("first: %+v", c)
	}
}

func TestRetryOptionsMustBePositive(t *testing.T) {
	for name, edit := range map[string]func(*Options){
		"cap":   func(o *Options) { o.RetryCap = -time.Second },
		"bound": func(o *Options) { o.RetryBound = -time.Second },
		"stuck": func(o *Options) { o.StuckAfter = -time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			edit(&f.opts)
			if _, err := New(f.opts); err == nil {
				t.Fatal("negative duration accepted")
			}
		})
	}
}

// A wall clock set back must not park a clean retry behind a next_attempt now
// far in the future: a wait never exceeds RetryCap.
func TestClockRewindDoesNotParkARetry(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(5, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("rewind", "message.created", message()))
	next := rf.change(t, messageName).NextAttempt
	rf.clock.Set(next.Add(-time.Hour))
	rf.poll(t)
	if got := len(rf.gc.Inbound()); got != 5 {
		t.Fatalf("retry parked by a rewound clock: posts=%d", got)
	}
	rf.poll(t)
	if got := len(rf.gc.Inbound()); got != 5 {
		t.Fatalf("posted again before the new next_attempt: %d", got)
	}
}

// The mail for a change pending at gc names the receipt's deadline as well
// as the retry bound.
func TestPendingRaiseNamesTheReceiptDeadline(t *testing.T) {
	rf := retrySetup(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
	handle(t, rf.fixture, event("pending-mail", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	got := rf.raises.list()
	if len(got) != 1 {
		t.Fatalf("raises: %+v", got)
	}
	for _, want := range []string{"pending", "2026-11-10T21:22:58Z", "2026-11-11T20:22:58Z", "POSTs made: 1"} {
		if !strings.Contains(got[0].Body, want) {
			t.Fatalf("body lacks %q:\n%s", want, got[0].Body)
		}
	}
}
