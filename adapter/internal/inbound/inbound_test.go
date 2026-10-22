package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

const space = "spaces/room"
const messageName = space + "/messages/one"
const identity = "users/123"

type fixture struct {
	p      *Processor
	l      *ledger.Ledger
	gc     *fakegc.Server
	google *fakegoogle.Server
	opts   Options
}

func setup(t *testing.T) *fixture {
	t.Helper()
	g := fakegoogle.New(t, nil)
	f := fakegc.New(t)
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity"}, Endpoints: config.Endpoints{
		ChatAPIBase: g.URL(), MediaDir: filepath.Join(dir, "media"), MediaMaxBytes: 1024, StateFile: filepath.Join(dir, "state.json"),
	}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
	c, err := chat.New(context.Background(), http.DefaultClient, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	l := ledger.New()
	g.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = &api.Space{Name: space, SpaceType: "SPACE"} })
	opts := Options{Config: cfg, Ledger: l, LedgerMu: &sync.Mutex{}, LedgerPath: cfg.Endpoints.StateFile,
		GC: gcclient.New(f.URL(), "examplecity"), Chat: c, IdentityUserID: identity,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryDelay: time.Millisecond}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{p: p, l: l, gc: f, google: g, opts: opts}
}

func message() *api.Message {
	return &api.Message{Name: messageName, Text: "hello", CreateTime: "2026-10-22T10:00:00Z",
		Space: &api.Space{Name: space, SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", DisplayName: "Reader", Type: "HUMAN"},
		Thread: &api.Thread{Name: space + "/threads/topic"}}
}

func event(id, kind string, m *api.Message) *puller.Event {
	return &puller.Event{PubSubID: id, Changes: []puller.Change{{Kind: kind, Message: m, Space: space}}}
}

func handle(t *testing.T, f *fixture, ev *puller.Event) {
	t.Helper()
	out, err := f.p.Handle(context.Background(), ev)
	if out != puller.Ack || err != nil {
		t.Fatalf("Handle = %v, %v", out, err)
	}
}

func TestWakePolicyMappingAndDedup(t *testing.T) {
	for _, tc := range []struct {
		name, kind, wake  string
		mention, own, bot bool
		want              int
	}{
		{"dm", "DIRECT_MESSAGE", "mentions", false, false, false, 1},
		{"room_all", "SPACE", "all", false, false, false, 1},
		{"mention", "SPACE", "mentions", true, false, false, 1},
		{"ambient", "SPACE", "mentions", false, false, false, 0},
		{"own", "SPACE", "all", false, true, false, 0},
		{"bot", "SPACE", "all", false, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.opts.Config.Settings.Defaults.Wake = config.WakePolicy(tc.wake)
			m := message()
			m.Space.SpaceType = tc.kind
			if tc.own {
				m.Sender.Name = identity
			}
			if tc.bot {
				m.Sender.Type = "BOT"
			}
			if tc.mention {
				m.Text = "hello <users/123>"
				m.Annotations = []*api.Annotation{{Type: "USER_MENTION", StartIndex: 6, Length: 11, UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity, DisplayName: "Example"}}}}
			}
			ev := event("p1", "message.created", m)
			handle(t, f, ev)
			handle(t, f, ev)
			ev.PubSubID = "p2"
			handle(t, f, ev)
			got := f.gc.Inbound()
			if len(got) != tc.want || f.l.Counters["turns:"+space] != int64(tc.want) {
				t.Fatalf("posts=%d, counters=%v", len(got), f.l.Counters)
			}
			if tc.want == 0 {
				if f.l.Changes[messageName] == nil {
					t.Fatal("local change not retained")
				}
				return
			}
			w := got[0]
			kind := "room"
			if tc.kind == "DIRECT_MESSAGE" {
				kind = "dm"
			}
			if w.Conversation != (wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: space, Kind: kind}) ||
				w.ProviderMessageID != messageName || w.ReplyToMessageID != m.Thread.Name || w.ReceivedAt.IsZero() || w.Actor.IsBot != tc.bot || w.Actor.DisplayName != "Reader" {
				t.Fatalf("mapping: %+v", w)
			}
			if tc.mention && w.Text != "hello Example" {
				t.Fatalf("mention: %q", w.Text)
			}
		})
	}
}

func TestRevisionsEditsDeletesAndUpdateBeforeCreate(t *testing.T) {
	f := setup(t)
	m := message()
	handle(t, f, event("create", "message.created", m))
	m.Text = "changed"
	m.LastUpdateTime = "2026-10-22T10:01:00Z"
	handle(t, f, event("edit", "message.updated", m))
	m.Text = "stale"
	m.LastUpdateTime = "2026-10-22T10:00:30Z"
	handle(t, f, event("stale", "message.updated", m))
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: messageName}))
	m.LastUpdateTime = "2026-10-22T10:02:00Z"
	handle(t, f, event("resurrection", "message.updated", m))
	got := f.gc.Inbound()
	if len(got) != 3 || got[1].ProviderMessageID != messageName+"#updated#2026-10-22T10:01:00Z" || got[1].Text != "edited: changed" ||
		got[2].ProviderMessageID != messageName+"#deleted" || got[2].Text != "deleted" || got[2].ReplyToMessageID != m.Thread.Name || !f.l.Messages[messageName].Deleted || f.l.Counters["turns:"+space] != 3 {
		t.Fatalf("revisions: %+v, %+v", got, f.l.Messages[messageName])
	}

	f = setup(t)
	m = message()
	m.LastUpdateTime = "2026-10-22T10:01:00Z"
	handle(t, f, event("early_update", "message.updated", m))
	handle(t, f, event("late_create", "message.created", message()))
	got = f.gc.Inbound()
	if len(got) != 1 || got[0].ProviderMessageID != messageName || got[0].Text != "hello" {
		t.Fatalf("update before create: %+v", got)
	}
}

func TestFollowupStillWakesWhenMentionRemoved(t *testing.T) {
	f := setup(t)
	m := message()
	handle(t, f, event("create", "message.created", m))
	f.l.Spaces[space].Wake = "mentions"
	m.LastUpdateTime = "2026-10-22T10:01:00Z"
	handle(t, f, event("edit", "message.updated", m))
	if len(f.gc.Inbound()) != 2 {
		t.Fatal("edit to delivered message filtered")
	}
}

func TestReactionsAndMembership(t *testing.T) {
	f := setup(t)
	m := message()
	m.Sender.Name = identity
	f.google.Configure(func(s *fakegoogle.Server) { s.Messages[m.Name] = m })
	for i, user := range []string{"users/456", "users/789"} {
		r := &api.Reaction{Name: messageName + "/reactions/" + user[6:], User: &api.User{Name: user}, Emoji: &api.Emoji{Unicode: "👍"}}
		handle(t, f, &puller.Event{PubSubID: user, Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: r}}})
		if i == 0 {
			handle(t, f, &puller.Event{PubSubID: "removed", Changes: []puller.Change{{Kind: "reaction.deleted", Space: space, Reaction: &api.Reaction{Name: r.Name}}}})
		}
	}
	got := f.gc.Inbound()
	if len(got) != 3 || got[0].ProviderMessageID != messageName+"/reactions/456#created" || got[1].ProviderMessageID != messageName+"/reactions/456#deleted" || got[2].Actor.ID != "users/789" || !strings.Contains(got[0].Text, "👍") || !strings.Contains(got[0].Text, messageName) || got[0].ReplyToMessageID != m.Thread.Name {
		t.Fatalf("reactions: %+v", got)
	}
	f.l.Messages[messageName].Mine = false
	handle(t, f, &puller.Event{PubSubID: "local", Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: messageName + "/reactions/local", User: &api.User{Name: identity}, Emoji: &api.Emoji{Unicode: "👍"}}}}})
	if len(f.gc.Inbound()) != 3 || f.l.Changes[messageName+"/reactions/local#created"] == nil {
		t.Fatal("reaction on another message must be local")
	}
	f.opts.Config.Settings.Defaults.Wake = config.WakeMentions
	handle(t, f, &puller.Event{PubSubID: "joined", Changes: []puller.Change{{Kind: "membership.created", Space: "spaces/new", Membership: &api.Membership{Name: "spaces/new/members/123", Member: &api.User{Name: identity}, State: "JOINED"}}}})
	if s := f.l.Spaces["spaces/new"]; s == nil || s.Wake != "mentions" {
		t.Fatalf("membership: %+v", s)
	}
}

func TestAttachments(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(map[bool]string{false: "download", true: "oversize"}[oversize], func(t *testing.T) {
			f := setup(t)
			m := message()
			data := "contents"
			if oversize {
				data = strings.Repeat("x", 1025)
			}
			f.google.Configure(func(s *fakegoogle.Server) { s.Media["spaces/room/media/file"] = []byte(data) })
			m.Attachment = []*api.Attachment{{Name: messageName + "/attachments/file", ContentName: "report.txt", ContentType: "text/plain", Source: "UPLOADED_CONTENT", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/file"}},
				{Name: messageName + "/attachments/drive", ContentName: "document", ContentType: "text/plain", Source: "DRIVE_FILE", DriveDataRef: &api.DriveDataRef{DriveFileId: "drive-id"}}}
			handle(t, f, event("attachment", "message.created", m))
			got := f.gc.Inbound()[0]
			if len(got.Attachments) != 2 || !strings.Contains(got.Text, "https://drive.google.com/file/d/drive-id/view") {
				t.Fatalf("attachments: %+v", got)
			}
			if oversize {
				if !strings.Contains(got.Text, "[attachment] report.txt unavailable:") || got.Attachments[0].URL != "" {
					t.Fatalf("oversize: %+v", got)
				}
				return
			}
			u, err := url.Parse(got.Attachments[0].URL)
			if err != nil || u.Scheme != "file" {
				t.Fatalf("url: %v, %v", u, err)
			}
			body, err := os.ReadFile(u.Path)
			if err != nil || string(body) != data || !strings.Contains(got.Text, "[attachment] report.txt (text/plain) "+u.String()) {
				t.Fatalf("download: %q, %v, %q", body, err, got.Text)
			}
		})
	}
}

func TestDeliveryOutcomes(t *testing.T) {
	for _, status := range []string{"delivered", "no_route", "failed", "partial", "pending"} {
		t.Run(status, func(t *testing.T) {
			f := setup(t)
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = status })
			ev := event("p", "message.created", message())
			handle(t, f, ev)
			change := f.l.Changes[messageName]
			if status == "pending" {
				if change.Status != "pending" || change.ReceiptID == "" {
					t.Fatalf("pending: %+v", change)
				}
				handle(t, f, ev)
				if err := f.p.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(f.gc.Inbound()) != 1 || f.l.Counters["turns:"+space] != 0 {
					t.Fatal("pending reposted or counted")
				}
				r := wire.ReceiptResponse{State: "concluded"}
				r.Delivery.Status = "delivered"
				f.gc.SetReceipt(change.ReceiptID, r)
				if err := f.p.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			change = f.l.Changes[messageName]
			if change.Status != "committed" || f.l.Counters["turns:"+space] != 1 {
				t.Fatalf("terminal: %+v, %v", change, f.l.Counters)
			}
			want := 1
			if status == "failed" || status == "partial" {
				want = 2
			}
			if len(f.gc.Inbound()) != want {
				t.Fatalf("posts=%d want=%d", len(f.gc.Inbound()), want)
			}
			if status == "no_route" && f.l.Counters["no_route:"+space] != 1 {
				t.Fatal("routing problem not surfaced")
			}
		})
	}
}

func TestReceiptUncertaintyIsNeverReposted(t *testing.T) {
	for _, state := range []string{"evicted", "expired", "pending_conclusion"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			ev := event("p", "message.created", message())
			handle(t, f, ev)
			id := f.l.Changes[messageName].ReceiptID
			if state == "evicted" {
				f.gc.EvictReceipt(id)
			} else {
				r := wire.ReceiptResponse{State: "concluded"}
				r.Delivery.Status = "pending"
				if state == "expired" {
					r.Delivery.Status = "delivered"
				}
				f.gc.SetReceipt(id, r)
				if state == "expired" {
					f.gc.Advance(time.Hour + time.Second)
				}
			}
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			handle(t, f, ev)
			ev.PubSubID = "again"
			handle(t, f, ev)
			if f.l.Unresolved[messageName] == nil || f.l.Changes[messageName].Status != "unresolved" || len(f.gc.Inbound()) != 1 {
				t.Fatalf("uncertainty: %+v", f.l.Changes[messageName])
			}
		})
	}
}

func TestCrashAfterTerminalBeforeTransportSave(t *testing.T) {
	f := setup(t)
	fail := true
	f.opts.Save = func(l *ledger.Ledger) error {
		if fail && l.Transport.Seen("pubsub:p") {
			return errors.New("simulated crash")
		}
		return l.Save(f.opts.LedgerPath)
	}
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	ev := event("p", "message.created", message())
	if out, err := f.p.Handle(context.Background(), ev); out != puller.Nack || err == nil {
		t.Fatalf("crash=%v %v", out, err)
	}
	fail = false
	f.l, err = ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Ledger = f.l
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	handle(t, f, ev)
	if len(f.gc.Inbound()) != 1 || f.l.Counters["turns:"+space] != 1 {
		t.Fatal("terminal change reposted after restart")
	}
}

func TestLostResponseAndInterruptedIntent(t *testing.T) {
	f := setup(t)
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer s.Close()
	f.opts.GC = gcclient.New(s.URL, "examplecity")
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	ev := event("p", "message.created", message())
	handle(t, f, ev)
	handle(t, f, ev)
	if calls.Load() != 1 || f.l.Unresolved[messageName] == nil {
		t.Fatal("lost response was not held")
	}

	f = setup(t)
	f.opts.Save = func(l *ledger.Ledger) error {
		if c := l.Changes[messageName]; c != nil && c.Status == "committed" {
			return errors.New("crash before terminal persistence")
		}
		return l.Save(f.opts.LedgerPath)
	}
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := f.p.Handle(context.Background(), ev); out != puller.Nack {
		t.Fatal("failed save acknowledged")
	}
	f.l, err = ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Ledger = f.l
	f.opts.Save = nil
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	handle(t, f, ev)
	if len(f.gc.Inbound()) != 1 || f.l.Unresolved[messageName] == nil {
		t.Fatal("interrupted POST must remain unresolved")
	}
}

func TestMalformedCancellationAndPrivateErrors(t *testing.T) {
	f := setup(t)
	if out, err := f.p.Handle(context.Background(), event("bad", "message.created", &api.Message{Name: "bad"})); out != puller.Quarantine || err == nil {
		t.Fatalf("malformed: %v %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := f.p.Handle(ctx, event("cancel", "message.created", message())); out != puller.Nack || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v %v", out, err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "PRIVATE MESSAGE CONTENT", http.StatusServiceUnavailable)
	}))
	defer s.Close()
	f.opts.GC = gcclient.New(s.URL, "examplecity")
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.p.Handle(context.Background(), event("private", "message.created", message()))
	if err != nil && strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("response body leaked")
	}
	b, _ := json.Marshal(f.l.Unresolved)
	if strings.Contains(string(b), "PRIVATE") {
		t.Fatal("response body persisted in reason")
	}
}

func TestReactionLookupDoesNotConsumeMessageCreation(t *testing.T) {
	for _, kind := range []string{"message.created", "message.updated"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			m := message()
			f.google.Configure(func(s *fakegoogle.Server) { s.Messages[m.Name] = m })
			handle(t, f, &puller.Event{PubSubID: "reaction-first", Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: messageName + "/reactions/first", User: &api.User{Name: identity}, Emoji: &api.Emoji{Unicode: "👍"}}}}})
			if kind == "message.updated" {
				m.LastUpdateTime = "2026-10-22T10:01:00Z"
			}
			handle(t, f, event("message-after-reaction", kind, m))
			got := f.gc.Inbound()
			if len(got) != 1 || got[0].ProviderMessageID != messageName || got[0].Text != "hello" {
				t.Fatalf("lookup consumed message: %+v", got)
			}
		})
	}
}

func TestEditsThatFirstWakeRemainFollowed(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.Defaults.Wake = config.WakeMentions
	m := message()
	handle(t, f, event("ambient", "message.created", m))
	m.LastUpdateTime = "2026-10-22T10:01:00Z"
	m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity}}}}
	handle(t, f, event("mention-edit", "message.updated", m))
	m.LastUpdateTime = "2026-10-22T10:02:00Z"
	m.Annotations = nil
	handle(t, f, event("remove-mention", "message.updated", m))
	handle(t, f, event("delete", "message.deleted", &api.Message{Name: messageName}))
	if got := f.gc.Inbound(); len(got) != 3 || got[2].Text != "deleted" {
		t.Fatalf("followups after first wake: %+v", got)
	}
}

func TestRetriesExhaustedAndPendingFailureRetries(t *testing.T) {
	for _, status := range []string{"failed", "partial"} {
		t.Run(status, func(t *testing.T) {
			f := setup(t)
			f.opts.Save = func(l *ledger.Ledger) error {
				// Keep the real fake on the selected failure through every attempt.
				f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = status })
				return l.Save(f.opts.LedgerPath)
			}
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			ev := event("exhausted", "message.created", message())
			handle(t, f, ev)
			handle(t, f, ev)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.gc.Inbound(); len(got) != 4 || f.l.Unresolved[messageName] == nil || f.l.Counters["turns:"+space] != 0 {
				t.Fatalf("bounded retries: posts=%d, %+v", len(got), f.l.Changes[messageName])
			}
			for _, m := range f.gc.Inbound() {
				if m.ProviderMessageID != messageName {
					t.Fatal("retry identity changed")
				}
			}
		})
	}
	for _, status := range []string{"failed", "partial", "no_route"} {
		t.Run("receipt_"+status, func(t *testing.T) {
			f := setup(t)
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, f, event("pending", "message.created", message()))
			r := wire.ReceiptResponse{State: "concluded"}
			r.Delivery.Status = status
			f.gc.SetReceipt(f.l.Changes[messageName].ReceiptID, r)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 2
			if status == "no_route" {
				want = 1
			}
			if len(f.gc.Inbound()) != want || f.l.Changes[messageName].Status != "committed" || f.l.Counters["turns:"+space] != 1 {
				t.Fatal("concluded receipt not honored")
			}
		})
	}
}

func TestTransportIDsBatchAndUnreachableRecovery(t *testing.T) {
	f := setup(t)
	ev := event("pubsub-one", "message.created", message())
	ev.CloudEventID = "cloud-one"
	handle(t, f, ev)
	m := message()
	m.Name = space + "/messages/two"
	ev = event("pubsub-two", "message.created", m)
	ev.CloudEventID = "cloud-one"
	handle(t, f, ev)
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("CloudEvent repeat was posted")
	}
	ev.CloudEventID = ""
	handle(t, f, ev)
	if len(f.gc.Inbound()) != 2 {
		t.Fatal("absent CloudEvent suppressed a distinct Pub/Sub ID")
	}

	f = setup(t)
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	s.Close()
	f.opts.GC = gcclient.New(s.URL, "examplecity")
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	ev = event("batch", "message.created", message())
	ev.Changes = append(ev.Changes, puller.Change{Kind: "message.created", Space: space, Message: m})
	if out, err := f.p.Handle(context.Background(), ev); out != puller.Nack || err == nil {
		t.Fatalf("unreachable: %v %v", out, err)
	}
	if f.l.Transport.Seen("pubsub:batch") || f.l.Unresolved[messageName] != nil {
		t.Fatal("unreachable event consumed")
	}
	f.opts.GC = gcclient.New(f.gc.URL(), "examplecity")
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	handle(t, f, ev)
	handle(t, f, ev)
	if len(f.gc.Inbound()) != 2 || f.l.Counters["turns:"+space] != 2 {
		t.Fatal("batch did not recover")
	}
}

func TestCancellationDuringPOSTAndWaitingPoll(t *testing.T) {
	f := setup(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer s.Close()
	defer close(release)
	f.opts.GC = gcclient.New(s.URL, "examplecity")
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.p.Handle(ctx, event("cancel", "message.created", message())); done <- err }()
	<-entered
	// The shared ledger lock remains available throughout a network call.
	if !f.opts.LedgerMu.TryLock() {
		t.Fatal("network request holds ledger lock")
	}
	f.opts.LedgerMu.Unlock()
	pollCtx, pollCancel := context.WithCancel(context.Background())
	pollCancel()
	if err := f.p.Poll(pollCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting poll: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("POST cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Handle did not cancel promptly")
	}
	if f.l.Unresolved[messageName] == nil {
		t.Fatal("canceled in-flight POST not retained")
	}
}

func TestAttachmentsRefuseUnsafePaths(t *testing.T) {
	for _, unsafe := range []string{"traversal", "space_symlink", "root_symlink"} {
		t.Run(unsafe, func(t *testing.T) {
			f := setup(t)
			m := message()
			m.Attachment = []*api.Attachment{{Name: messageName + "/attachments/file", ContentName: "report.txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/file"}}}
			f.google.Configure(func(s *fakegoogle.Server) { s.Media["spaces/room/media/file"] = []byte("safe") })
			outside := t.TempDir()
			switch unsafe {
			case "traversal":
				m.Attachment[0].ContentName = "../outside.txt"
			case "space_symlink":
				if err := os.MkdirAll(f.opts.Config.Endpoints.MediaDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(f.opts.Config.Endpoints.MediaDir, "room")); err != nil {
					t.Fatal(err)
				}
			case "root_symlink":
				if err := os.Symlink(outside, f.opts.Config.Endpoints.MediaDir); err != nil {
					t.Fatal(err)
				}
			}
			handle(t, f, event("unsafe", "message.created", m))
			if got := f.gc.Inbound()[0]; !strings.Contains(got.Text, "unavailable:") || got.Attachments[0].URL != "" {
				t.Fatalf("unsafe attachment: %+v", got)
			}
			files, err := os.ReadDir(outside)
			if err != nil || len(files) != 0 {
				t.Fatalf("escaped media root: %v %v", files, err)
			}
		})
	}
}

func TestSupersededIntentNeverRepostsOldText(t *testing.T) {
	for _, kind := range []string{"message.updated", "message.deleted"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			ev := event("old", "message.created", message())
			if out, _ := f.p.Handle(context.Background(), ev); out != puller.Nack {
				t.Fatal("expected unreachable")
			}
			f.opts.GC = gcclient.New(f.gc.URL(), "examplecity")
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			m.Text = "replacement"
			m.LastUpdateTime = "2026-10-22T10:01:00Z"
			handle(t, f, event("newer", kind, m))
			handle(t, f, ev)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := f.gc.Inbound(); len(got) != 1 || got[0].ProviderMessageID == messageName {
				t.Fatalf("superseded text replayed: %+v", got)
			}
			if f.l.Unresolved[messageName] == nil {
				t.Fatal("superseded undelivered intent not retained")
			}
		})
	}
}

func TestMalformedDeliveryAndReceiptLookupFailure(t *testing.T) {
	for _, response := range []string{`{"delivery":{"status":"pending"}}`, `{"delivery":{"status":"future"}}`, `{broken`} {
		t.Run(response, func(t *testing.T) {
			f := setup(t)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			handle(t, f, event("malformed-response", "message.created", message()))
			if f.l.Unresolved[messageName] == nil {
				t.Fatal("malformed response did not retain uncertainty")
			}
		})
	}
	f := setup(t)
	f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
	handle(t, f, event("pending", "message.created", message()))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private", 500) }))
	defer s.Close()
	f.opts.GC = gcclient.New(s.URL, "examplecity")
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.p.Poll(context.Background()); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("receipt failure: %v", err)
	}
	if f.l.Changes[messageName].Status != "pending" || f.l.Unresolved[messageName] != nil {
		t.Fatal("transient lookup lost receipt")
	}
}
