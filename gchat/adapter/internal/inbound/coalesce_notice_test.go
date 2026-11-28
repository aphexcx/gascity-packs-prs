package inbound_test

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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

const noticeRoom = "spaces/room"
const noticeA = noticeRoom + "/messages/m002"

type memberNoticeFixture struct {
	p                   *inbound.Processor
	l                   *ledger.Ledger
	opts                inbound.Options
	ns                  *neversilent.Engine
	nsOpts              neversilent.Options
	clock               atomic.Int64
	mu                  sync.Mutex
	mode                string
	receipt             bool
	backlog             bool
	crashAfterAdmission bool
	posts               []string
	lines               []string
	ids                 map[int]*api.Message
}

func noticeFixture(t *testing.T, mode, window string) *memberNoticeFixture {
	t.Helper()
	f := &memberNoticeFixture{mode: mode, ids: map[int]*api.Message{}}
	f.clock.Store(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC).UnixNano())
	dir := t.TempDir()
	g := fakegoogle.New(t, nil)
	g.Configure(func(s *fakegoogle.Server) { s.Spaces[noticeRoom] = &api.Space{Name: noticeRoom, SpaceType: "SPACE"} })
	gc := httptest.NewServer(http.HandlerFunc(f.respond))
	t.Cleanup(gc.Close)
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity"}, Endpoints: config.Endpoints{
		ChatAPIBase: g.URL(), MediaDir: filepath.Join(dir, "media"), MediaMaxBytes: 1024, StateFile: filepath.Join(dir, "ledger.json"),
	}}
	cfg.Settings.CoalesceWindow = &window
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
	c, err := chat.New(context.Background(), http.DefaultClient, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.l = ledger.New()
	f.l.SpacesListedAt = time.Unix(0, 0).UTC()
	f.nsOpts = neversilent.Options{Enabled: true, Path: filepath.Join(dir, "notices.json"), HealthFile: filepath.Join(dir, "health.json"), Now: f.now,
		Post: func(_ context.Context, m neversilent.Message, line string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lines = append(f.lines, line)
			if m.ID != noticeA && m.ID != noticeRoom+"/messages/m003" {
				t.Errorf("notice addressed ambient message %s", m.ID)
			}
			if m.Thread != strings.Replace(m.ID, "/messages/", "/threads/", 1) {
				t.Errorf("notice lost its member's thread: %+v", m)
			}
			return nil
		}, Send: func(context.Context, neversilent.Mail) error { return nil },
		SendLocal: func(context.Context, neversilent.Message, string) error { return nil }, Log: func(string) {}}
	f.ns, err = neversilent.New(f.nsOpts)
	if err != nil {
		t.Fatal(err)
	}
	f.opts = inbound.Options{Config: cfg, Ledger: f.l, LedgerMu: new(sync.Mutex), LedgerPath: cfg.Endpoints.StateFile,
		Chat: c, GC: gcclient.New(gc.URL, "examplecity"), IdentityUserID: "users/123", Now: f.now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryDelay: time.Millisecond,
		Save: func(l *ledger.Ledger) error {
			if f.crashAfterAdmission {
				if err := l.Save(f.opts.LedgerPath); err != nil {
					return err
				}
				if l.Changes[noticeA] != nil {
					return errors.New("stop after admission")
				}
			}
			return nil
		}, NeverSilent: f.ns,
		Raise: func(context.Context, inbound.Raise) error { return nil }}
	f.rebuild(t)
	return f
}

func (f *memberNoticeFixture) now() time.Time          { return time.Unix(0, f.clock.Load()).UTC() }
func (f *memberNoticeFixture) advance(d time.Duration) { f.clock.Add(int64(d)) }
func (f *memberNoticeFixture) rebuild(t *testing.T) {
	t.Helper()
	var err error
	f.p, err = inbound.New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *memberNoticeFixture) respond(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		answer := wire.ReceiptResponse{State: "pending"}
		if f.receipt {
			answer.State = "concluded"
			answer.Delivery = wire.Delivery{Status: "pending", Members: []wire.DeliveryMember{{Status: "pending", SessionID: "example-session", DeliveredBytes: 120, ExpectedBytes: 120}}}
			switch f.mode {
			case "whole_hold":
				answer.Delivery.Terminal = true
			case "terminal_hold":
				answer.Delivery.Terminal = true
				answer.Delivery.Members[0].DeliveredBytes = 0
			case "receipt_failed":
				answer.Delivery.Status = "failed"
			}
		}
		_ = json.NewEncoder(w).Encode(answer)
		return
	}
	var envelope wire.InboundEnvelope
	if json.NewDecoder(r.Body).Decode(&envelope) != nil {
		w.WriteHeader(400)
		return
	}
	m := envelope.Message
	f.posts = append(f.posts, m.ProviderMessageID)
	if f.backlog && !strings.Contains(m.Text, "enrolled-A") {
		f.backlog = false
		w.WriteHeader(503)
		return
	}
	d := wire.Delivery{Status: "delivered"}
	if strings.Contains(m.Text, "enrolled-A") {
		switch f.mode {
		case "refused":
			w.WriteHeader(503)
			return
		case "rejected":
			w.WriteHeader(422)
			return
		case "lost":
			_, _ = io.WriteString(w, "{")
			return
		case "pending", "whole_hold", "landed", "terminal_hold", "receipt_failed":
			d.Status, d.ReceiptID = "pending", "example-receipt"
			if f.receipt && f.mode == "receipt_failed" {
				d.Status, d.ReceiptID = "failed", ""
			}
		default:
			d.Status = f.mode
		}
		d.Members = []wire.DeliveryMember{{Status: d.Status, SessionID: "example-session"}}
	}
	_ = json.NewEncoder(w).Encode(wire.InboundResponse{Delivery: d})
}

func (f *memberNoticeFixture) send(t *testing.T, n int, text string, mention bool, kind string) error {
	t.Helper()
	m := f.ids[n]
	if m == nil {
		id := fmt.Sprintf("%s/messages/m%03d", noticeRoom, n)
		m = &api.Message{Name: id, Text: text, CreateTime: f.now().Format(time.RFC3339Nano),
			Space: &api.Space{Name: noticeRoom, SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", Type: "HUMAN"},
			Thread: &api.Thread{Name: strings.Replace(id, "/messages/", "/threads/", 1)}}
		if mention {
			m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: "users/123"}}}}
		}
		f.ids[n] = m
	}
	ev := &puller.Event{PubSubID: fmt.Sprintf("%s-%d-%d", kind, n, f.clock.Load()), Changes: []puller.Change{{Kind: kind, Message: m, Space: noticeRoom, PublishTime: f.now(), EventTime: f.now()}}}
	_, err := f.p.Handle(context.Background(), ev)
	return err
}

func (f *memberNoticeFixture) begin(t *testing.T, member bool, entry string) {
	t.Helper()
	mention := "enrolled-A " + strings.Repeat("a", 990)
	if entry == "off" {
		f.advance(11 * time.Second)
		_ = f.send(t, 2, mention, true, "message.created")
		return
	}
	if entry == "restart" && member {
		// A delayed mention is older than already-held ordinary C. Stop after
		// its durable admission, before the urgent anchor can freeze the turn.
		f.ids[3] = &api.Message{Name: noticeRoom + "/messages/m003", Text: "ordinary C",
			CreateTime: f.now().Add(2 * time.Second).Format(time.RFC3339Nano),
			Space:      &api.Space{Name: noticeRoom, SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", Type: "HUMAN"},
			Thread: &api.Thread{Name: noticeRoom + "/threads/m003"}}
		_ = f.send(t, 3, "ordinary C", false, "message.created")
		f.advance(time.Second)
		f.crashAfterAdmission = true
		if err := f.send(t, 2, mention, true, "message.created"); err == nil {
			t.Fatal("crash point not reached")
		}
		f.crashAfterAdmission = false
		f.reload(t, false)
		f.advance(10 * time.Second)
		_ = f.p.Pass(context.Background())
	} else if member && entry == "act_cap" {
		// The fiftieth act freezes before the mention arrives. Even when that
		// POST is refused, the mention owns a separate turn: this is a pin,
		// not an invented path to a non-owner enrollment.
		f.mu.Lock()
		f.backlog = true
		f.mu.Unlock()
		for i := 0; i < 50; i++ {
			_ = f.send(t, 100+i, "ordinary backlog", false, "message.created")
			f.advance(time.Millisecond)
		}
		f.advance(time.Second - 50*time.Millisecond)
		_ = f.send(t, 2, mention, true, "message.created")
		f.advance(time.Second)
		_ = f.send(t, 3, "ordinary C", false, "message.created")
		f.advance(9 * time.Second)
		_ = f.p.Pass(context.Background())
	} else if member {
		f.mu.Lock()
		f.backlog = true
		f.mu.Unlock()
		if err := f.send(t, 1, strings.Repeat("b", 31500), false, "message.created"); err != nil {
			t.Fatal(err)
		}
		f.advance(time.Second)
		if err := f.send(t, 2, mention, true, "message.created"); err == nil {
			t.Fatal("expected backlog 503 before mention freeze")
		}
		f.advance(time.Second)
		_ = f.send(t, 3, "ordinary C", entry == "two_mentions", "message.created")
		f.advance(9 * time.Second)
		_ = f.p.Pass(context.Background())
	} else {
		f.advance(10 * time.Second)
		if err := f.send(t, 1, "ordinary X", false, "message.created"); err != nil {
			t.Fatal(err)
		}
		f.advance(time.Second)
		_ = f.send(t, 2, mention, true, "message.created")
	}
	c := f.l.Changes[noticeA]
	wantMember := member && entry != "act_cap"
	if c == nil || (wantMember && c.MemberOf != noticeRoom+"/messages/m003") || (!wantMember && c.MemberOf != "") {
		t.Fatalf("fixture did not reach intended enrollment/ownership: %+v", c)
	}
}

func (f *memberNoticeFixture) records(t *testing.T) map[string]neversilent.Message {
	t.Helper()
	data, err := os.ReadFile(f.nsOpts.Path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Messages map[string]neversilent.Message `json:"messages"`
	}
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.Messages
}

func (f *memberNoticeFixture) reload(t *testing.T, enrollmentOnly bool) {
	t.Helper()
	if enrollmentOnly {
		old := f.records(t)[noticeA]
		f.nsOpts.Path = filepath.Join(filepath.Dir(f.nsOpts.Path), "restarted-notices.json")
		var err error
		f.ns, err = neversilent.New(f.nsOpts)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.ns.Track(neversilent.Message{ID: old.ID, Conversation: old.Conversation, Thread: old.Thread, Created: old.Created}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.l, f.opts.Ledger, f.opts.NeverSilent = l, l, f.ns
	f.opts.Save = func(*ledger.Ledger) error { return nil }
	f.rebuild(t)
}

func (f *memberNoticeFixture) resolve(t *testing.T, action string) {
	t.Helper()
	owner := f.l.Delivery(f.l.Changes[noticeA]).ID
	s, err := server.New(server.Options{Config: f.opts.Config, Ledger: f.l, LedgerMu: f.opts.LedgerMu,
		LedgerPath: f.opts.LedgerPath, Chat: f.opts.Chat, GC: f.opts.GC, Poll: f.p.Pass,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetHealth(server.Health{State: "ready"})
	s.SetNeverSilent(f.ns)
	data, _ := json.Marshal(map[string]string{"id": owner, "action": action})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/resolve", bytes.NewReader(data)))
	if w.Code != 200 {
		t.Fatalf("resolve %s: %d %s", action, w.Code, w.Body.String())
	}
}

type noticeView struct {
	Kind                                  string
	Delivered                             bool
	Failure                               string
	Stopped, Uncertain, Removed, Notified bool
}

func viewNotice(m neversilent.Message) noticeView {
	return noticeView{m.ObservedKind, !m.Delivered.IsZero(), m.FailureKind, !m.Stopped.IsZero(), m.Uncertain, m.Removed, m.Notified}
}

func TestNoticeFollowsEveryEnrolledMember(t *testing.T) {
	type row struct {
		name, outcome, entry, action, withdrawal string
		restart, down                            bool
	}
	rows := []row{
		{name: "refused", outcome: "refused"}, {name: "gate_and_read_422", outcome: "rejected"},
		{name: "answer_lost", outcome: "lost"}, {name: "failed", outcome: "failed"},
		{name: "held", outcome: "held"}, {name: "fourth_partial", outcome: "partial"},
		{name: "no_route", outcome: "no_route"}, {name: "delivered_ok", outcome: "delivered"},
		{name: "delivered_down", outcome: "delivered", down: true}, {name: "pending", outcome: "pending"},
		{name: "whole_paste_terminal", outcome: "whole_hold"}, {name: "landed_whole", outcome: "landed"},
		{name: "terminal_not_whole", outcome: "terminal_hold"}, {name: "receipt_failed", outcome: "receipt_failed"},
		{name: "restart_422", outcome: "rejected", restart: true}, {name: "restart_no_route", outcome: "no_route", restart: true},
		{name: "restart_delivered_ok", outcome: "delivered", restart: true}, {name: "restart_delivered_down", outcome: "delivered", restart: true, down: true},
		{name: "restart_whole_paste", outcome: "whole_hold", restart: true},
		{name: "accept", outcome: "rejected", action: "accept", restart: true},
		{name: "replay", outcome: "rejected", action: "replay", restart: true},
		{name: "two_mentions", outcome: "rejected", entry: "two_mentions"},
		{name: "restart_before_urgent_freeze", outcome: "rejected", entry: "restart"},
		{name: "act_cap_owns_mention_pin", outcome: "rejected", entry: "act_cap"},
		{name: "owner_change", outcome: "refused", withdrawal: "owner"},
		{name: "member_deleted", outcome: "refused", withdrawal: "member"},
		{name: "replay_then_owner_change", outcome: "rejected", action: "replay", withdrawal: "owner", restart: true},
		{name: "window_off_pin", outcome: "rejected", entry: "off"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			type observation struct {
				Stages map[string]noticeView
				Lines  []string
			}
			run := func(member bool) observation {
				window := "8s"
				if tc.entry == "off" {
					window = "0s"
				}
				f := noticeFixture(t, tc.outcome, window)
				f.begin(t, member, tc.entry)
				stages := map[string]noticeView{}
				capture := func(stage string) {
					states := f.records(t)
					m, enrolled := states[noticeA]
					if !enrolled {
						t.Fatal("fixture did not enroll the mention")
					}
					stages[stage] = viewNotice(m)
					for id, state := range states {
						if id != noticeA && !(tc.entry == "two_mentions" && id == noticeRoom+"/messages/m003") && !state.Removed {
							t.Errorf("%s enrolled ambient %s: %+v", stage, id, state)
						}
					}
					ambient := map[string]bool{}
					for n, message := range f.ids {
						if n != 2 && !(tc.entry == "two_mentions" && n == 3) {
							if s, ok := states[message.Name]; ok {
								ambient[message.Name] = s.Removed
							}
						}
					}
					data, _ := json.Marshal(struct {
						Stage   string
						Member  bool
						Record  neversilent.Message
						Ambient map[string]bool
					}{stage, member, m, ambient})
					t.Logf("notice-row %s", data)
					if tc.entry == "two_mentions" && member && stage == "initial" {
						owner := states[noticeRoom+"/messages/m003"]
						if !m.Observed.Equal(owner.Observed) || !reflect.DeepEqual(m.Targets, owner.Targets) {
							t.Error("members did not share observation time and sessions")
						}
					}
				}
				capture("initial")
				if strings.Contains(tc.outcome, "hold") || tc.outcome == "landed" || tc.outcome == "receipt_failed" {
					f.mu.Lock()
					f.receipt = true
					f.mu.Unlock()
					f.advance(9 * time.Second)
					_ = f.p.Pass(context.Background())
					capture("receipt")
				}
				if tc.action != "" {
					if tc.withdrawal != "" {
						f.mu.Lock()
						f.mode = "refused"
						f.mu.Unlock()
					}
					f.resolve(t, tc.action)
					capture("resolved")
				}
				if tc.withdrawal != "" {
					n := 2
					if tc.withdrawal == "owner" {
						n = 1
						if member {
							n = 3
						}
					}
					f.advance(time.Second)
					_ = f.send(t, n, "", false, "message.deleted")
					f.mu.Lock()
					f.mode = "rejected"
					f.mu.Unlock()
					f.advance(9 * time.Second)
					_ = f.p.Pass(context.Background())
					capture("withdrawal")
				}
				if tc.restart {
					if err := f.l.Save(f.opts.LedgerPath); err != nil {
						t.Fatal(err)
					}
					live := viewNotice(f.records(t)[noticeA])
					f.reload(t, true)
					_ = f.p.Pass(context.Background())
					capture("restart")
					if tc.action == "" && stages["restart"] != live {
						t.Errorf("restart differs from live terminal observation: live=%+v restart=%+v", live, stages["restart"])
					}
				}
				for i := 0; i < 3; i++ {
					f.advance(9 * time.Second)
					_ = f.p.Pass(context.Background())
				}
				f.advance(4 * time.Minute)
				health := neversilent.Health{State: "ok", Since: f.now().Add(-4 * time.Minute), CheckedAt: f.now()}
				if tc.down {
					health.State = "session_down"
				}
				data, _ := json.Marshal(health)
				if err := os.WriteFile(f.nsOpts.HealthFile, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := f.ns.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				capture("polled")
				f.mu.Lock()
				lines := append([]string(nil), f.lines...)
				posts := append([]string(nil), f.posts...)
				f.mu.Unlock()
				t.Logf("notice-end member=%v posts=%v lines=%v", member, posts, lines)
				if tc.entry == "off" && len(lines) != 1 {
					t.Errorf("window off posted %d lines, want one", len(lines))
				}
				return observation{stages, lines}
			}
			control, member := run(false), run(true)
			for stage, expected := range control.Stages {
				if got := member.Stages[stage]; got != expected {
					t.Errorf("%s: member=%+v control=%+v", stage, got, expected)
				}
			}
			if !reflect.DeepEqual(member.Lines, control.Lines) {
				t.Errorf("member lines=%v control lines=%v", member.Lines, control.Lines)
			}
		})
	}
}
