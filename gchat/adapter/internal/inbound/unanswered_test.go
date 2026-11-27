package inbound

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
)

const lostAnswer = "POST response unavailable; delivery unknown"

// UNPROVEN: the fake models /health on the same process as the claim and
// treats an inbound receipt as a sign that the process claims by message ID.
func healthAnswer(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }
}

func signedProcessor(t *testing.T) *retryFixture {
	t.Helper()
	rf := retrySetup(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.Health = healthAnswer(`{"uptime_sec":3600}`) })
	handle(t, rf.fixture, otherEvent("sign"))
	rf.clock.Add(time.Minute)
	return rf
}

func postSteps(rf *retryFixture, steps ...fakegc.PostStep) {
	rf.gc.Configure(func(s *fakegc.Server) { s.Posts = steps })
}

func checkEnd(t *testing.T, rf *retryFixture, reason string, attempts int) {
	t.Helper()
	c := rf.change(t, messageName)
	if c.Status != "unresolved" || c.Cause != "unknown" || c.Reason != reason || c.Attempts != attempts {
		t.Fatalf("end = %s/%s/%q after %d posts, want unknown/%q after %d", c.Status, c.Cause, c.Reason, c.Attempts, reason, attempts)
	}
	rf.raises.setFail(errors.New("wake unavailable"))
	rf.poll(t)
	if c = rf.change(t, messageName); !c.RaisedEnd.IsZero() {
		t.Fatal("marked before the wake was requested")
	}
	rf.raises.setFail(nil)
	rf.clock.Add(time.Minute)
	rf.poll(t)
	rf.clock.Add(time.Hour)
	rf.poll(t)
	got := rf.raises.list()
	c = rf.change(t, messageName)
	if len(got) != 2 || !got[0].Ended || !got[1].Ended || c.RaisedEnd.IsZero() || c.Status != "unresolved" || c.Attempts != attempts {
		t.Fatalf("after one failed try and one success: notices=%d, end marked=%t, state=%s, attempts=%d", len(got), !c.RaisedEnd.IsZero(), c.Status, c.Attempts)
	}
	if n := rf.gc.Fanouts(messageName); n > 1 {
		t.Fatalf("fan-outs = %d", n)
	}
}

func TestUnansweredPostOneTurn(t *testing.T) {
	for _, status := range []string{"delivered", "failed"} {
		t.Run("pending_then_"+status, func(t *testing.T) {
			rf := signedProcessor(t)
			rf.script(wire.Delivery{Status: "pending"})
			postSteps(rf, fakegc.PostStep{Drop: true})
			handle(t, rf.fixture, event("lost", "message.created", message()))
			c := rf.change(t, messageName)
			if c.Status != "pending" || c.ReceiptID == "" || c.Attempts != 2 {
				t.Fatalf("after re-post: state=%s attempts=%d receipt=%q", c.Status, c.Attempts, c.ReceiptID)
			}
			rf.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: status}})
			if err := rf.p.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			if status == "failed" {
				checkEnd(t, rf, "re-post not confirmed (failed); delivery unknown", 2)
			} else {
				rf.clock.Add(time.Hour)
				rf.poll(t)
				c = rf.change(t, messageName)
				if c.Status != "committed" || c.Reason != "delivered" || len(rf.raises.list()) != 0 {
					t.Fatalf("state=%s reason=%s notices=%d", c.Status, c.Reason, len(rf.raises.list()))
				}
			}
			if n := rf.gc.Fanouts(messageName); n != 1 {
				t.Fatalf("fan-outs=%d", n)
			}
		})
	}
	t.Run("unclaimed_after_disconnect", func(t *testing.T) {
		rf := signedProcessor(t)
		postSteps(rf, fakegc.PostStep{HTTPStatus: 500, Drop: true})
		handle(t, rf.fixture, event("lost", "message.created", message()))
		rf.clock.Add(time.Hour)
		rf.poll(t)
		c := rf.change(t, messageName)
		if c.Status != "committed" || c.Reason != "delivered" || c.Attempts != 2 || rf.gc.Fanouts(messageName) != 1 || len(rf.raises.list()) != 0 {
			t.Fatalf("state=%s reason=%s attempts=%d fan-outs=%d notices=%d", c.Status, c.Reason, c.Attempts, rf.gc.Fanouts(messageName), len(rf.raises.list()))
		}
	})
	t.Run("gc_restarted_after_delivery", func(t *testing.T) {
		rf := signedProcessor(t)
		postSteps(rf, fakegc.PostStep{Drop: true, After: func(*httptest.ResponseRecorder) {
			rf.gc.Restart(healthAnswer(`{"uptime_sec":1}`))
		}})
		handle(t, rf.fixture, event("lost", "message.created", message()))
		checkEnd(t, rf, lostAnswer, 1)
	})
	for _, kind := range []string{"first", "old_answer_replaces_sign", "processor_rebuilt"} {
		t.Run(kind, func(t *testing.T) {
			rf := retrySetup(t)
			rf.gc.Configure(func(s *fakegc.Server) { s.Health = healthAnswer(`{"uptime_sec":86400}`) })
			if kind != "first" {
				handle(t, rf.fixture, otherEvent("sign"))
				if kind == "processor_rebuilt" {
					rf.restart(t)
				} else {
					postSteps(rf, fakegc.PostStep{After: func(out *httptest.ResponseRecorder) {
						out.Body.Reset()
						out.Body.WriteString(`{"Message":{},"TargetSessionID":"example"}`)
					}})
					handle(t, rf.fixture, event("old", "message.created", secondMessage()))
				}
			}
			postSteps(rf, fakegc.PostStep{Drop: true})
			handle(t, rf.fixture, event("lost", "message.created", message()))
			checkEnd(t, rf, lostAnswer, 1)
		})
	}
	for name, body := range map[string]string{"absent": "", "missing_age": "{}", "zero": `{"uptime_sec":0}`, "fraction": `{"uptime_sec":3.5}`, "younger_than_sign": `{"uptime_sec":30}`, "within_post_margin": `{"uptime_sec":3}`} {
		t.Run("health_"+name, func(t *testing.T) {
			rf := signedProcessor(t)
			rf.gc.Configure(func(s *fakegc.Server) {
				if body == "" {
					s.Health = nil
				} else {
					s.Health = healthAnswer(body)
				}
			})
			postSteps(rf, fakegc.PostStep{Drop: true})
			handle(t, rf.fixture, event("lost", "message.created", message()))
			checkEnd(t, rf, lostAnswer, 1)
		})
	}
}

func TestUnansweredPostEnds(t *testing.T) {
	for _, word := range []string{"no_route", "failed", "held", "partial", "http_500", "no_answer"} {
		t.Run(word, func(t *testing.T) {
			rf := signedProcessor(t)
			second := fakegc.PostStep{}
			switch word {
			case "no_answer":
				second.Drop = true
			case "http_500":
				second.HTTPStatus = 500
			default:
				rf.script(wire.Delivery{Status: word, Members: []wire.DeliveryMember{{Status: "partial"}}})
			}
			postSteps(rf, fakegc.PostStep{HTTPStatus: 500, Drop: true}, second)
			handle(t, rf.fixture, event("lost", "message.created", message()))
			checkEnd(t, rf, "re-post not confirmed ("+word+"); delivery unknown", 2)
			handle(t, rf.fixture, event("again", "message.created", message()))
			if c := rf.change(t, messageName); c.Attempts != 2 || c.Fragment != (word == "partial" || word == "held") || rf.l.Counters["no_route:"+space] != 0 {
				t.Fatalf("attempts=%d fragment=%t no_route=%d", c.Attempts, c.Fragment, rf.l.Counters["no_route:"+space])
			}
		})
	}
}

func replaySnapshot(t *testing.T, attempted, legacy bool) *retryFixture {
	t.Helper()
	rf := signedProcessor(t)
	postSteps(rf, fakegc.PostStep{Drop: true})
	rf.gc.Configure(func(s *fakegc.Server) { s.Health = nil })
	handle(t, rf.fixture, event("lost", "message.created", message()))
	rf.poll(t)
	rf.clock.Add(time.Hour)
	err := rf.p.update(func(l *ledger.Ledger) {
		c := l.Unresolved[messageName]
		c.Status, c.Created, c.Reason, c.Cause = "intent", rf.clock.Now(), "operator replay", ""
		c.Attempts, c.Posted, c.Fragment, c.ReceiptID = 0, time.Time{}, false, ""
		if !legacy {
			c.Raised, c.RaisedEnd = time.Time{}, time.Time{}
		}
		if attempted {
			c.Attempts, c.Reason, c.Posted = 1, "", rf.clock.Now()
		}
		l.Changes[c.ID] = c
		delete(l.Unresolved, c.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	rf.restart(t)
	postSteps(rf, fakegc.PostStep{Drop: true})
	return rf
}

func checkReplaySnapshot(t *testing.T, attempted, legacy bool) {
	t.Helper()
	rf := replaySnapshot(t, attempted, legacy)
	before := len(rf.gc.Inbound())
	rf.poll(t)
	rf.clock.Add(time.Hour)
	rf.poll(t)
	posts, reason := 1, lostAnswer
	if attempted {
		posts, reason = 0, "interrupted POST; delivery unknown"
	}
	wantMails := 1
	if legacy {
		wantMails = 0
	}
	c := rf.change(t, messageName)
	if n := len(rf.gc.Inbound()) - before; n != posts || c.Status != "unresolved" || c.Cause != "unknown" || c.Reason != reason || len(rf.raises.list()) != wantMails {
		t.Fatalf("posts=%d end=%s/%s/%q notices=%d", n, c.Status, c.Cause, c.Reason, len(rf.raises.list()))
	}
}

// The first legacy exception is by design: an older replay kept an end mark.
func TestUnansweredPostLegacyExceptionInterruptedReplay(t *testing.T) {
	checkReplaySnapshot(t, true, true)
}

// The second legacy exception is by design: an older intent saved before
// its first POST can also carry a preceding life's end mark.
func TestUnansweredPostSecondLegacyExceptionSavedIntent(t *testing.T) {
	checkReplaySnapshot(t, false, true)
}

func TestUnansweredPostCurrentReplayMarks(t *testing.T) {
	checkReplaySnapshot(t, true, false)
	checkReplaySnapshot(t, false, false)
}

func TestUnansweredPostOrder(t *testing.T) {
	rf := signedProcessor(t)
	open, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	postSteps(rf, fakegc.PostStep{HTTPStatus: 500, Drop: true}, fakegc.PostStep{After: func(*httptest.ResponseRecorder) { close(open); <-release }})
	done := make(chan error, 3)
	go func() { _, err := rf.p.Handle(t.Context(), event("lost", "message.created", message())); done <- err }()
	select {
	case <-open:
	case <-done:
		t.Fatal("second POST never started")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out before re-post")
	}
	go func() {
		_, err := rf.p.Handle(t.Context(), event("second", "message.created", secondMessage()))
		done <- err
	}()
	// The gate still belongs to the same handler until the re-post returns.
	if got := postedIDs(rf.fixture); !slices.Equal(got[1:], []string{messageName, messageName}) {
		t.Fatalf("while open: %v", got)
	}
	release <- struct{}{}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out")
		}
	}
	if got := postedIDs(rf.fixture); !slices.Equal(got[1:], []string{messageName, messageName, secondMessage().Name}) {
		t.Fatalf("post order: %v", got)
	}
}

func TestUnansweredPostDurableAttempts(t *testing.T) {
	rf := signedProcessor(t)
	type saved struct {
		Status, Reason, Cause, Receipt string
		Attempts                       int
	}
	var rows []saved
	rf.p.opts.Save = func(l *ledger.Ledger) error {
		if c := l.Changes[messageName]; c != nil {
			s := saved{c.Status, c.Reason, c.Cause, c.ReceiptID, c.Attempts}
			if len(rows) == 0 || rows[len(rows)-1] != s {
				rows = append(rows, s)
			}
		}
		return l.Save(rf.opts.LedgerPath)
	}
	postSteps(rf, fakegc.PostStep{HTTPStatus: 500, Drop: true}, fakegc.PostStep{Drop: true})
	handle(t, rf.fixture, event("lost", "message.created", message()))
	want := []saved{{Status: "intent"}, {Status: "intent", Attempts: 1}, {Status: "intent", Attempts: 2}, {Status: "unresolved", Reason: "re-post not confirmed (no_answer); delivery unknown", Cause: "unknown", Attempts: 2}}
	if !slices.Equal(rows, want) {
		t.Fatalf("saves=%+v", rows)
	}
}
