package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func TestNeverSilentRepeatedReactionIsNotNewActivity(t *testing.T) {
	for _, repeat := range []bool{true, false} {
		t.Run(fmt.Sprintf("repeat=%t", repeat), func(t *testing.T) {
			now := time.Now()
			dir := t.TempDir()
			posts := 0
			health := filepath.Join(dir, "health.json")
			opts := neversilent.Options{Enabled: true, Path: filepath.Join(dir, "notices.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }}
			e, err := neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if repeat && calls > 1 {
					_, _ = w.Write([]byte(`{"ok":false,"error":"already_reacted"}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer fake.Close()
			base := slackAPIBase
			slackAPIBase = fake.URL
			defer func() { slackAPIBase = base }()
			cfg := config{cityPath: dir, neverSilent: e, inboundJournal: &inboundJournal{dir: filepath.Join(dir, "journal")}}
			react := func() {
				w := httptest.NewRecorder()
				handleReact(cfg)(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(`{"conversation":{"conversation_id":"example-channel"},"message_id":"old-message","emoji":"eyes","session_id":"example-session"}`)))
				var receipt reactReceipt
				if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || !receipt.Delivered {
					t.Fatalf("reaction was not delivered: %s", w.Body.String())
				}
			}
			react()
			now = time.Now()
			if err = e.Track(neversilent.Message{ID: "new-request", Conversation: "example-channel", Created: now}); err != nil {
				t.Fatal(err)
			}
			if err = e.Result("new-request", "delivered", []string{"example-session"}); err != nil {
				t.Fatal(err)
			}
			react()
			e, err = neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			cfg.neverSilent = e
			if err = cfg.reconcileNeverSilent(); err != nil {
				t.Fatal(err)
			}
			now = now.Add(3 * time.Minute)
			stamp := now.Format(time.RFC3339)
			if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if repeat {
				want = 1
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d; provider no-op became fresh activity", posts, want)
			}
		})
	}
}

func TestNeverSilentSuccessfulWordsBeforeReadbackAndRecovery(t *testing.T) {
	for _, mode := range []string{"accepted-text", "caption", "empty-caption", "receipt-recovery"} {
		t.Run(mode, func(t *testing.T) {
			fastReadbackTimings(t)
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Add(-2 * time.Minute)
			posts := 0
			e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Track(neversilent.Message{ID: "example", Conversation: "C1", Created: now, Targets: []string{"mayor"}}); err != nil {
				t.Fatal(err)
			}
			if err = e.Stop("example", false); err != nil {
				t.Fatal(err)
			}
			now = now.Add(2 * time.Minute)
			ts := fmt.Sprintf("%d.000001", now.Unix())
			store := testPublishReceiptStore(t, filepath.Join(dir, "receipts"))
			cfg := config{cityPath: dir, neverSilent: e, slackBotToken: "xoxb-fake", fileUploadRoot: dir, publishReceipts: store}
			if mode == "receipt-recovery" {
				err = store.RecordPosted("example-key", publishReceipt{Delivered: true, Conversation: conversationRef{ConversationID: "C1"}, MessageID: ts, sourceSessionID: "mayor"}, slackPostMessageReq{Channel: "C1", Text: "reply"})
				if err != nil {
					t.Fatal(err)
				}
				cfg.publishReceipts = testPublishReceiptStore(t, store.dir)
			} else {
				var fake *httptest.Server
				fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/chat.postMessage":
						fmt.Fprintf(w, `{"ok":true,"ts":%q}`, ts)
					case "/conversations.history":
						fmt.Fprint(w, `{"ok":false,"error":"internal_error"}`)
					case "/files.getUploadURLExternal":
						fmt.Fprintf(w, `{"ok":true,"upload_url":%q,"file_id":"F_EXAMPLE"}`, fake.URL+"/upload")
					case "/files.completeUploadExternal":
						fmt.Fprint(w, `{"ok":true,"files":[{"id":"F_EXAMPLE"}]}`)
					case "/upload":
						w.WriteHeader(http.StatusOK)
					default:
						http.NotFound(w, r)
					}
				}))
				defer fake.Close()
				withSlackAPIStub(t, fake)
				w := httptest.NewRecorder()
				if mode == "accepted-text" {
					handlePublish(cfg, nil, nil, newPublishDedupCache(publishDedupTTL))(w, httptest.NewRequest("POST", "/publish", strings.NewReader(`{"session_id":"mayor","conversation":{"conversation_id":"C1"},"text":"reply","idempotency_key":"example-key"}`)))
				} else {
					file := filepath.Join(dir, "example.png")
					if err = os.WriteFile(file, []byte("PNGDATA"), 0600); err != nil {
						t.Fatal(err)
					}
					caption := "reply words"
					if mode == "empty-caption" {
						caption = " "
					}
					body, _ := json.Marshal(map[string]any{"session_id": "mayor", "conversation": map[string]string{"conversation_id": "C1"}, "file_path": file, "initial_comment": caption})
					handlePublishFile(cfg, nil)(w, httptest.NewRequest("POST", "/publish-file", strings.NewReader(string(body))))
				}
				var receipt struct {
					Delivered bool `json:"delivered"`
				}
				if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || !receipt.Delivered {
					t.Fatalf("publish failed: %s", w.Body.String())
				}
			}
			if err = cfg.reconcileNeverSilent(); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "empty-caption" {
				want = 1
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d", posts, want)
			}
		})
	}
}

func TestNeverSilentSlackTransportAndEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, status     string
		eligible, replay bool
	}{
		{"failed", "failed", true, false}, {"pending", "pending", true, false}, {"delivered", "delivered", true, false}, {"ambient", "failed", false, false}, {"replay", "failed", true, true}, {"other-agent", "failed", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			posts := 0
			mails := 0
			dir := t.TempDir()
			e, err := neversilent.New(neversilent.Options{Enabled: true, City: "example-city", Channel: "slack", OperatorContext: "example-operator", Path: filepath.Join(dir, "state.json"), HealthFile: filepath.Join(dir, "health.json"), Now: func() time.Time { return now },
				Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }, Send: func(context.Context, neversilent.Mail) error { mails++; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"delivery":{"status":"` + tc.status + `","members":[{"session_id":"example-session","status":"` + tc.status + `"}]}}`))
			}))
			defer gc.Close()
			cfg := config{neverSilent: e, gcAPIBase: gc.URL, cityName: "example-city"}
			msg := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "EXAMPLE_CHANNEL", Kind: "dm"}, Actor: externalActor{ID: "EXAMPLE_PERSON"}, Text: "CANARY_CUSTOMER_TEXT"}
			if tc.name == "other-agent" {
				msg.ExplicitTarget = "worker"
			}
			eventID := "example-event"
			if tc.replay {
				eventID = "backfill:example-event"
			}
			cfg.trackNeverSilent(msg, eventID, tc.eligible)
			if _, err = postInboundWithReceipt(cfg, msg); err != nil {
				t.Fatal(err)
			}
			now = now.Add(3 * time.Minute)
			if _, err = postInboundWithReceipt(cfg, msg); err != nil {
				t.Fatal(err)
			}
			if err = e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.name == "failed" {
				want = 1
			}
			if posts != want || mails != want {
				t.Fatalf("posts=%d mails=%d want=%d", posts, mails, want)
			}
		})
	}
}

func TestNeverSilentSlackStoppedUnknownReplyAndNoticeDoesNotCount(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	posts := 0
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{neverSilent: e}
	m := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "EXAMPLE_CHANNEL", Kind: "dm"}, ReplyToMessageID: "123.000"}
	cfg.trackNeverSilent(m, "example-event", true)
	cfg.observeNeverSilent(m, deliveryReceipt{present: true, status: "pending", members: []deliveryReceiptMember{{SessionID: "example-session", Status: "pending"}}}, nil)
	cfg.stopNeverSilent(m, false)
	if err = e.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("unknown outcome posted too early")
	}
	if err = e.Act("EXAMPLE_CHANNEL", "123.000", "example-session", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if err = e.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("session reply did not suppress line")
	}
}

func TestNeverSilentSlackPendingRequiresConclusion(t *testing.T) {
	for _, concluded := range []bool{false, true} {
		now := time.Now()
		dir := t.TempDir()
		posts := 0
		health := filepath.Join(dir, "health.json")
		e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
		if err != nil {
			t.Fatal(err)
		}
		cfg := config{neverSilent: e}
		m := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "EXAMPLE_CHANNEL", Kind: "dm"}}
		cfg.trackNeverSilent(m, "example-event", true)
		phase := "accepted"
		if concluded {
			phase = "concluded"
		}
		cfg.inboundJournal.record(cfg, m, deliveryReceipt{status: "pending", members: []deliveryReceiptMember{{SessionID: "example-session", Status: "pending", Delivered: 10, DeliveredOK: true, DeliveredUnit: "bytes", Expected: 10, ExpectedOK: true, ExpectedUnit: "bytes"}}}, phase)
		now = now.Add(3 * time.Minute)
		stamp := now.Format(time.RFC3339)
		if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err = e.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := 0
		if concluded {
			want = 1
		}
		if posts != want {
			t.Fatalf("concluded=%v posts=%d want=%d", concluded, posts, want)
		}
	}
}

func TestNeverSilentSlackMemberEvidenceOutranksSummary(t *testing.T) {
	for _, status := range []string{"pending", "delivered"} {
		now := time.Now()
		dir := t.TempDir()
		health := filepath.Join(dir, "health.json")
		posts := 0
		e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: health, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
		if err != nil {
			t.Fatal(err)
		}
		cfg := config{neverSilent: e}
		m := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "EXAMPLE_CHANNEL", Kind: "dm"}}
		cfg.trackNeverSilent(m, "example-event", true)
		cfg.observeNeverSilent(m, deliveryReceipt{present: true, status: "delivered", members: []deliveryReceiptMember{{Status: status, SessionID: "example-session", Delivered: 0, DeliveredOK: true, DeliveredUnit: "bytes", Expected: 10, ExpectedOK: true, ExpectedUnit: "bytes"}}}, nil)
		now = now.Add(3 * time.Minute)
		stamp := now.Format(time.RFC3339)
		if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err = e.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if posts != 0 {
			t.Fatalf("summary overrode %s member", status)
		}
	}
}

func TestNeverSilentSlackDefiniteStoppedFailureHasNoGrace(t *testing.T) {
	now := time.Now()
	posts := 0
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(t.TempDir(), "state.json"), Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{neverSilent: e}
	m := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "EXAMPLE_CHANNEL", Kind: "dm"}}
	cfg.trackNeverSilent(m, "example-event", true)
	cfg.observeNeverSilent(m, deliveryReceipt{status: "failed"}, nil)
	cfg.stopNeverSilent(m, false)
	if err = e.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatal("definite stopped failure incorrectly delayed")
	}
}

func TestNeverSilentSlackTopLevelReplyBeforeUnknownReceipt(t *testing.T) {
	now := time.Now()
	posts := 0
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(t.TempDir(), "state.json"), Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{neverSilent: e}
	m := externalInboundMessage{ProviderMessageID: "123.456", Conversation: conversationRef{ConversationID: "EXAMPLE_CHANNEL", Kind: "dm"}}
	cfg.trackNeverSilent(m, "example-event", true)
	cfg.stopNeverSilent(m, false)
	recordDeliveredPublish(cfg, "EXAMPLE_CHANNEL", "", fmt.Sprintf("%d.000000", now.Add(time.Minute).Unix()), "example-session")
	now = now.Add(3 * time.Minute)
	if err = e.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("top-level attributed reply was ignored without a receipt")
	}
}
