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
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

// Both observers are installed in production order, including recovery's copy
// of the config. Every retry uses its real HTTP sender against the fake gc.
type noticeHoldFixture struct {
	t        *testing.T
	cfg      config
	now      time.Time
	opts     neversilent.Options
	mode     atomic.Value
	requests atomic.Int32
	lines    int
	mails    []neversilent.Mail
}

func newNoticeHoldFixture(t *testing.T, mode string) *noticeHoldFixture {
	t.Helper()
	f := &noticeHoldFixture{t: t, now: time.Now().UTC().Add(time.Second)}
	f.mode.Store(mode)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.requests.Add(1)
		mode := f.mode.Load().(string)
		if strings.HasPrefix(mode, "http-") {
			var status int
			_, _ = fmt.Sscanf(mode, "http-%d", &status)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"example-refusal"}`))
			return
		}
		if mode == "timeout" {
			select {
			case <-req.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
			return
		}
		member := `"session_id":"example-session","status":"` + mode + `"`
		if mode == "whole" {
			mode = "pending"
			member = `"session_id":"example-session","status":"pending","delivered_bytes":10,"expected_bytes":10`
		}
		fmt.Fprintf(w, `{"TargetSessionID":"example-session","delivery":{"receipt_id":"example-receipt","status":%q,"members":[{%s}]}}`, mode, member)
	}))
	t.Cleanup(srv.Close)
	f.cfg = recoveryTestConfig(t, srv.URL)
	f.opts = neversilent.Options{Enabled: true, Path: filepath.Join(f.cfg.cityPath, "notices.json"), HealthFile: filepath.Join(f.cfg.cityPath, "health.json"), OperatorContext: "example-operator", City: "example-city", Channel: "slack", Now: func() time.Time { return f.now }, Post: func(context.Context, neversilent.Message, string) error { f.lines++; return nil }, Send: func(_ context.Context, m neversilent.Mail) error { f.mails = append(f.mails, m); return nil }, SendLocal: func(context.Context, neversilent.Message, string) error { return nil }}
	var err error
	f.cfg.neverSilent, err = neversilent.New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := installInboundRecovery(&f.cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg.inboundRecovery.now = func() time.Time { return f.now }
	f.cfg.inboundJournal.now = func() time.Time { return f.now }
	f.cfg.coalescer.deadLetter = inboundDeadLetterSink(f.cfg)
	return f
}

func (f *noticeHoldFixture) message() pendingChannelInbound {
	f.t.Helper()
	p := testPending("example-channel", "100.000001", "invented message")
	p.inbound.Actor = externalActor{ID: "example-user", DisplayName: "Example User"}
	p.inbound.Conversation.Kind = "dm"
	if err := f.cfg.neverSilent.Track(neversilent.Message{ID: neverSilentID(p.inbound.Conversation.ConversationID, p.inbound.ProviderMessageID), Conversation: p.inbound.Conversation.ConversationID, Created: f.now, Targets: []string{"example-session"}}); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *noticeHoldFixture) poll() {
	f.t.Helper()
	if err := f.cfg.reconcileNeverSilent(); err != nil {
		f.t.Fatal(err)
	}
	if err := f.cfg.neverSilent.Poll(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *noticeHoldFixture) counts(lines, starts, clears int) {
	f.t.Helper()
	s, c := 0, 0
	for _, m := range f.mails {
		if strings.HasSuffix(m.Subject, " cleared") {
			c++
		} else {
			s++
		}
	}
	if f.lines != lines || s != starts || c != clears {
		f.t.Fatalf("owner lines=%d start mails=%d cleared mails=%d; want %d/%d/%d", f.lines, s, c, lines, starts, clears)
	}
}

func (f *noticeHoldFixture) stateMessage(p pendingChannelInbound) neversilent.Message {
	f.t.Helper()
	b, err := os.ReadFile(f.opts.Path)
	if err != nil {
		f.t.Fatal(err)
	}
	var state struct {
		Messages map[string]neversilent.Message `json:"messages"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		f.t.Fatal(err)
	}
	return state.Messages[neverSilentID(p.inbound.Conversation.ConversationID, p.inbound.ProviderMessageID)]
}

func (f *noticeHoldFixture) hold(p pendingChannelInbound, rendered bool) {
	f.t.Helper()
	receipt, err := postInboundWithReceipt(f.cfg, p.inbound)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.cfg.inboundRecovery.hold(p, receipt, rendered); err != nil {
		f.t.Fatal(err)
	}
}

func TestNeverSilentHoldLegacyWriters(t *testing.T) {
	for _, writer := range []string{"sink", "followup", "alias"} {
		for _, certainty := range []string{"uncertain", "failed", "refused"} {
			t.Run(writer+"/"+certainty, func(t *testing.T) {
				f := newNoticeHoldFixture(t, "held")
				p := f.message()
				var cause error = fmt.Errorf("delivery outcome unknown")
				if certainty == "failed" {
					f.cfg.observeNeverSilent(p.inbound, recoveryReceipt("example-receipt", "example-session", false), nil)
				}
				if certainty == "refused" {
					cause = &inboundPostError{Status: 422, StatusText: "422 Unprocessable Entity", Body: "invented refusal"}
				}
				// Alias follow-up loses an accepted receipt, not a payload POST;
				// its existing certainty rule is failed evidence only.
				definite := certainty == "failed" || (certainty == "refused" && writer != "alias")
				ok := false
				switch writer {
				case "sink":
					ok = f.cfg.coalescer.deadLetter("example-channel", []pendingChannelInbound{p}, cause)
				case "followup":
					cfg := f.cfg
					cfg.coalescer = nil
					ok = deadLetterHeldLoss(cfg, "example-channel", []pendingChannelInbound{p}, cause)
				case "alias":
					ok = deadLetterHeldAliasLoss(f.cfg, p.inbound, "example-session", "@example", "invented body", cause)
				}
				if !ok {
					t.Fatal("writer did not persist")
				}
				known := f.cfg.inboundRecovery.known("example-channel", p.inbound.ProviderMessageID)
				if writer == "alias" {
					known = f.cfg.inboundRecovery.knownAlias(p.inbound, "example-session")
				}
				if !known {
					t.Error("record missing from recovery index")
				}
				m := f.stateMessage(p)
				if m.Stopped.IsZero() || m.Uncertain == definite {
					t.Fatalf("writer stop=%s uncertain=%t want definite=%t", m.Stopped, m.Uncertain, definite)
				}
				if err := f.cfg.neverSilent.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				lines := 0
				if definite {
					lines = 1
				}
				f.counts(lines, 1, 0)
				f.now = f.now.Add(3 * time.Minute)
				f.poll()
				f.counts(1, 1, 0)
			})
		}
	}
}

func TestNeverSilentHoldPostBranches(t *testing.T) {
	t.Run("refused_connections", func(t *testing.T) {
		f := newNoticeHoldFixture(t, "held")
		p := f.message()
		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()
		f.cfg.gcAPIBase = closed.URL
		for i := 0; i <= 3; i++ {
			if _, err := postInboundWithReceipt(f.cfg, p.inbound); err == nil {
				t.Fatal("expected connection refusal")
			}
			f.poll()
			if i < 3 {
				f.counts(0, 0, 0)
				f.now = f.now.Add(time.Minute)
			}
		}
		f.counts(1, 1, 0)
	})
	t.Run("whole_pending_concluded", func(t *testing.T) {
		f := newNoticeHoldFixture(t, "whole")
		p := f.message()
		f.hold(p, true)
		rows := ingestionJournalRows(t, f.cfg.inboundJournal)
		concluded := false
		for _, r := range rows {
			if r.Phase == "concluded" {
				concluded = true
			}
		}
		if !concluded || f.stateMessage(p).Delivered.IsZero() {
			t.Fatal("whole-paste pending did not reach the engine as concluded delivery")
		}
		f.cfg.inboundRecovery.tick(false)
		if f.cfg.inboundRecovery.counts()["example-channel"].Held != 0 {
			t.Fatal("whole paste stayed held")
		}
		f.now = f.now.Add(3 * time.Minute)
		f.poll()
		f.counts(0, 0, 0)
		stamp := f.now.Format(time.RFC3339Nano)
		if err := os.WriteFile(f.opts.HealthFile, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		f.poll()
		f.counts(1, 1, 0)
		if got := f.cfg.neverSilent.Status().Outages; len(got) != 1 || got[0].Cause != "session_down" {
			t.Fatalf("outages=%+v", got)
		}
	})
}

func TestNeverSilentHoldLadder(t *testing.T) { testNeverSilentHoldLadder(t, false) }

func TestNeverSilentHoldBound(t *testing.T) { testNeverSilentHoldLadder(t, true) }

func testNeverSilentHoldLadder(t *testing.T, bound bool) {
	for _, mode := range []string{"held", "failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newNoticeHoldFixture(t, mode)
			p := f.message()
			start := f.now
			notices := 0
			slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/chat.postMessage" {
					notices++
				}
				_, _ = w.Write([]byte(`{"ok":true,"ts":"example-notice"}`))
			}))
			defer slack.Close()
			old := slackAPIBase
			slackAPIBase = slack.URL
			defer func() { slackAPIBase = old }()
			f.cfg.inboundRecovery.notice = inboundHoldPoster("example-token")
			f.hold(p, true)
			for minute := 1; minute <= 29; minute++ {
				f.now = start.Add(time.Duration(minute) * time.Minute)
				f.cfg.inboundRecovery.tick(false)
				f.poll()
				lines := 0
				if mode == "failed" && minute >= 3 {
					lines = 1
				}
				f.counts(lines, lines, 0)
				if !f.stateMessage(p).Stopped.IsZero() {
					t.Fatal("hold became a stop before the bound")
				}
			}
			if notices != 1 || f.requests.Load() < 3 {
				t.Fatalf("held notices=%d POSTs=%d", notices, f.requests.Load())
			}
			for _, row := range ingestionJournalRows(t, f.cfg.inboundJournal) {
				if row.SessionAct != nil {
					t.Fatal("held notice wrote a session act")
				}
			}
			if !bound {
				return
			}
			f.now = start.Add(30 * time.Minute)
			f.cfg.inboundRecovery.tick(false)
			f.poll()
			if f.cfg.inboundRecovery.counts()["example-channel"].Dead != 1 {
				t.Fatal("bound did not dead-letter")
			}
			lines := 0
			if mode == "failed" {
				lines = 1
			}
			f.counts(lines, 1, 0)
			f.now = f.now.Add(3 * time.Minute)
			f.poll()
			f.counts(1, 1, 0)
		})
	}
}

func TestNeverSilentHoldLandedReplay(t *testing.T) {
	for _, rendered := range []bool{true, false} {
		t.Run(fmt.Sprintf("rendered=%t", rendered), func(t *testing.T) {
			f := newNoticeHoldFixture(t, "held")
			p := f.message()
			f.hold(p, rendered)
			f.cfg.inboundRecovery.bound = time.Minute
			f.now = f.now.Add(time.Minute)
			f.cfg.inboundRecovery.tick(false)
			f.poll()
			f.counts(0, 1, 0)
			f.now = f.now.Add(3 * time.Minute)
			f.poll()
			f.counts(1, 1, 0)
			f.mode.Store("delivered")
			f.cfg.inboundRecovery.observe(recoveryReceipt("example-vouch", "example-session", true))
			f.now = f.now.Add(time.Second)
			f.cfg.inboundRecovery.tick(false)
			f.poll()
			if f.stateMessage(p).Delivered.IsZero() {
				t.Fatal("landed replay did not reach the tracked message under its own ID")
			}
			f.counts(1, 1, 1)
			if f.cfg.inboundRecovery.counts()["example-channel"].Dead != 0 {
				t.Fatal("landed replay remained dead-lettered")
			}
			for i := 0; i < 10; i++ {
				f.now = f.now.Add(5 * time.Second)
				f.poll()
				f.counts(1, 1, 1)
			}
			var err error
			f.cfg.neverSilent, err = neversilent.New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.poll()
			f.counts(1, 1, 1)
			if !f.stateMessage(p).Stopped.IsZero() {
				t.Fatal("completion history resurrected the stop")
			}
		})
	}
}

func TestNeverSilentHoldUnlandedReplay(t *testing.T) {
	for _, mode := range []string{"held", "failed", "http-503", "http-429", "timeout", "http-422", "pending"} {
		t.Run(mode, func(t *testing.T) {
			f := newNoticeHoldFixture(t, "held")
			p := f.message()
			f.hold(p, true)
			f.cfg.inboundRecovery.bound = time.Minute
			f.now = f.now.Add(time.Minute)
			f.cfg.inboundRecovery.tick(false)
			f.poll()
			f.counts(0, 1, 0)
			before := f.stateMessage(p)
			f.now = f.now.Add(3 * time.Minute)
			f.poll()
			f.counts(1, 1, 0)
			f.mode.Store(mode)
			if mode == "timeout" {
				old := gcForwardClient
				gcForwardClient = &http.Client{Timeout: 50 * time.Millisecond}
				defer func() { gcForwardClient = old }()
			}
			f.now = f.now.Add(time.Second)
			f.cfg.inboundRecovery.observe(recoveryReceipt("example-vouch", "example-session", true))
			f.cfg.inboundRecovery.tick(false)
			f.poll()
			if mode == "pending" {
				f.counts(1, 1, 1)
				if !f.stateMessage(p).Stopped.IsZero() {
					t.Fatal("accepted pending replay retained the stop")
				}
				return
			}
			f.counts(1, 1, 0)
			after := f.stateMessage(p)
			if !after.Stopped.Equal(before.Stopped) || after.Uncertain != before.Uncertain {
				t.Fatalf("unlanded replay changed stop: before=%s/%t after=%s/%t", before.Stopped, before.Uncertain, after.Stopped, after.Uncertain)
			}
			f.now = f.now.Add(4 * time.Minute)
			for i := 0; i < 10; i++ {
				f.poll()
			}
			f.counts(1, 1, 0)
			if got := f.cfg.neverSilent.Status().Outages; len(got) != 1 || !got[0].End.IsZero() {
				t.Fatalf("outage did not stay open: %+v", got)
			}
		})
	}
}

func TestNeverSilentHoldPermanentRefusal(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprintf("alias=%t", alias), func(t *testing.T) {
			f := newNoticeHoldFixture(t, "held")
			p := f.message()
			if alias {
				if err := f.cfg.inboundRecovery.holdAlias(p.inbound, "example-session", "@example", "invented body", recoveryReceipt("example-receipt", "example-session", false)); err != nil {
					t.Fatal(err)
				}
				f.mode.Store("http-404")
			} else {
				f.hold(p, true)
				f.mode.Store("http-422")
			}
			f.now = f.now.Add(20 * time.Second)
			f.cfg.inboundRecovery.tick(false)
			f.poll()
			f.counts(1, 1, 0)
			if f.stateMessage(p).Uncertain {
				t.Fatal("permanent re-post refusal became an uncertain stop")
			}
			f.now = f.now.Add(3 * time.Minute)
			f.poll()
			f.counts(1, 1, 0)
		})
	}
}
