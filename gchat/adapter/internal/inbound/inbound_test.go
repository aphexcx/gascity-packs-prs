package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
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
	"gchat-adapter/internal/token"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
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
				m.Text = "hello @Example"
				m.Annotations = []*api.Annotation{{Type: "USER_MENTION", StartIndex: 6, Length: 8, UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity, DisplayName: "Example"}}}}
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

func TestUnavailableMessageDoesNotMakeFollowupsWake(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.Defaults.Wake = config.WakeMentions
	f.google.Configure(func(s *fakegoogle.Server) { delete(s.Spaces, space) })
	m := message()
	m.Space = nil
	handle(t, f, event("unavailable-create", "message.created", m))
	if c := f.l.Changes[messageName]; c == nil || c.Reason != "space unavailable" {
		t.Fatalf("missing unavailable base: %+v", c)
	}
	f.google.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = &api.Space{Name: space, SpaceType: "SPACE"} })
	m.LastUpdateTime = "2026-10-22T10:01:00Z"
	handle(t, f, event("available-edit", "message.updated", m))
	handle(t, f, event("available-delete", "message.deleted", &api.Message{Name: messageName}))
	if c := f.l.Changes[messageName+"#updated#"+m.LastUpdateTime]; c == nil || c.Reason != "wake_policy" {
		t.Fatalf("unavailable base counted as forwarded: %+v", c)
	}
	if c := f.l.Changes[messageName+"#deleted"]; c == nil || c.Reason != "local" || len(f.gc.Inbound()) != 0 || f.l.Counters["turns:"+space] != 0 {
		t.Fatalf("unforwarded deletion woke: %+v", c)
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

func TestNormalizeContentForHistory(t *testing.T) {
	f := setup(t)
	m := message()
	m.Text = "@Reader"
	m.Annotations = []*api.Annotation{{Type: "USER_MENTION", Length: 7, UserMention: &api.UserMentionMetadata{User: &api.User{DisplayName: "Example Reader"}}}}
	m.Attachment = []*api.Attachment{
		{Name: "drive", ContentName: "document", ContentType: "text/plain", DriveDataRef: &api.DriveDataRef{DriveFileId: "drive-id"}},
		{Name: "upload", ContentName: "report.txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/report"}},
		{Name: "missing", ContentName: "missing.txt", ContentType: "text/plain"},
	}
	f.google.Configure(func(g *fakegoogle.Server) { g.Media["spaces/room/media/report"] = []byte("report") })
	text, attachments := NormalizeContent(context.Background(), f.opts.Chat, f.opts.Config.Endpoints, m)
	if len(attachments) != 3 {
		t.Fatalf("%+v", attachments)
	}
	want := "Example Reader\n[attachment] document (text/plain) https://drive.google.com/file/d/drive-id/view\n[attachment] report.txt (text/plain) " + attachments[1].URL + "\n[attachment] missing.txt unavailable: attachment data unavailable"
	if text != want {
		t.Fatalf("got %q want %q", text, want)
	}
	u, err := url.Parse(attachments[1].URL)
	if err != nil || u.Scheme != "file" {
		t.Fatalf("%+v %v", u, err)
	}
	data, err := os.ReadFile(u.Path)
	if err != nil || string(data) != "report" {
		t.Fatalf("%q %v", data, err)
	}
	// History is an independent API boundary; malformed names cannot index past
	// the message-name components when choosing the media directory.
	m.Name = "malformed"
	text, _ = NormalizeContent(context.Background(), f.opts.Chat, f.opts.Config.Endpoints, m)
	if !strings.Contains(text, "report.txt unavailable: invalid message name") {
		t.Fatal(text)
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
			wantTurns := int64(1)
			if status == "no_route" {
				wantTurns = 0
			}
			if change.Status != "committed" || f.l.Counters["turns:"+space] != wantTurns {
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
			wantTurns := int64(1)
			if status == "no_route" {
				wantTurns = 0
			}
			if len(f.gc.Inbound()) != want || f.l.Changes[messageName].Status != "committed" || f.l.Counters["turns:"+space] != wantTurns {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConcurrentOutboundMetadataSurvivesOwnEcho(t *testing.T) {
	f := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"spaces":[{"name":"spaces/room","spaceType":"SPACE"}]}`))}, nil
	})}
	var err error
	f.opts.Chat, err = chat.New(context.Background(), httpClient, f.opts.Config.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	m := message()
	m.Sender.Name = identity
	m.Space.SpaceType = ""
	done := make(chan error, 1)
	go func() { _, err := f.p.Handle(context.Background(), event("echo", "message.created", m)); done <- err }()
	<-entered
	f.opts.LedgerMu.Lock()
	f.l.Messages[messageName] = &ledger.MessageState{Mine: true, Thread: m.Thread.Name, SendKey: "outbound-key"}
	f.opts.LedgerMu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := f.l.Messages[messageName].SendKey; got != "outbound-key" {
		t.Fatalf("outbound send key lost: %q", got)
	}
}

func TestRecordPreservesConcurrentExplicitWake(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.Defaults.Wake = config.WakeAll
	cursor := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	f.l.Spaces[space] = &ledger.SpaceState{MessageCursor: cursor, Backlog: []string{"older-change"}}
	target, _ := url.Parse(f.google.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	var lookups atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		// The command surface writes an explicit policy while space() is away
		// on the network, after inbound has copied the empty Wake.
		f.opts.LedgerMu.Lock()
		f.l.Spaces[space].Wake = "mentions"
		f.opts.LedgerMu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	defer s.Close()
	f.opts.Config.Endpoints.ChatAPIBase = s.URL
	var err error
	f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	m := message()
	m.Space = nil
	m.Sender.Name = identity
	handle(t, f, event("own-echo", "message.created", m))
	saved := f.l.Spaces[space]
	if saved.Wake != "mentions" || saved.Kind != "room" || !saved.MessageCursor.Equal(cursor) || len(saved.Backlog) != 1 || saved.Backlog[0] != "older-change" || lookups.Load() != 1 {
		t.Fatalf("concurrent space state overwritten: %+v", saved)
	}
	m = message()
	m.Name = space + "/messages/after-policy-change"
	handle(t, f, event("after-policy-change", "message.created", m))
	if len(f.gc.Inbound()) != 0 || f.l.Changes[m.Name].Reason != "wake_policy" {
		t.Fatal("explicit wake policy was not applied to the next message")
	}
}

func TestRetryablePOSTResponseRecovers(t *testing.T) {
	for _, status := range []int{500, 503, 429, 408} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := setup(t)
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			ids := make(chan string, 2)
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var envelope wire.InboundEnvelope
				if err := json.Unmarshal(body, &envelope); err != nil {
					t.Error(err)
				}
				ids <- envelope.Message.ProviderMessageID
				if calls.Add(1) == 1 {
					http.Error(w, "PRIVATE RESPONSE CONTENT", status)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			ev := event("retryable", "message.created", message())
			out, err := f.p.Handle(context.Background(), ev)
			if out != puller.Nack || err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("retryable response: %v %v", out, err)
			}
			c := f.l.Changes[messageName]
			if c.Status != "intent" || c.Reason != "retry" || c.Attempts != 0 || f.l.Unresolved[messageName] != nil || f.l.Transport.Seen("pubsub:retryable") {
				t.Fatalf("retry state: %+v", c)
			}
			handle(t, f, ev)
			handle(t, f, ev)
			if calls.Load() != 2 || len(f.gc.Inbound()) != 1 || f.l.Counters["turns:"+space] != 1 || f.l.Changes[messageName].Status != "committed" {
				t.Fatalf("recovery: calls=%d changes=%+v", calls.Load(), f.l.Changes[messageName])
			}
			if first, second := <-ids, <-ids; first != messageName || second != first {
				t.Fatalf("retry IDs: %q %q", first, second)
			}
		})
	}
}

func TestRejectedPOSTRetainsStatusWithoutPrivateLogs(t *testing.T) {
	f := setup(t)
	var logs bytes.Buffer
	f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"code":"validation-failed","detail":"PRIVATE RESPONSE CONTENT"}`)
	}))
	defer s.Close()
	f.opts.GC = gcclient.New(s.URL, "examplecity")
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	m := message()
	m.Text = "PRIVATE REQUEST CONTENT"
	ev := event("rejected", "message.created", m)
	handle(t, f, ev)
	handle(t, f, ev)
	c := f.l.Unresolved[messageName]
	want := "gc rejected the POST: HTTP 422 validation-failed"
	if c == nil || c.Reason != want || calls.Load() != 1 {
		t.Fatalf("rejected POST: %+v, calls=%d", c, calls.Load())
	}
	if strings.Contains(logs.String(), "PRIVATE") || strings.Contains(c.Reason, "PRIVATE") {
		t.Fatalf("private content leaked: %s", logs.String())
	}
	var entry struct{ Level, Cause string }
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "WARN" || entry.Cause != want {
		t.Fatalf("missing rejection warning: %s (%v)", logs.String(), err)
	}
}

func TestPOSTStatusReasonsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   string
	}{
		{"redirect", 302, "", "gc answered HTTP 302; delivery unknown"},
		{"redirect_preserving_method", 307, "", "gc answered HTTP 307; delivery unknown"},
		{"not_found", 404, "", "gc rejected the POST: HTTP 404"},
		{"long_code", 422, strings.Repeat("a", 60000), "gc rejected the POST: HTTP 422 " + strings.Repeat("a", 64)},
		{"exactly_65_bytes", 422, strings.Repeat("a", 65), "gc rejected the POST: HTTP 422 " + strings.Repeat("a", 64)},
		{"bad_character_after_bound", 422, strings.Repeat("a", 64) + "!", "gc rejected the POST: HTTP 422"},
		{"allowed_token", 422, "AZaz09_-", "gc rejected the POST: HTTP 422 AZaz09_-"},
		{"newline", 422, "bad\ncode", "gc rejected the POST: HTTP 422"},
		{"whitespace", 422, "bad code", "gc rejected the POST: HTTP 422"},
		{"punctuation", 422, "bad:code", "gc rejected the POST: HTTP 422"},
		{"unicode", 422, "badé", "gc rejected the POST: HTTP 422"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "detail": "PRIVATE RESPONSE BODY"})
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			ev := event("status-reason", "message.created", message())
			handle(t, f, ev)
			ev.PubSubID = "status-again"
			handle(t, f, ev)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			c := f.l.Unresolved[messageName]
			if c == nil {
				t.Fatal("POST was not retained as unresolved")
			}
			if c.Status != "unresolved" || c.Reason != tc.want || calls.Load() != 1 || f.l.Counters["turns:"+space] != 0 {
				t.Fatalf("status=%s reason=%.120q (%d bytes) calls=%d", c.Status, c.Reason, len(c.Reason), calls.Load())
			}
			var entry struct{ Level, Cause string }
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "WARN" || entry.Cause != tc.want || strings.Contains(logs.String(), "PRIVATE") {
				t.Fatalf("status log: %s (%v)", logs.String(), err)
			}
		})
	}
}

func TestUnavailableReactionParentDoesNotBlockBatch(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := setup(t)
			target, _ := url.Parse(f.google.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var lookups atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages/missing") {
					lookups.Add(1)
					if status == http.StatusForbidden {
						w.WriteHeader(status)
						io.WriteString(w, `{"error":{"code":403,"message":"not a member: PRIVATE LOOKUP CONTENT"}}`)
						return
					}
					http.Error(w, "PRIVATE LOOKUP CONTENT", status)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.Config.Endpoints.ChatAPIBase = s.URL
			var err error
			f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.l.Messages[messageName] = &ledger.MessageState{Mine: true}
			missing := space + "/messages/missing/reactions/first"
			second := messageName + "/reactions/second"
			ev := &puller.Event{PubSubID: "parents", Changes: []puller.Change{
				{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: missing}},
				{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: second}},
			}}
			timeout := 5 * time.Second
			if status == 503 {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			out, err := f.p.Handle(ctx, ev)
			if status != 400 && status != 403 && status != 404 {
				if out != puller.Nack || err == nil || strings.Contains(err.Error(), "PRIVATE") || f.l.Changes[missing+"#created"] != nil || f.l.Transport.Seen("pubsub:parents") {
					t.Fatalf("transient lookup: %v %v", out, err)
				}
				if len(f.gc.Inbound()) != 0 || len(f.l.Changes) != 0 {
					t.Fatal("failed lookup consumed the batch")
				}
				return
			}
			if out != puller.Ack || err != nil {
				t.Fatalf("permanent parent failure: %v %v", out, err)
			}
			c := f.l.Changes[missing+"#created"]
			wantReason := "parent unavailable"
			if status == 400 {
				wantReason += ": permanent (HTTP 400)"
			}
			if c == nil || c.Status != "committed" || c.Reason != wantReason || len(f.gc.Inbound()) != 1 || f.gc.Inbound()[0].ProviderMessageID != second+"#created" {
				t.Fatalf("batch did not continue: %+v, posts=%+v", c, f.gc.Inbound())
			}
			handle(t, f, ev)
			ev.PubSubID = "parents-again"
			handle(t, f, ev)
			if lookups.Load() != 1 || len(f.gc.Inbound()) != 1 {
				t.Fatalf("redelivery repeated lookup: %d", lookups.Load())
			}
		})
	}
}

func TestUnavailableSpaceIsRetained(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := setup(t)
			var lookups atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lookups.Add(1)
				if status == 200 {
					io.WriteString(w, `{"spaces":[]}`)
				} else {
					http.Error(w, "PRIVATE SPACE CONTENT", status)
				}
			}))
			defer s.Close()
			f.opts.Config.Endpoints.ChatAPIBase = s.URL
			var err error
			f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			m.Space = nil
			ev := event("space-unavailable", "message.created", m)
			timeout := 5 * time.Second
			if status == 503 {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			out, err := f.p.Handle(ctx, ev)
			if status != 200 && status != 400 && status != 404 {
				if out != puller.Nack || err == nil || strings.Contains(err.Error(), "PRIVATE") || f.l.Changes[messageName] != nil || f.l.Transport.Seen("pubsub:space-unavailable") {
					t.Fatalf("transient space lookup: %v %v", out, err)
				}
				if len(f.gc.Inbound()) != 0 || len(f.l.Messages) != 0 {
					t.Fatal("failed lookup consumed the message")
				}
				return
			}
			if out != puller.Ack || err != nil {
				t.Fatalf("unavailable space: %v %v", out, err)
			}
			c := f.l.Changes[messageName]
			wantReason := "space unavailable"
			if status == 400 {
				wantReason += ": permanent (HTTP 400)"
			}
			if c == nil || c.Reason != wantReason || c.Status != "committed" || len(f.gc.Inbound()) != 0 {
				t.Fatalf("unavailable space not retained: %+v", c)
			}
			ev.PubSubID = "space-again"
			handle(t, f, ev)
			if lookups.Load() != 1 {
				t.Fatal("unavailable space looked up again")
			}
		})
	}
}

func TestRetainedUnavailableChangesAreCountedAndLoggedOnce(t *testing.T) {
	for _, kind := range []string{"message_space", "reaction_parent", "reaction_space"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "PRIVATE LOOKUP BODY", http.StatusNotFound)
			}))
			defer s.Close()
			f.opts.Config.Endpoints.ChatAPIBase = s.URL
			var err error
			f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			m.Space = nil
			ev := event("retained", "message.created", m)
			id, reason := messageName, "space unavailable"
			if kind != "message_space" {
				r := &api.Reaction{Name: messageName + "/reactions/one"}
				ev.Changes = []puller.Change{{Kind: "reaction.created", Space: space, Reaction: r}}
				id = r.Name + "#created"
				if kind == "reaction_parent" {
					reason = "parent unavailable"
				} else {
					f.l.Messages[messageName] = &ledger.MessageState{Mine: true}
				}
			}
			failSave := true
			f.opts.Save = func(l *ledger.Ledger) error {
				if failSave && l.Changes[id] != nil {
					return errors.New("injected save failure")
				}
				return l.Save(f.opts.LedgerPath)
			}
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := f.p.Handle(context.Background(), ev); out != puller.Nack || err == nil {
				t.Fatalf("failed persistence: %v %v", out, err)
			}
			if logs.Len() != 0 || len(f.l.Counters) != 0 || f.l.Changes[id] != nil {
				t.Fatal("failed save logged or counted a retained record")
			}
			failSave = false
			handle(t, f, ev)
			handle(t, f, ev)
			ev.PubSubID = "retained-again"
			handle(t, f, ev)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			c := f.l.Changes[id]
			if c == nil || c.Status != "committed" || c.Reason != reason || f.l.Unresolved[id] != nil || len(f.gc.Inbound()) != 0 {
				t.Fatalf("retained state: %+v", c)
			}
			if f.l.Counters["unavailable:"+space] != 1 || len(f.l.Counters) != 1 {
				t.Fatalf("unavailable counter: %v", f.l.Counters)
			}
			var entry struct{ Level, Space, Change, Message, Cause string }
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "WARN" || entry.Space != space || entry.Change != id || entry.Message != messageName || entry.Cause != reason {
				t.Fatalf("want one structured warning: %s (%v)", logs.String(), err)
			}
			stored, err := ledger.Load(f.opts.LedgerPath, true)
			if err != nil || stored.Counters["unavailable:"+space] != 1 {
				t.Fatalf("counter not durable: %v", err)
			}
			data, err := os.ReadFile(f.opts.LedgerPath)
			if err != nil || strings.Contains(string(data)+logs.String(), "PRIVATE") {
				t.Fatalf("lookup body leaked or state unreadable: %v", err)
			}
		})
	}
}

func TestMentionRenderingUsesAnnotatedSpans(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"documented_text", `{"text":"Hello @FooBot how are you!","annotations":[{"type":"USER_MENTION","startIndex":6,"length":7,"userMention":{"user":{"name":"users/123","displayName":"FooBot"},"type":"MENTION"}}]}`, "Hello FooBot how are you!"},
		{"unicode_prefix", `{"text":"é😀 @Example!","annotations":[{"type":"USER_MENTION","startIndex":4,"length":8,"userMention":{"user":{"name":"users/123","displayName":"Example"}}}]}`, "é😀 Example!"},
		{"literal_token", `{"text":"@Example <users/123>","annotations":[{"type":"USER_MENTION","length":8,"userMention":{"user":{"name":"users/123","displayName":"Example"}}}]}`, "Example <users/123>"},
		{"bounded_token", `{"text":"<users/123> <users/123>","annotations":[{"type":"USER_MENTION","length":11,"userMention":{"user":{"name":"users/123","displayName":"Example"}}}]}`, "Example <users/123>"},
		{"literal_fallback", `{"text":"@Example","annotations":[{"type":"USER_MENTION","length":8,"userMention":{"user":{"name":"users/123"}}}]}`, "@Example"},
		{"unsorted_spans", `{"text":"@One and @Two","annotations":[{"type":"USER_MENTION","startIndex":9,"length":4,"userMention":{"user":{"displayName":"Second"}}},{"type":"USER_MENTION","length":4,"userMention":{"user":{"displayName":"First"}}}]}`, "First and Second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			m := message()
			if err := json.Unmarshal([]byte(tc.payload), m); err != nil {
				t.Fatal(err)
			}
			handle(t, f, event(tc.name, "message.created", m))
			if got := f.gc.Inbound()[0].Text; got != tc.want {
				t.Fatalf("text=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestMentionRenderingIgnoresInvalidAndOverlappingSpans(t *testing.T) {
	annotation := func(start, length int64) *api.Annotation {
		return &api.Annotation{Type: "USER_MENTION", StartIndex: start, Length: length, UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity, DisplayName: "Example"}}}
	}
	for _, span := range [][2]int64{{-1, 3}, {0, 0}, {0, -1}, {100, 1}, {0, 100}, {1, 1}, {0, 1}, {3, 1<<63 - 1}} {
		m := &api.Message{Text: "😀 <users/123>", Annotations: []*api.Annotation{annotation(span[0], span[1])}}
		if got := renderMentions(m); got != m.Text {
			t.Errorf("span=%v text=%q", span, got)
		}
	}
	m := &api.Message{Text: "@Example!", Annotations: []*api.Annotation{annotation(0, 8), annotation(2, 6), annotation(0, 8), nil, {Type: "USER_MENTION"}}}
	if got := renderMentions(m); got != "Example!" {
		t.Fatalf("overlap: %q", got)
	}
}

func TestPendingReceiptBeyondDefaultDeadlineIsUnresolved(t *testing.T) {
	f := setup(t)
	f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
	ev := event("pending-deadline", "message.created", message())
	handle(t, f, ev)
	// Model an old durable pending change recovered after a long outage.
	f.l.Changes[messageName].Created = time.Now().Add(-time.Hour)
	if err := f.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := f.l.Unresolved[messageName]
	if c == nil || c.Reason != "receipt pending beyond deadline" {
		t.Fatalf("old pending receipt: %+v", f.l.Changes[messageName])
	}
	handle(t, f, ev)
	if err := f.p.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.gc.Inbound()) != 1 || f.l.Counters["turns:"+space] != 0 {
		t.Fatal("expired pending receipt was reposted or counted")
	}
}

func TestReceiptIsLookedUpAfterPendingDeadline(t *testing.T) {
	for _, status := range []string{"delivered", "failed", "pending", "unknown", "lookup_error"} {
		t.Run(status, func(t *testing.T) {
			f := setup(t)
			now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			f.opts.Now = func() time.Time { return now }
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var recovered atomic.Bool
			var lookups atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !recovered.Load() {
					http.Error(w, "gc restarting", http.StatusServiceUnavailable)
					return
				}
				if r.Method == http.MethodGet {
					lookups.Add(1)
					if status == "lookup_error" {
						http.Error(w, "PRIVATE RECEIPT BODY", http.StatusServiceUnavailable)
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			ev := event("outage", "message.created", message())
			if out, err := f.p.Handle(context.Background(), ev); out != puller.Nack || err == nil {
				t.Fatalf("outage: %v %v", out, err)
			}
			// Recovery first obtains a fresh pending receipt for an old intent.
			now = now.Add(2 * time.Hour)
			recovered.Store(true)
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			c := f.l.Changes[messageName]
			if c.Status != "pending" || c.ReceiptID == "" {
				t.Fatalf("recovery: %+v", c)
			}
			r := wire.ReceiptResponse{State: "concluded"}
			r.Delivery.Status = status
			if status == "pending" || status == "unknown" {
				r.State = status
			}
			f.gc.SetReceipt(c.ReceiptID, r)
			err = f.p.Poll(context.Background())
			if (err != nil) != (status == "lookup_error") || (err != nil && strings.Contains(err.Error(), "PRIVATE")) {
				t.Fatalf("receipt poll: %v", err)
			}
			if lookups.Load() != 1 {
				t.Fatalf("receipt lookups=%d, want 1 before applying deadline", lookups.Load())
			}
			wantLookups := int64(1)
			if status == "lookup_error" {
				if f.l.Changes[messageName].Status != "pending" {
					t.Fatal("first late lookup error abandoned receipt")
				}
				if err := f.p.Poll(context.Background()); err == nil {
					t.Fatal("second lookup error missing")
				}
				wantLookups++
			}
			c = f.l.Changes[messageName]
			wantPosts := 1
			if status == "delivered" || status == "failed" {
				if c.Status != "committed" || c.Reason != "delivered" || f.l.Counters["turns:"+space] != 1 || f.l.Unresolved[messageName] != nil {
					t.Fatalf("concluded receipt not honored: %+v", c)
				}
				if status == "failed" {
					wantPosts = 2
				}
			} else {
				wantReason := map[string]string{"pending": "receipt pending beyond deadline", "unknown": "receipt unavailable or unknown", "lookup_error": "receipt lookup failed beyond deadline"}[status]
				if c.Status != "unresolved" || c.Reason != wantReason || f.l.Unresolved[messageName] == nil || f.l.Counters["turns:"+space] != 0 {
					t.Fatalf("unresolved receipt: %+v", c)
				}
			}
			handle(t, f, ev)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			posts := f.gc.Inbound()
			if len(posts) != wantPosts || lookups.Load() != wantLookups {
				t.Fatalf("posts=%d lookups=%d", len(posts), lookups.Load())
			}
			for _, post := range posts {
				if post.ProviderMessageID != messageName {
					t.Fatalf("retry changed identity: %q", post.ProviderMessageID)
				}
			}
		})
	}
}

func TestPendingDeadlineUsesCreatedTimeAndInjectedClock(t *testing.T) {
	for _, deadline := range []time.Duration{0, 2 * time.Minute} {
		t.Run(deadline.String(), func(t *testing.T) {
			f := setup(t)
			now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			created := now
			f.opts.Now = func() time.Time { return now }
			f.opts.PendingDeadline = deadline
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, f, event("clock", "message.created", message()))
			if !f.l.Changes[messageName].Created.Equal(created) {
				t.Fatal("Created ignored injected clock")
			}
			if deadline == 0 {
				deadline = time.Hour
			}
			now = created.Add(deadline - time.Nanosecond)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.l.Changes[messageName].Status != "pending" {
				t.Fatal("receipt expired early")
			}
			// A restart must not reset the deadline.
			f.l, err = ledger.Load(f.opts.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			f.opts.Ledger = f.l
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			now = created.Add(deadline)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if c := f.l.Unresolved[messageName]; c == nil || c.Reason != "receipt pending beyond deadline" {
				t.Fatalf("deadline not enforced: %+v", c)
			}
			now = created.Add(3 * time.Hour)
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.gc.Inbound()) != 1 {
				t.Fatal("deadline caused repost")
			}
		})
	}
	f := setup(t)
	f.opts.PendingDeadline = -time.Second
	if _, err := New(f.opts); err == nil {
		t.Fatal("negative pending deadline accepted")
	}
}

func TestExistingSpaceInheritsWakeDefault(t *testing.T) {
	for _, kind := range []string{"room", "dm", ""} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			f.opts.Config.Settings.Defaults.Wake = config.WakeMentions
			f.l.Spaces[space] = &ledger.SpaceState{Kind: kind}
			handle(t, f, event("seeded", "message.created", message()))
			want := 0
			if kind == "dm" {
				want = 1
			}
			if len(f.gc.Inbound()) != want || f.l.Counters["turns:"+space] != int64(want) {
				t.Fatalf("empty wake bypassed default: posts=%d counters=%v", len(f.gc.Inbound()), f.l.Counters)
			}
			if f.l.Spaces[space].Wake != "mentions" {
				t.Fatalf("wake default not retained: %+v", f.l.Spaces[space])
			}
			// Once inherited, a space's wake policy survives later default changes.
			f.opts.Config.Settings.Defaults.Wake = config.WakeAll
			m := message()
			m.Name = space + "/messages/after-default-change"
			handle(t, f, event("changed-default", "message.created", m))
			if f.l.Spaces[space].Wake != "mentions" || len(f.gc.Inbound()) != 2*want {
				t.Fatal("config default replaced the persisted wake policy")
			}
		})
	}
}

func TestChangeRemovedDuringPOSTDoesNotPanic(t *testing.T) {
	for _, status := range []int{200, 422, 503, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("removed change panicked: %v", recovered)
				}
			}()
			f := setup(t)
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Model an operator removing the change while the network is in flight.
				f.opts.LedgerMu.Lock()
				delete(f.l.Changes, messageName)
				err := f.l.Save(f.opts.LedgerPath)
				f.opts.LedgerMu.Unlock()
				if err != nil {
					t.Error(err)
				}
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				w.WriteHeader(status)
				io.WriteString(w, `{"delivery":{"status":"delivered"}}`)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			out, err := f.p.Handle(context.Background(), event("removed", "message.created", message()))
			if status == 503 {
				if out != puller.Nack || err == nil {
					t.Fatalf("retryable response: %v %v", out, err)
				}
			} else if out != puller.Ack || err != nil {
				t.Fatalf("removed change: %v %v", out, err)
			}
			if f.l.Changes[messageName] != nil || f.l.Unresolved[messageName] != nil || f.l.Counters["turns:"+space] != 0 {
				t.Fatal("removed change was resurrected or counted")
			}
			if logs.Len() != 0 {
				t.Fatalf("logged a change that was not recorded: %s", logs.String())
			}
		})
	}
}

// retryWaitContext schedules an operator change exactly when wait registers
// its cancellation channel, after resume's snapshot and before its next save.
type retryWaitContext struct {
	context.Context
	onWait func()
	once   sync.Once
}

func (c *retryWaitContext) Done() <-chan struct{} {
	c.once.Do(c.onWait)
	return c.Context.Done()
}

func TestChangeRemovedBeforeRetryAttemptIsNotPosted(t *testing.T) {
	f := setup(t)
	turn := f.p.envelope(messageName, space, "room", message().Sender, "")
	data, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	f.l.Changes[messageName] = &ledger.ChangeState{ID: messageName, Space: space, Message: messageName, Kind: "message.created", Status: "intent", Attempts: 1, Reason: "retry", Event: string(data)}
	removed := false
	ctx := &retryWaitContext{Context: context.Background(), onWait: func() {
		f.opts.LedgerMu.Lock()
		defer f.opts.LedgerMu.Unlock()
		delete(f.l.Changes, messageName)
		removed = true
	}}
	if err := f.p.resume(ctx, messageName); err != nil {
		t.Fatal(err)
	}
	if !removed || len(f.gc.Inbound()) != 0 || len(f.l.Changes) != 0 || len(f.l.Counters) != 0 {
		t.Fatalf("removed intent crossed process boundary: removed=%v posts=%d", removed, len(f.gc.Inbound()))
	}
}

func TestLiveDeliveryThenCatchUpDoesNotRecordOrPostAgain(t *testing.T) {
	f := setup(t)
	handle(t, f, event("live-first", "message.created", message()))
	before, err := json.Marshal(f.l)
	if err != nil {
		t.Fatal(err)
	}
	saves := 0
	f.opts.Save = func(l *ledger.Ledger) error {
		saves++
		return l.Save(f.opts.LedgerPath)
	}
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	c := puller.Change{Kind: "message.created", Space: space, Message: message()}
	if out, err := f.p.HandleChange(context.Background(), c); out != puller.Ack || err != nil {
		t.Fatalf("catch-up after live: %v %v", out, err)
	}
	after, err := json.Marshal(f.l)
	if err != nil {
		t.Fatal(err)
	}
	if saves != 0 || !bytes.Equal(before, after) || len(f.gc.Inbound()) != 1 || f.l.Counters["turns:"+space] != 1 {
		t.Fatalf("catch-up re-recorded or posted live change: saves=%d posts=%d", saves, len(f.gc.Inbound()))
	}
	var stored wire.InboundMessage
	if err := json.Unmarshal([]byte(f.l.Changes[messageName].Event), &stored); err != nil || stored.Text != "hello" || f.gc.Inbound()[0].Text != "hello" {
		t.Fatalf("live text relabeled by catch-up: %+v %v", stored, err)
	}
}

func TestCatchUpSavesOnlyDeliveryState(t *testing.T) {
	f := setup(t)
	saves := 0
	f.opts.Save = func(l *ledger.Ledger) error {
		saves++
		return l.Save(f.opts.LedgerPath)
	}
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	c := puller.Change{Kind: "message.created", Space: space, Message: message()}
	if out, err := f.p.HandleChange(context.Background(), c); out != puller.Ack || err != nil {
		t.Fatalf("catch-up: %v %v", out, err)
	}
	if saves != 3 {
		t.Fatalf("catch-up saves=%d, want intent, attempt, outcome only", saves)
	}
	if out, err := f.p.HandleChange(context.Background(), c); out != puller.Ack || err != nil {
		t.Fatalf("catch-up repeat: %v %v", out, err)
	}
	if saves != 3 {
		t.Fatalf("deduplicated catch-up saved unchanged state: %d saves", saves)
	}
	for _, ev := range []*puller.Event{
		{PubSubID: "pubsub-only", Changes: []puller.Change{c}},
		{CloudEventID: "cloud-only", Changes: []puller.Change{c}},
	} {
		handle(t, f, ev)
	}
	if saves != 5 || !f.l.Transport.Seen("pubsub:pubsub-only") || !f.l.Transport.Seen("cloudevent:cloud-only") || len(f.gc.Inbound()) != 1 {
		t.Fatalf("live transport state: saves=%d ledger=%+v", saves, f.l)
	}
}

func TestHandleChangeUsesWakePolicyAndCatchUpMarker(t *testing.T) {
	for _, wake := range []config.WakePolicy{config.WakeAll, config.WakeMentions} {
		t.Run(string(wake), func(t *testing.T) {
			f := setup(t)
			f.opts.Config.Settings.Defaults.Wake = wake
			catchup, ok := any(f.p).(interface {
				HandleChange(context.Context, puller.Change) (puller.Outcome, error)
			})
			if !ok {
				t.Fatal("Processor has no exported catch-up entry")
			}
			c := puller.Change{Kind: "message.created", Space: space, Message: message()}
			out, err := catchup.HandleChange(context.Background(), c)
			if out != puller.Ack || err != nil {
				t.Fatalf("catch-up: %v %v", out, err)
			}
			handle(t, f, event("live-duplicate", c.Kind, c.Message))
			want := 1
			if wake == config.WakeMentions {
				want = 0
			}
			if len(f.gc.Inbound()) != want || f.l.Counters["turns:"+space] != int64(want) {
				t.Fatalf("catch-up wake: %+v", f.gc.Inbound())
			}
			if want == 1 && (f.gc.Inbound()[0].Text != "[catch-up]\nhello" || f.gc.Inbound()[0].ProviderMessageID != messageName) {
				t.Fatalf("catch-up mapping: %+v", f.gc.Inbound()[0])
			}
		})
	}
}

func TestCatchUpReactionMarkerBelongsToCurrentChange(t *testing.T) {
	for _, catchUpDelete := range []bool{true, false} {
		t.Run(fmt.Sprint(catchUpDelete), func(t *testing.T) {
			f := setup(t)
			f.l.Messages[messageName] = &ledger.MessageState{Mine: true}
			r := &api.Reaction{Name: messageName + "/reactions/first", User: &api.User{Name: "users/456"}, Emoji: &api.Emoji{Unicode: "👍"}}
			c := puller.Change{Kind: "reaction.created", Space: space, Reaction: r}
			if out, err := f.p.HandleChange(context.Background(), c); out != puller.Ack || err != nil {
				t.Fatalf("create: %v %v", out, err)
			}
			c.Kind = "reaction.deleted"
			c.Reaction = &api.Reaction{Name: r.Name}
			if catchUpDelete {
				if out, err := f.p.HandleChange(context.Background(), c); out != puller.Ack || err != nil {
					t.Fatalf("delete: %v %v", out, err)
				}
			} else {
				handle(t, f, &puller.Event{PubSubID: "live-delete", Changes: []puller.Change{c}})
			}
			want := "reaction deleted: 👍 on " + messageName
			if catchUpDelete {
				want = "[catch-up]\n" + want
			}
			if got := f.gc.Inbound()[1].Text; got != want {
				t.Fatalf("delete marker: %q, want %q", got, want)
			}
		})
	}
}

func TestLookupHoldCarriesCredentialKind(t *testing.T) {
	for _, path := range []string{"message_space", "reaction_parent", "reaction_space"} {
		for _, status := range []int{401, 403, 408, 409, 503, 429} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				f := setup(t)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "message": "PRIVATE SCOPE CONTENT"}})
				}))
				defer s.Close()
				f.opts.Config.Endpoints.ChatAPIBase = s.URL
				var err error
				f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				f.p, err = New(f.opts)
				if err != nil {
					t.Fatal(err)
				}
				m := message()
				m.Space = nil
				ev := event("credential-hold", "message.created", m)
				if path != "message_space" {
					ev.Changes = []puller.Change{{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: messageName + "/reactions/one"}}}
					if path == "reaction_space" {
						f.l.Messages[messageName] = &ledger.MessageState{Mine: true}
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				out, err := f.p.Handle(ctx, ev)
				auth := status == 401 || status == 403
				kind := map[int]string{401: "auth", 403: "auth", 408: "transient", 409: "transient", 503: "transient", 429: "rate_limited"}[status]
				if out != puller.Nack || err == nil || errors.Is(err, ErrCredentials) != auth {
					t.Fatalf("hold = %v, %v; credentials=%v", out, err, auth)
				}
				if !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("unsafe or unclassified hold: %v", err)
				}
				var apiErr *googleapi.Error
				if !errors.As(err, &apiErr) || apiErr.Code != status {
					t.Fatalf("hold lost the underlying API error: %v", err)
				}
				if len(f.l.Changes) != 0 || len(f.gc.Inbound()) != 0 || f.l.Transport.Seen("pubsub:credential-hold") {
					t.Fatal("held lookup consumed the change")
				}
			})
		}
	}
}

func TestLookupHoldPreservesTokenError(t *testing.T) {
	for _, path := range []string{"message_space", "reaction_parent", "reaction_space"} {
		t.Run(path, func(t *testing.T) {
			f := setup(t)
			cause := fmt.Errorf("PRIVATE TOKEN CONTENT: %w", token.ErrNeedsSignIn)
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, cause
			})}
			var err error
			f.opts.Chat, err = chat.New(context.Background(), client, f.opts.Config.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			m.Space = nil
			ev := event("token-hold", "message.created", m)
			lookup := "space"
			if path != "message_space" {
				ev.Changes = []puller.Change{{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: messageName + "/reactions/one"}}}
				if path == "reaction_space" {
					f.l.Messages[messageName] = &ledger.MessageState{Mine: true}
				} else {
					lookup = "reaction parent"
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			out, err := f.p.Handle(ctx, ev)
			if out != puller.Nack || !errors.Is(err, token.ErrNeedsSignIn) || !errors.Is(err, cause) {
				t.Fatalf("hold lost token error: %v, %v", out, err)
			}
			if want := "inbound: " + lookup + " lookup failed: transient"; err.Error() != want {
				t.Fatalf("hold text = %q, want %q", err.Error(), want)
			}
			if len(f.l.Changes) != 0 || len(f.gc.Inbound()) != 0 || f.l.Transport.Seen("pubsub:token-hold") {
				t.Fatal("token failure consumed the change")
			}
		})
	}
}

func TestRetainedLookupFailureContinuesBatch(t *testing.T) {
	for _, path := range []string{"message_space", "reaction_parent", "reaction_space"} {
		for _, status := range []int{400, 403, 422} {
			t.Run(fmt.Sprintf("%s/%d", path, status), func(t *testing.T) {
				f := setup(t)
				var logs bytes.Buffer
				f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if status == http.StatusForbidden {
						w.WriteHeader(status)
						io.WriteString(w, `{"error":{"code":403,"message":"not a member: PRIVATE LOOKUP BODY"}}`)
						return
					}
					http.Error(w, "PRIVATE LOOKUP BODY", status)
				}))
				defer s.Close()
				f.opts.Config.Endpoints.ChatAPIBase = s.URL
				var err error
				f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				f.p, err = New(f.opts)
				if err != nil {
					t.Fatal(err)
				}
				m := message()
				m.Space = nil
				ev := event("permanent-batch", "message.created", m)
				id, reason := messageName, "space unavailable"
				if path != "message_space" {
					r := &api.Reaction{Name: messageName + "/reactions/one"}
					ev.Changes = []puller.Change{{Kind: "reaction.created", Space: space, Reaction: r}}
					id = r.Name + "#created"
					if path == "reaction_parent" {
						reason = "parent unavailable"
					} else {
						f.l.Messages[messageName] = &ledger.MessageState{Mine: true}
					}
				}
				later := message()
				later.Name = space + "/messages/later"
				ev.Changes = append(ev.Changes, puller.Change{Kind: "message.created", Space: space, Message: later})
				handle(t, f, ev)
				// A new transport delivery still cannot recount or relog the retained change.
				ev.PubSubID = "permanent-batch-again"
				handle(t, f, ev)
				want := reason
				if status != http.StatusForbidden {
					want += fmt.Sprintf(": permanent (HTTP %d)", status)
				}
				c := f.l.Changes[id]
				if c == nil || c.Status != "committed" || c.Reason != want || f.l.Counters["unavailable:"+space] != 1 {
					t.Fatalf("permanent record: %+v counters=%v", c, f.l.Counters)
				}
				if got := f.gc.Inbound(); len(got) != 1 || got[0].ProviderMessageID != later.Name || f.l.Changes[later.Name].Reason != "delivered" {
					t.Fatalf("later change not delivered once: %+v", got)
				}
				var entry struct{ Level, Cause string }
				if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "WARN" || entry.Cause != want || strings.Contains(logs.String(), "PRIVATE") {
					t.Fatalf("retained warning: %s (%v)", logs.String(), err)
				}
				stored, err := ledger.Load(f.opts.LedgerPath, true)
				if err != nil || stored.Changes[id].Reason != want || stored.Counters["unavailable:"+space] != 1 {
					t.Fatalf("retention not durable: %v", err)
				}
				if path == "message_space" {
					f.l.Spaces[space].Wake = "mentions"
					m.LastUpdateTime = "2026-10-22T10:01:00Z"
					handle(t, f, event("later-edit", "message.updated", m))
					if f.l.Changes[messageName+"#updated#"+m.LastUpdateTime].Reason != "wake_policy" || len(f.gc.Inbound()) != 1 {
						t.Fatal("retained base was treated as forwarded")
					}
				}
			})
		}
	}
}

func TestStoppedCityPOSTRetriesOnlyExactDiscriminator(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		code, detail string
		retry        bool
	}{
		{"code", 404, "city-not-found", "PRIVATE DETAIL", true},
		{"detail", 404, "not-found", "not_found: city not found or not running: PRIVATE CITY", true},
		{"ordinary_404", 404, "route-not-found", "PRIVATE DETAIL", false},
		{"bare_404", 404, "", "", false},
		{"code_suffix", 404, "city-not-found-extra", "PRIVATE DETAIL", false},
		{"detail_not_prefix", 404, "not-found", "PRIVATE not_found: city not found or not running", false},
		{"wrong_status", 422, "city-not-found", "not_found: city not found or not running", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.code, "detail": tc.detail})
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			ev := event("stopped-city", "message.created", message())
			out, err := f.p.Handle(context.Background(), ev)
			c := f.l.Changes[messageName]
			if tc.retry {
				if out != puller.Nack || err == nil || strings.Contains(err.Error(), "PRIVATE") || c.Status != "intent" || c.Reason != "retry" || c.Attempts != 0 || f.l.Transport.Seen("pubsub:stopped-city") {
					t.Fatalf("stopped city not held: %v %v %+v", out, err, c)
				}
				if err := f.p.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				handle(t, f, ev)
				if calls.Load() != 2 || len(f.gc.Inbound()) != 1 || f.gc.Inbound()[0].ProviderMessageID != messageName || f.l.Changes[messageName].Reason != "delivered" || f.l.Counters["turns:"+space] != 1 {
					t.Fatal("stopped city did not recover once")
				}
			} else {
				if out != puller.Ack || err != nil || c.Status != "unresolved" {
					t.Fatalf("definite rejection: %v %v %+v", out, err, c)
				}
				if err := f.p.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				handle(t, f, ev)
				if calls.Load() != 1 || len(f.gc.Inbound()) != 0 {
					t.Fatal("definite rejection reposted")
				}
			}
			if strings.Contains(logs.String(), "PRIVATE") || strings.Contains(c.Reason, "PRIVATE") {
				t.Fatal("gc detail leaked")
			}
		})
	}
}

func TestLocalRecordsDoNotCountOrWarnUnavailable(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"own_unlisted", "own"}, {"reaction_unlisted", "local"},
		{"own_permanent", "own"}, {"reaction_permanent", "local"},
		{"wake_policy", "wake_policy"}, {"local_delete", "local"},
		{"known_local_delete", "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			f.google.Configure(func(s *fakegoogle.Server) { delete(s.Spaces, space) })
			var err error
			if strings.HasSuffix(tc.name, "permanent") {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "PRIVATE", http.StatusBadRequest) }))
				defer s.Close()
				f.opts.Config.Endpoints.ChatAPIBase = s.URL
				f.opts.Chat, err = chat.New(context.Background(), http.DefaultClient, f.opts.Config.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
			}
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			m.Space = nil
			ev := event("local-counter", "message.created", m)
			id := messageName
			switch {
			case strings.HasPrefix(tc.name, "own"):
				m.Sender.Name = identity
			case strings.HasPrefix(tc.name, "reaction"):
				f.l.Messages[messageName] = &ledger.MessageState{Mine: false}
				r := &api.Reaction{Name: messageName + "/reactions/one"}
				ev.Changes = []puller.Change{{Kind: "reaction.created", Space: space, Reaction: r}}
				id = r.Name + "#created"
			case tc.name == "wake_policy":
				f.l.Spaces[space] = &ledger.SpaceState{Kind: "room", Wake: "mentions"}
			default:
				ev.Changes[0].Kind = "message.deleted"
				id += "#deleted"
				if tc.name == "known_local_delete" {
					f.l.Messages[messageName] = &ledger.MessageState{}
					f.l.Changes[messageName] = &ledger.ChangeState{ID: messageName, Message: messageName, Kind: "message.created", Status: "committed", Reason: "wake_policy"}
				}
			}
			handle(t, f, ev)
			ev.PubSubID = "local-counter-again"
			handle(t, f, ev)
			if len(f.gc.Inbound()) != 0 || logs.Len() != 0 || len(f.l.Counters) != 0 {
				t.Fatalf("local record counted or warned: counters=%v logs=%s", f.l.Counters, logs.String())
			}
			c := f.l.Changes[id]
			if c == nil || c.Reason != tc.reason || c.Status != "committed" {
				t.Fatalf("local record: %+v", c)
			}
			stored, err := ledger.Load(f.opts.LedgerPath, true)
			if err != nil || len(stored.Counters) != 0 || stored.Changes[id].Reason != tc.reason {
				t.Fatalf("local record not durable: %v", err)
			}
		})
	}
}

func TestLateReceiptLookupFailureGetsDurableGrace(t *testing.T) {
	for _, second := range []string{"delivered", "lookup_error"} {
		t.Run(second, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			f.opts.Now = func() time.Time { return now }
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var lookups atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					n := lookups.Add(1)
					if n == 1 || second == "lookup_error" {
						http.Error(w, "PRIVATE RECEIPT", http.StatusServiceUnavailable)
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, f, event("grace", "message.created", message()))
			before := *f.l.Changes[messageName]
			receipt := wire.ReceiptResponse{State: "concluded"}
			receipt.Delivery.Status = "delivered"
			f.gc.SetReceipt(before.ReceiptID, receipt)
			now = now.Add(time.Hour)
			if err := f.p.Poll(context.Background()); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("first lookup: %v", err)
			}
			c := f.l.Changes[messageName]
			if c.Status != "pending" || c.Reason == "" || c.ReceiptID != before.ReceiptID || c.Attempts != before.Attempts || !c.Created.Equal(before.Created) || len(f.l.Unresolved) != 0 || logs.Len() != 0 {
				t.Fatalf("first failure did not retain pending receipt: %+v logs=%s", c, logs.String())
			}
			// Rebuild from disk: the second pass must remember the first failure.
			f.l, err = ledger.Load(f.opts.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			f.opts.Ledger = f.l
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			err = f.p.Poll(context.Background())
			if (err != nil) != (second == "lookup_error") {
				t.Fatalf("second lookup: %v", err)
			}
			c = f.l.Changes[messageName]
			if second == "delivered" {
				if c.Status != "committed" || c.Reason != "delivered" || f.l.Counters["turns:"+space] != 1 || len(f.l.Unresolved) != 0 || logs.Len() != 0 {
					t.Fatalf("concluded receipt not honored: %+v", c)
				}
			} else {
				if c.Status != "unresolved" || c.Reason != "receipt lookup failed beyond deadline" || f.l.Unresolved[messageName] == nil || f.l.Counters["turns:"+space] != 0 {
					t.Fatalf("repeated failure not surfaced: %+v", c)
				}
				var entry struct{ Level, Cause string }
				if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.Level != "WARN" || entry.Cause != c.Reason {
					t.Fatalf("warning: %s (%v)", logs.String(), err)
				}
			}
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if lookups.Load() != 2 || len(f.gc.Inbound()) != 1 {
				t.Fatalf("receipt was reposted or looked up again: %d", lookups.Load())
			}
		})
	}
}

func TestReceiptLookupGracePreservesConcurrentTerminalChange(t *testing.T) {
	for _, terminal := range []string{"delivered", "unresolved"} {
		t.Run(terminal, func(t *testing.T) {
			f := setup(t)
			now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			f.opts.Now = func() time.Time { return now }
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, f, event("concurrent-grace", "message.created", message()))
			now = now.Add(time.Hour)
			resolved := make(chan []byte, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Simulate an operator resolving the change while Poll's receipt
				// lookup is in flight, after Poll took its pending snapshot.
				var err error
				if terminal == "delivered" {
					err = f.p.outcome(messageName, "delivered", "")
				} else {
					err = f.p.unresolved(messageName, "operator retained uncertainty")
				}
				if err != nil {
					t.Error(err)
				}
				l, err := f.p.snapshot()
				if err != nil {
					t.Error(err)
				}
				data, err := json.Marshal(l)
				if err != nil {
					t.Error(err)
				}
				resolved <- data
				http.Error(w, "PRIVATE RECEIPT", http.StatusServiceUnavailable)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.p.Poll(context.Background()); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("receipt lookup: %v", err)
			}
			var want []byte
			select {
			case want = <-resolved:
			default:
				t.Fatal("receipt lookup did not run the concurrent resolution")
			}
			got, err := json.Marshal(f.l)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("grace changed the terminal ledger: %+v (%v)", f.l.Changes[messageName], err)
			}
			stored, err := ledger.Load(f.opts.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			got, err = json.Marshal(stored)
			if err != nil || !bytes.Equal(got, want) || len(f.gc.Inbound()) != 1 {
				t.Fatalf("terminal resolution not preserved durably: %+v (%v)", stored.Changes[messageName], err)
			}
		})
	}
}

func TestLateReceiptLookupFailuresMustBeConsecutive(t *testing.T) {
	for _, intermediateError := range []bool{false, true} {
		t.Run(fmt.Sprint(intermediateError), func(t *testing.T) {
			f := setup(t)
			created := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			now := created
			f.opts.Now = func() time.Time { return now }
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var fail atomic.Bool
			fail.Store(true)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && fail.Load() {
					http.Error(w, "lookup failed", http.StatusServiceUnavailable)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, f, event("consecutive", "message.created", message()))
			now = created.Add(time.Hour)
			if err := f.p.Poll(context.Background()); err == nil || f.l.Changes[messageName].Status != "pending" {
				t.Fatalf("first late failure: %v", err)
			}
			// A backward clock step allows a pre-deadline pass, which breaks the streak.
			now = created.Add(30 * time.Minute)
			fail.Store(intermediateError)
			if err := f.p.Poll(context.Background()); (err != nil) != intermediateError {
				t.Fatal(err)
			}
			now = created.Add(time.Hour)
			fail.Store(true)
			if err := f.p.Poll(context.Background()); err == nil || f.l.Changes[messageName].Status != "pending" {
				t.Fatalf("nonconsecutive failure abandoned receipt: %v %+v", err, f.l.Changes[messageName])
			}
			if err := f.p.Poll(context.Background()); err == nil || f.l.Changes[messageName].Status != "unresolved" {
				t.Fatalf("consecutive failures not surfaced: %v", err)
			}
		})
	}
}

func TestLiveReactionThenCatchUpDoesNotRecordOrPostAgain(t *testing.T) {
	f := setup(t)
	f.l.Messages[messageName] = &ledger.MessageState{Mine: true, Thread: space + "/threads/topic"}
	r := &api.Reaction{Name: messageName + "/reactions/one", User: &api.User{Name: "users/456"}, Emoji: &api.Emoji{Unicode: "👍"}}
	c := puller.Change{Kind: "reaction.created", Space: space, Reaction: r}
	handle(t, f, &puller.Event{PubSubID: "live-reaction", Changes: []puller.Change{c}})
	before, err := json.Marshal(f.l)
	if err != nil {
		t.Fatal(err)
	}
	saves := 0
	f.opts.Save = func(l *ledger.Ledger) error { saves++; return l.Save(f.opts.LedgerPath) }
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := f.p.HandleChange(context.Background(), c); out != puller.Ack || err != nil {
		t.Fatalf("catch-up: %v %v", out, err)
	}
	after, err := json.Marshal(f.l)
	if err != nil {
		t.Fatal(err)
	}
	if saves != 0 || !bytes.Equal(before, after) || len(f.gc.Inbound()) != 1 || f.l.Counters["turns:"+space] != 1 || f.l.Counters["no_route:"+space] != 0 {
		t.Fatalf("catch-up changed live reaction: saves=%d posts=%d counters=%v", saves, len(f.gc.Inbound()), f.l.Counters)
	}
	change := f.l.Changes[r.Name+"#created"]
	var turn wire.InboundMessage
	if err := json.Unmarshal([]byte(change.Event), &turn); err != nil || turn.Text != "reaction created: 👍 on "+messageName || turn.Text != f.gc.Inbound()[0].Text || change.Reason != "delivered" {
		t.Fatalf("live reaction relabeled: %+v %v", change, err)
	}
}

func TestCanceledLateReceiptLookupKeepsPendingState(t *testing.T) {
	for _, usedGrace := range []bool{false, true} {
		t.Run(fmt.Sprint(usedGrace), func(t *testing.T) {
			f := setup(t)
			now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
			f.opts.Now = func() time.Time { return now }
			var logs bytes.Buffer
			f.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			pollCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var lookups atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					n := lookups.Add(1)
					if !usedGrace || n > 1 {
						cancel()
					}
					http.Error(w, "PRIVATE RECEIPT", http.StatusServiceUnavailable)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer s.Close()
			f.opts.GC = gcclient.New(s.URL, "examplecity")
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
			handle(t, f, event("cancel-poll", "message.created", message()))
			now = now.Add(2 * time.Hour)
			if usedGrace {
				if err := f.p.Poll(context.Background()); err == nil {
					t.Fatal("first lookup should fail")
				}
			}
			before, err := json.Marshal(f.l)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.p.Poll(pollCtx); err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("canceled lookup: %v", err)
			}
			after, err := json.Marshal(f.l)
			if err != nil {
				t.Fatal(err)
			}
			if pollCtx.Err() != context.Canceled || !bytes.Equal(before, after) || f.l.Changes[messageName].Status != "pending" || len(f.l.Unresolved) != 0 || logs.Len() != 0 || len(f.gc.Inbound()) != 1 {
				t.Fatalf("cancel changed pending receipt: %+v logs=%s", f.l.Changes[messageName], logs.String())
			}
			stored, err := os.ReadFile(f.opts.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved ledger.Ledger
			if err := json.Unmarshal(stored, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Changes[messageName].Status != "pending" || saved.Changes[messageName].Reason != f.l.Changes[messageName].Reason {
				t.Fatal("canceled poll changed durable state")
			}
		})
	}
}

// Large retained-history backlogs must not serialize the whole ledger merely
// to obtain an isolated in-memory view. The budget leaves room for one copied
// object per message/change plus map storage, but catches JSON round trips.
func TestSnapshotBacklogAllocationBudget(t *testing.T) {
	l := ledger.New()
	l.Spaces["spaces/room"] = &ledger.SpaceState{Backlog: make([]string, 3000)}
	for i := range 3000 {
		id := fmt.Sprintf("spaces/room/messages/%d", i)
		l.Messages[id] = &ledger.MessageState{Thread: "spaces/room/threads/one"}
		l.Changes[id] = &ledger.ChangeState{ID: id, Message: id, Event: `{"text":"retained history"}`}
		l.Spaces["spaces/room"].Backlog[i] = id
	}
	var snapshot *ledger.Ledger
	allocs := testing.AllocsPerRun(1, func() {
		var err error
		snapshot, err = copyLedger(l)
		if err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 12000 {
		t.Fatalf("snapshot of 3000-message backlog allocated %.0f objects; budget 12000", allocs)
	}
	snapshot.Spaces["spaces/room"].Backlog[0] = "changed"
	snapshot.Messages["spaces/room/messages/0"].Thread = "changed"
	snapshot.Changes["spaces/room/messages/0"].Event = "changed"
	if l.Spaces["spaces/room"].Backlog[0] == "changed" || l.Messages["spaces/room/messages/0"].Thread == "changed" || l.Changes["spaces/room/messages/0"].Event == "changed" {
		t.Fatal("snapshot aliases durable state")
	}
}

func TestPendingReceiptDoesNotClearConcurrentResolutionAfterClockRewind(t *testing.T) {
	f := setup(t)
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	f.opts.Now = func() time.Time { return now }
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "pending" })
	handle(t, f, event("pending-rewind", "message.created", message()))
	f.l.Changes[messageName].Reason = receiptLookupGrace
	now = now.Add(-time.Hour)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f.p.unresolved(messageName, "operator retained uncertainty"); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(wire.ReceiptResponse{State: "pending"})
	}))
	defer h.Close()
	f.opts.GC = gcclient.New(h.URL, "examplecity")
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	c := saved.Changes[messageName]
	if c.Status != "unresolved" || c.Reason != "operator retained uncertainty" || len(f.gc.Inbound()) != 1 {
		t.Fatalf("concurrent terminal change overwritten: %+v", c)
	}
}
