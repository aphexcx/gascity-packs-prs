package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

const lookupRoom = "spaces/example"
const lookupParent = lookupRoom + "/messages/parent"

type credentialFixture struct {
	*fixture
	logs                                     logBuffer
	notices, directoryGET, parentGET, denied atomic.Int32
	refreshes                                func() int32
	identity                                 string
	reference                                *fakegc.Server
}

// UNPROVEN: a bare live actor may need a message or membership GET. The
// issuer, Chat, directory, event stream and receiving service are all local.
func newCredentialFixture(t *testing.T, managed, knownParent bool, denyPath string) *credentialFixture {
	t.Helper()
	f := &credentialFixture{reference: fakegc.New(t)}
	if managed {
		var k *fakeKeyholder
		f.fixture, k = managedFixture(t)
		f.refreshes = k.mints.Load
		f.identity = "users/123456"
		// Only the first mint succeeds. The next, after explicit expiry, is final.
		target, _ := url.Parse(f.cfg.Endpoints.PeopleAPIBase)
		search := httputil.NewSingleHostReverseProxy(target)
		directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/people:searchDirectoryPeople" {
				k.code.Store("grant_rejected")
				k.status.Store(http.StatusBadRequest)
				search.ServeHTTP(w, r)
				return
			}
			f.directoryGET.Add(1)
			if denyPath == "directory" {
				f.denied.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(directory.Close)
		f.cfg.Endpoints.PeopleAPIBase = directory.URL
	} else {
		f.fixture = newFixture(t, false)
		f.identity = fakegoogle.UserID
		f.replaceIdentityToken(t, f.identity)
		var refresh atomic.Int32
		f.refreshes = refresh.Load
		issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			refresh.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}))
		t.Cleanup(issuer.Close)
		f.cfg.Endpoints.OAuthTokenURL = issuer.URL
		dir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.directoryGET.Add(1)
			http.NotFound(w, r)
		}))
		t.Cleanup(dir.Close)
		f.cfg.Endpoints.PeopleAPIBase = dir.URL
	}
	target, _ := url.Parse(f.cfg.Endpoints.ChatAPIBase)
	chatProxy := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/spaces/expiry/messages", denyPath:
			f.denied.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":401}}`)
			return
		case "/v1/" + lookupParent:
			f.parentGET.Add(1)
			if r.Header.Get("Authorization") != "Bearer example" {
				t.Error("parent read did not use the original bearer")
			}
		}
		chatProxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.cfg.Endpoints.ChatAPIBase = srv.URL
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces[lookupRoom] = &api.Space{Name: lookupRoom, SpaceType: "DIRECT_MESSAGE"}
	})
	if knownParent {
		l := ledger.New()
		l.IdentityUserID = f.identity
		l.Messages[lookupParent] = &ledger.MessageState{Mine: true}
		if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
			t.Fatal(err)
		}
	}
	f.startOptions(t, Options{Logger: slog.New(slog.NewTextHandler(&f.logs, nil)), Notify: func(context.Context, string, string) error { f.notices.Add(1); return nil }})
	f.health(t, "ready", 10*time.Second)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Messages[lookupParent] = &api.Message{Name: lookupParent, Sender: &api.User{Name: f.identity}}
	})
	return f
}

func (f *credentialFixture) expire(t *testing.T) {
	t.Helper()
	resp, err := f.client.Get("http://adapter/read?space=spaces/expiry")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || f.denied.Load() != 1 {
		t.Fatalf("required read did not expire the original bearer: HTTP %d", resp.StatusCode)
	}
}

func (f *credentialFixture) change(t *testing.T, source, label, user string) {
	t.Helper()
	before, _ := historyState(t, f.fixture)
	u := &api.User{Name: user, Type: "HUMAN"}
	// The directory-only control skips the sender fill by providing both fields.
	if source == "directory" || source == "parent" {
		u.DisplayName, u.DomainId = "Example Reader", "example-org"
	}
	c := puller.Change{Space: lookupRoom, PublishTime: time.Now()}
	var id string
	switch source {
	case "member", "parent":
		c.Kind = "reaction.created"
		c.Reaction = &api.Reaction{Name: lookupParent + "/reactions/" + label, User: u, Emoji: &api.Emoji{Unicode: "+"}}
		id = c.Reaction.Name + "#created"
		data, err := json.Marshal(map[string]any{"reaction": c.Reaction})
		if err != nil {
			t.Fatal(err)
		}
		msg := &pubsub.Message{Data: data, Attributes: map[string]string{"ce-type": "google.workspace.chat.reaction.v1.created", "ce-id": id}}
		if _, err = f.pub.Publish(t.Context(), msg).Get(t.Context()); err != nil {
			t.Fatal(err)
		}
	default:
		c.Kind = "message.created"
		c.Message = historyMessage(lookupRoom, label, c.PublishTime)
		c.Message.Space = &api.Space{Name: lookupRoom, SpaceType: "DIRECT_MESSAGE"}
		c.Message.Sender = u
		id = c.Message.Name
		publishHistoryMessage(t, f.fixture, c.Message)
	}
	waitFor(t, 6*time.Second, func() bool {
		l, _ := historyState(t, f.fixture)
		return (l.Changes[id] != nil && l.Changes[id].Status == "committed") || f.notices.Load() != 0
	})
	rows, _ := historyState(t, f.fixture)
	got := rows.Changes[id]
	if got == nil || got.Status != "committed" || f.notices.Load() != 0 {
		t.Fatalf("optional lookup interrupted delivery: row present=%t notices=%d", got != nil, f.notices.Load())
	}
	turns := f.gc.Inbound()
	controls := len(f.reference.Inbound())
	if len(turns) != controls+1 {
		t.Fatalf("got %d turns for %d events", len(turns), controls+1)
	}
	// Replay the identical change through the original no-fill processor with
	// the same receipt sequence and receive time. The two processes have
	// independent disk/post clocks; compare those fields by their ordering.
	turn := turns[controls]
	endpoints := f.cfg.Endpoints
	endpoints.ChatAPIBase = f.google.URL()
	refChat, err := chat.New(t.Context(), http.DefaultClient, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := inbound.New(inbound.Options{
		Config: f.cfg, Ledger: before, LedgerMu: new(sync.Mutex),
		Save: func(*ledger.Ledger) error { return nil },
		GC:   gcclient.New(f.reference.URL(), f.cfg.Secrets.CityName),
		Chat: refChat, IdentityUserID: f.identity, Now: func() time.Time { return turn.ReceivedAt },
	})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := proc.Handle(t.Context(), &puller.Event{Changes: []puller.Change{c}}); err != nil || out != puller.Ack {
		t.Fatalf("control: %v %v", out, err)
	}
	want := before.Changes[id]
	if want == nil || len(f.reference.Inbound()) != len(turns) {
		t.Fatal("no control delivery")
	}
	if got.Created.Before(turn.ReceivedAt) || got.Posted.Before(got.Created) || got.NoticeAt.Before(got.Posted) {
		t.Fatal("change timestamps out of order")
	}
	want.Created, want.Posted, want.NoticeAt = got.Created, got.Posted, got.NoticeAt
	if *got != *want {
		t.Fatalf("fallback differs from the no-fill control: got %+v want %+v", got, want)
	}
	expected, err := json.Marshal(f.reference.Inbound()[controls])
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(turn)
	if err != nil || string(body) != string(expected) {
		t.Fatal("wire turn differs from no-fill control")
	}
}

func TestSenderFillCredentialRefusal(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, source := range []string{"message", "member", "directory"} {
			t.Run(fmt.Sprintf("managed_%t/%s", managed, source), func(t *testing.T) {
				f := newCredentialFixture(t, managed, true, "")
				f.expire(t)
				base := f.refreshes()
				for i, id := range []string{"users/1", "users/2", "users/1"} {
					f.change(t, source, fmt.Sprintf("event%d", i), id)
				}
				f.health(t, "ready", time.Second)
				lookups := int32(2)
				warning := "source=" + source + " cause=credential"
				if source == "directory" {
					warning = `msg="directory lookup incomplete" cause=http_400`
					if !managed {
						lookups = 0
					}
				} else if managed {
					lookups += 2
				} // directory attempts for the same two users
				if got := f.refreshes() - base; got != lookups {
					t.Errorf("refresh attempts=%d want %d", got, lookups)
				}
				if source == "directory" && !managed {
					if f.directoryGET.Load() != 0 || strings.Contains(f.logs.String(), "directory lookup incomplete") {
						t.Error("desktop made a directory lookup or warned")
					}
				} else if strings.Count(f.logs.String(), warning) != 1 {
					t.Errorf("expected one %q warning: %s", warning, f.logs.String())
				}
				// The required send owns the first permanent credential hold.
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://adapter/publish", strings.NewReader(`{"idempotency_key":"required","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"default","conversation_id":"spaces/example","kind":"dm"},"text":"example"}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				beforeSend := f.refreshes()
				resp, err := f.client.Do(req)
				// A required send can outlast this test request while entering its hold.
				if err != nil && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
				if resp != nil {
					resp.Body.Close()
					if resp.StatusCode < 400 {
						t.Fatal("required send succeeded with a refused credential")
					}
				}
				f.health(t, "needs-sign-in", 5*time.Second)
				if f.refreshes() != beforeSend+1 {
					t.Fatal("required send did not encounter the permanent refusal")
				}
				waitFor(t, 3*time.Second, func() bool { return f.notices.Load() == 1 })
			})
		}
	}
}

// UNPROVEN: Google may refuse a resource GET with 401 while accepting the
// same bearer for a required parent read. Each endpoint here is a fake.
func TestSenderFillResourceUnauthorized(t *testing.T) {
	for _, row := range []struct{ source, path, warn string }{
		{"member", "/v1/" + lookupRoom + "/members/1", "source=member cause=http_401"},
		{"message", "/v1/" + lookupRoom + "/messages/event", "source=message cause=http_401"},
		{"directory", "directory", `msg="directory lookup incomplete" cause=http_401`},
	} {
		t.Run(row.source, func(t *testing.T) {
			f := newCredentialFixture(t, row.source == "directory", false, row.path)
			base := f.refreshes()
			f.change(t, row.source, "event", "users/1")
			if row.source != "member" {
				f.change(t, "parent", "one", "users/1")
			}
			f.health(t, "ready", time.Second)
			if f.denied.Load() != 1 || f.parentGET.Load() != 1 || f.refreshes() != base || f.notices.Load() != 0 || strings.Count(f.logs.String(), row.warn) != 1 {
				t.Fatalf("denied=%d parent reads=%d refreshes=%d notices=%d logs=%s", f.denied.Load(), f.parentGET.Load(), f.refreshes()-base, f.notices.Load(), f.logs.String())
			}
		})
	}
}
