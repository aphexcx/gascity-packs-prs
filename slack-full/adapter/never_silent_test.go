package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func TestNeverSilentSlackTransportAndEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, status     string
		eligible, replay bool
	}{
		{"failed", "failed", true, false}, {"pending", "pending", true, false}, {"delivered", "delivered", true, false}, {"ambient", "failed", false, false}, {"replay", "failed", true, true},
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
