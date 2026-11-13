package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

type ownerPOST struct {
	Message  wire.InboundMessage
	Answer   string
	Accepted bool
}

// ownerExercise uses real HTTP and durable ledger saves. The only fabricated
// saved state is the resume-path withdrawal: it restores the frozen delivery
// records after recording the act, matching a writer without record refresh.
func ownerExercise(t *testing.T, outcome, withdrawal, path string, catchUp bool, failures int) (*windowFixture, []ownerPOST) {
	t.Helper()
	f := windowSetup(t, "8s")
	f.disk()
	a, b := message(), message()
	a.Name, a.Text, a.CreateTime = space+"/messages/m001", "body-001", f.now.Format(time.RFC3339Nano)
	a.Thread.Name = space + "/threads/t001"
	b.Name, b.Text, b.CreateTime = space+"/messages/m002", "body-002", f.now.Add(time.Second).Format(time.RFC3339Nano)
	b.Thread.Name = space + "/threads/t002"
	b.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var posts []ownerPOST
	initial, left := true, failures
	delivered := map[string]bool{}
	var crashed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope wire.InboundEnvelope
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&envelope) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		answer := "delivered"
		if initial {
			switch outcome {
			case "partial":
				answer = "partial"
			case "refused":
				answer = "refused"
			case "answer_lost", "pre_post_crash":
				answer = "lost"
			}
		} else if left > 0 {
			left--
			answer = "failed"
		}
		if delivered[envelope.Message.ProviderMessageID] {
			answer = "no_route"
		}
		accepted := answer == "delivered" || answer == "lost"
		posts = append(posts, ownerPOST{envelope.Message, answer, accepted})
		if accepted {
			delivered[envelope.Message.ProviderMessageID] = true
		}
		if answer == "refused" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:gascity:error:city-not-found", "status": 404, "code": "city-not-found", "detail": "not_found: city not found or not running: examplecity"})
			return
		}
		if answer == "lost" {
			if outcome == "pre_post_crash" {
				crashed.Store(true)
				cancel()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(wire.InboundResponse{Delivery: wire.Delivery{Status: answer}})
	}))
	t.Cleanup(srv.Close)
	f.opts.GC = gcclient.New(srv.URL, "examplecity")
	f.p.opts.GC = f.opts.GC
	failedSave := false
	f.p.opts.Save = func(l *ledger.Ledger) error {
		if crashed.Load() {
			return errors.New("process stopped")
		}
		c := l.Changes[b.Name]
		if outcome == "unsaved" && c != nil && c.Status == "committed" && c.Reason == "delivered" {
			failedSave = true
			return errors.New("outcome save failed")
		}
		if err := l.Save(f.opts.LedgerPath); err != nil {
			return err
		}
		if c != nil && c.Reason == reasonRetry && ((outcome == "partial" && c.Fragment) || outcome == "refused") {
			cancel()
		}
		return nil
	}
	if catchUp {
		_, _ = f.p.HandleChanges(ctx, []puller.Change{{Kind: "message.created", Space: space, Message: a}, {Kind: "message.created", Space: space, Message: b}})
	} else {
		f.handle(t, event("first", "message.created", a))
		f.now = f.now.Add(time.Second)
		ev := event("owner", "message.created", b)
		ev.Changes[0].PublishTime, ev.Changes[0].EventTime = f.now, f.now
		_, _ = f.p.Handle(ctx, ev)
	}
	if outcome == "unsaved" && !failedSave {
		t.Fatal("outcome save was not exercised")
	}
	mu.Lock()
	if len(posts) != 1 || !strings.Contains(posts[0].Message.Text, a.Text) || !strings.Contains(posts[0].Message.Text, b.Text) {
		mu.Unlock()
		t.Fatal("first POST was not the two-member turn")
	}
	initial = false
	mu.Unlock()
	crashed.Store(false)
	f.disk()
	ownerReload(t, f)
	before := f.l.Clone()
	owner := before.Changes[b.Name]
	t.Logf("initial owner: status=%s reason=%q attempts=%d fragment=%v", owner.Status, owner.Reason, owner.Attempts, owner.Fragment)
	if (outcome == "unsaved" || outcome == "pre_post_crash") && (owner.Status != "intent" || owner.Attempts != 1 || owner.Reason != "") {
		t.Fatal("missing saved ambiguous attempt")
	}
	if outcome == "partial" && (owner.Attempts != 1 || !owner.Fragment || owner.Reason != reasonRetry) {
		t.Fatal("missing saved fragment")
	}
	target, kind := b, "message.deleted"
	if withdrawal == "member_deleted" {
		target = a
	}
	if withdrawal == "owner_edited" {
		kind = "message.updated"
	}
	f.now = f.now.Add(time.Second)
	target.Text, target.LastUpdateTime, target.Annotations = "current-text", f.now.Format(time.RFC3339Nano), nil
	ev := event("withdraw", kind, target)
	ev.Changes[0].PublishTime, ev.Changes[0].EventTime = f.now, f.now
	// Save an older writer's shape before any admission can deliver it. The
	// withdrawal and message state are durable, but its turn remains frozen.
	if path == "resume" {
		injected := false
		f.p.opts.Save = func(l *ledger.Ledger) error {
			if !injected {
				injected = true
				for id, c := range before.Changes {
					copy := *c
					l.Changes[id] = &copy
					delete(l.Unresolved, id)
				}
				for id, c := range before.Unresolved {
					copy := *c
					l.Unresolved[id] = &copy
				}
			}
			return l.Save(f.opts.LedgerPath)
		}
	}
	if catchUp {
		_, _ = f.p.HandleChanges(context.Background(), ev.Changes)
	} else {
		_, _ = f.p.Handle(context.Background(), ev)
	}
	f.disk()
	ownerReload(t, f)
	for i := 0; i < 6; i++ {
		f.now = f.now.Add(10 * time.Minute)
		_ = f.p.Poll(context.Background())
	}
	ownerReload(t, f)
	mu.Lock()
	result := append([]ownerPOST(nil), posts...)
	mu.Unlock()
	for i, p := range result {
		t.Logf("POST %d answer=%s accepted=%v id=%s text=%q", i, p.Answer, p.Accepted, p.Message.ProviderMessageID, p.Message.Text)
	}
	ids := make([]string, 0, len(f.l.Changes))
	for id := range f.l.Changes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := f.l.Changes[id]
		t.Logf("END id=%s status=%s delivery_status=%s reason=%q cause=%q attempts=%d fragment=%v replayable=%v member_of=%s members=%v", id, c.Status, f.l.Delivery(c).Status, c.Reason, c.Cause, c.Attempts, c.Fragment, c.Event != "", c.MemberOf, c.Members)
	}
	t.Logf("MAILS %d", len(f.mails))
	return f, result
}

func ownerReload(t *testing.T, f *windowFixture) {
	t.Helper()
	l, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.l, f.opts.Ledger = l, l
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOwnerChangeKeepsDeliveryState(t *testing.T) {
	for _, outcome := range []string{"unsaved", "partial"} {
		for _, catchUp := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/catchup=%v", outcome, catchUp), func(t *testing.T) {
				f, posts := ownerExercise(t, outcome, "owner_deleted", "record", catchUp, 7)
				count, accepted := 0, 0
				for _, p := range posts {
					if strings.Contains(p.Message.Text, "body-001") {
						count++
						if p.Accepted {
							accepted++
						}
					}
				}
				want := 1
				reason := "interrupted POST; delivery unknown"
				attempts := 1
				if outcome == "partial" {
					want = 0
					reason = reasonExhausted
					attempts = firstPOSTs
				}
				if accepted != want || count > firstPOSTs {
					t.Errorf("survivor POSTs=%d accepted=%d want accepted=%d, at most %d POSTs", count, accepted, want, firstPOSTs)
				}
				a, b := f.l.Changes[space+"/messages/m001"], f.l.Changes[space+"/messages/m002"]
				if a.Status != "unresolved" || a.Reason != reason || a.Attempts != attempts || a.Fragment != (outcome == "partial") || a.Event == "" {
					t.Errorf("survivor state: %+v", a)
				}
				if b.Status != "unresolved" || b.Reason != reasonWithdrawn || b.Attempts != 1 || b.Fragment != (outcome == "partial") || b.Event != "" {
					t.Errorf("retired owner state: %+v", b)
				}
			})
		}
	}
}
