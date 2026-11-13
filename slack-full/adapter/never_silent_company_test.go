package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func TestNeverSilentCompanyHumanRoutes(t *testing.T) {
	for _, scenario := range []struct {
		route     string
		withdrawn bool
	}{{"room", false}, {"dm", false}, {"mpim", false}, {"room", true}, {"dm", true}, {"mpim", true}} {
		route := scenario.route
		t.Run(fmt.Sprintf("%s/withdrawn=%t", route, scenario.withdrawn), func(t *testing.T) {
			h, fakeGC, _, _ := setupMpim(t)
			defer h.wait()
			bf := baseBindingsFile()
			bf.Bindings[0].Session = "mayor"
			bindings, _, err := ParseCompanyBindings(marshalBindings(t, bf), h.dirStore.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			h.bindStore.set(bindings)
			h.gw.dmBindStore = loadDMBindingsStore(t, h, dmBindingsFile{SchemaVersion: 1, DMBindings: []DMBinding{{Agent: "ollie", Session: "mayor"}}})
			now := time.Now()
			h.gw.now = func() time.Time { return now }
			dir := t.TempDir()
			posts := []neversilent.Message{}
			health := filepath.Join(dir, "health.json")
			opts := neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(_ context.Context, m neversilent.Message, line string) error {
				posts = append(posts, m)
				if line != neversilent.DownLine {
					t.Errorf("line=%q", line)
				}
				return nil
			}}
			e, err := neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			h.gw.cfg.neverSilent = e
			if scenario.withdrawn {
				fakeGC.respStatus = func(int) int { return http.StatusBadRequest }
				for i := 0; i < cap(h.gw.cfg.dispatchSem); i++ {
					h.gw.cfg.dispatchSem <- struct{}{}
				}
			}
			switch route {
			case "dm":
				admitDMViaHandler(t, h, dmMessage("U0HUMAN01", "1700000000.000100", "example request"), ollieAppID, 0)
			case "mpim":
				admitMpimViaHandler(t, h, mpimMessage("U0HUMAN01", "1700000000.000100", "<@U0AAAAAA1> example request"), ollieAppID, 0)
			default:
				h.admitViaHandler(t, humanMessage("1700000000.000100", "<@U0AAAAAA1> example request"), 0)
			}
			if scenario.withdrawn {
				deleted := slackMessageEvent{Subtype: "message_deleted", DeletedTS: "1700000000.000100", TS: "1700000000.000200", Channel: testChannelID}
				switch route {
				case "dm":
					deleted.Channel = dmMessage("U0HUMAN01", "", "").Channel
					admitDMViaHandler(t, h, deleted, ollieAppID, 0)
				case "mpim":
					deleted.Channel = testMpimChannel
					admitMpimViaHandler(t, h, deleted, ollieAppID, 0)
				default:
					h.admitViaHandler(t, deleted, 0)
				}
				// Restart the notice observer after deletion but before target
				// resolution and enrollment by the durable receipt sweep.
				e, err = neversilent.New(opts)
				if err != nil {
					t.Fatal(err)
				}
				h.gw.cfg.neverSilent = e
				for len(h.gw.cfg.dispatchSem) > 0 {
					<-h.gw.cfg.dispatchSem
				}
				h.gw.sweepOnce()
			}
			h.wait()
			now = now.Add(3 * time.Minute)
			stamp := now.Format(time.RFC3339)
			if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if scenario.withdrawn {
				if len(posts) != 0 || len(e.Status().Outages) != 0 {
					t.Fatalf("withdrawn request enrolled: posts=%d outages=%d", len(posts), len(e.Status().Outages))
				}
				return
			}
			if len(posts) != 1 {
				t.Fatalf("owner posts=%d", len(posts))
			}
			if posts[0].OwnerAgent != "ollie" {
				t.Fatalf("owner=%q", posts[0].OwnerAgent)
			}
			if (route == "room") != (posts[0].Thread != "") {
				t.Fatalf("thread=%q", posts[0].Thread)
			}
		})
	}
}

func TestNeverSilentCompanyUsesActingToken(t *testing.T) {
	t.Setenv("NEVER_SILENT", "on")
	t.Setenv("NEVER_SILENT_OPERATOR_CONTEXT", "")
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-gc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", bin)
	posts := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-ollie" {
			t.Errorf("wrong reply identity")
		}
		var p slackPostMessageReq
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p.Channel != "D_EXAMPLE" || p.ThreadTS != "" || p.Text != neversilent.DownLine {
			t.Errorf("wrong notice coordinates or text")
		}
		posts++
		_, _ = w.Write([]byte(`{"ok":true,"ts":"123.456"}`))
	}))
	defer fake.Close()
	base := slackAPIBase
	slackAPIBase = fake.URL
	t.Cleanup(func() { slackAPIBase = base })
	cfg := config{cityPath: dir, slackBotToken: "xoxb-SWITCHBOARD", companySecretsDir: writeTokenFile(t, "ollie", "xoxb-ollie", 0700, 0600)}
	e, err := newNeverSilent(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Track(neversilent.Message{ID: "example", Conversation: "D_EXAMPLE", OwnerAgent: "ollie"}); err != nil {
		t.Fatal(err)
	}
	if err = e.Stop("example", true); err != nil {
		t.Fatal(err)
	}
	if err = e.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("posts=%d", posts)
	}
}

func TestNeverSilentReconcilesDeadLettersWithoutResurrectingDelivery(t *testing.T) {
	for _, alias := range []bool{false, true} {
		for _, resolved := range []bool{false, true} {
			t.Run(fmt.Sprintf("alias=%t/resolved=%t", alias, resolved), func(t *testing.T) {
				dir := t.TempDir()
				now := time.Now()
				posts := 0
				opts := neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }}
				e, err := neversilent.New(opts)
				if err != nil {
					t.Fatal(err)
				}
				cfg := config{cityPath: dir, neverSilent: e, inboundDeadLetterDir: filepath.Join(dir, "dead")}
				m := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "C_EXAMPLE"}}
				cfg.trackNeverSilent(m, "example", true)
				// Only the primary write survives the crash; stopNeverSilent never ran.
				if alias {
					_, err = writeAliasDeadLetter(cfg.inboundDeadLetterDir, "C_EXAMPLE", m, "example-session", "example", "example body", nil)
				} else {
					_, err = writeInboundDeadLetter(cfg.inboundDeadLetterDir, "C_EXAMPLE", []pendingChannelInbound{{inbound: m}}, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Minute)
				if resolved {
					if err = e.Result(neverSilentID("C_EXAMPLE", "123.456"), "delivered", nil); err != nil {
						t.Fatal(err)
					}
				}
				e, err = neversilent.New(opts)
				if err != nil {
					t.Fatal(err)
				}
				cfg.neverSilent = e
				if err = cfg.reconcileNeverSilent(); err != nil {
					t.Fatal(err)
				}
				now = now.Add(3 * time.Minute)
				if err = e.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				want := 1
				if resolved {
					want = 0
				}
				if posts != want {
					t.Fatalf("posts=%d want=%d", posts, want)
				}
			})
		}
	}
}

func TestNeverSilentCompanyRecoveryAndSessionActivity(t *testing.T) {
	for _, mode := range []string{"primary-only", "old", "other-agent", "foreign-city", "reaction", "other-thread", "activity-spool", "published-intent", "journal-act", "redrive", "corrupt-history"} {
		t.Run(mode, func(t *testing.T) {
			h, _, _, _ := setupMpim(t)
			now := time.Now()
			dir := t.TempDir()
			posts := 0
			health := filepath.Join(dir, "health.json")
			stamp := now.Format(time.RFC3339)
			if err := os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			origin := baseOrigin()
			td := TargetDelivery{Agent: "ollie", Session: "mayor", Kind: wakeKindTargeted, Status: companyTargetDelivered, NoticeKind: "delivered", UpdatedAt: now.Add(-4 * time.Minute)}
			if mode == "redrive" {
				td.Status, td.NoticeKind = companyTargetFailed, "pending"
			}
			if mode == "other-agent" {
				td.Session = "other"
			}
			if mode == "foreign-city" {
				td.City = "elsewhere"
			}
			raw, _ := json.Marshal(humanMessage(origin.TS, "example human request"))
			r := &IngressReceipt{Origin: origin, Event: raw, NeverSilent: mode != "old", Status: ingressStatusDelivered, ReceivedAt: now.Add(-4 * time.Minute), Targets: map[string]TargetDelivery{"target": td}, ThreadRootTS: origin.TS}
			if _, _, err = h.receipts.Admit(r); err != nil {
				t.Fatal(err)
			}
			h.gw.cfg.neverSilent = e
			cfg := config{cityPath: dir, neverSilent: e, companyGateway: h.gw, companyIntentsDir: h.intentsDir, inboundDeadLetterDir: filepath.Join(dir, "dead")}
			if err = cfg.reconcileNeverSilent(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "redrive":
				var current *IngressReceipt
				current, err = h.receipts.Get(origin)
				if err == nil {
					td.Status, td.NoticeKind, td.UpdatedAt = companyTargetPending, "pending", now
					current.Targets["target"] = td
					err = h.receipts.Update(current)
				}
			case "reaction":
				err = e.Reaction(neverSilentID(origin.ChannelID, origin.TS), "mayor", now)
			case "other-thread":
				err = e.Act(origin.ChannelID, "another-thread", "mayor", now)
			case "activity-spool":
				body, _ := json.Marshal(map[string]any{"conversation": origin.ChannelID, "thread": "another-thread", "session": "mayor", "at": now})
				err = companyWriteFileAtomic(filepath.Join(dir, ".gc", "slack", "never-silent-acts", "example.json"), body)
			case "journal-act":
				cfg.inboundJournal = &inboundJournal{dir: filepath.Join(dir, "journal")}
				body, _ := json.Marshal(inboundJournalRecord{SessionAct: &ingestionSessionAct{Kind: "reply", Source: "session_publish", Channel: origin.ChannelID, MessageTS: "example-post", SessionID: "mayor", Timestamp: now}})
				err = companyWriteFileAtomic(filepath.Join(cfg.inboundJournal.dir, "example.jsonl"), append(body, '\n'))
			case "published-intent":
				body, _ := json.Marshal(map[string]any{"status": "published", "channel_id": origin.ChannelID, "requester_session": "mayor", "posted_ts": fmt.Sprintf("%d.000001", now.Unix())})
				err = companyWriteFileAtomic(filepath.Join(h.intentsDir, "example.json"), body)
			case "corrupt-history":
				if err = os.MkdirAll(cfg.inboundDeadLetterDir, 0700); err == nil {
					err = os.WriteFile(filepath.Join(cfg.inboundDeadLetterDir, "example.jsonl"), []byte("{incomplete\n"), 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = cfg.reconcileNeverSilent(); err != nil {
				t.Fatal(err)
			}
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "primary-only" || mode == "corrupt-history" {
				want = 1
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d", posts, want)
			}
		})
	}
}

func TestNeverSilentCompanyResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, kind string
	}{{503, "", "unreachable"}, {500, "", "unreachable"}, {404, `{"code":"city-not-found"}`, "unreachable"}, {404, `{"code":"session-not-found"}`, "unknown"}, {429, "", "pending"}, {408, "", "pending"}, {400, "", "failed"}} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			if got := companyNoticeTransport(tc.status, []byte(tc.body), nil); got != tc.kind {
				t.Fatalf("kind=%s want=%s", got, tc.kind)
			}
		})
	}
	for _, status := range []string{"pending", "delivered", "failed", ""} {
		t.Run("200-"+status, func(t *testing.T) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
			}))
			defer fake.Close()
			h := newCompanyHarness(t, fake.URL, nil, nil, 1)
			result := h.gw.postCompanyMessage(TargetDelivery{Session: "mayor"}, "example")
			want := status
			if want == "" {
				want = "unknown"
			}
			if result.noticeKind != want {
				t.Fatalf("kind=%q", result.noticeKind)
			}
		})
	}
}
