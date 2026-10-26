package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	"google.golang.org/api/googleapi"
)

func TestStatusOmitsBacklogContentAndClassifiesSubscriptionRefusal(t *testing.T) {
	f := setup(t)
	f.l.Spaces["spaces/room"] = &ledger.SpaceState{Backlog: []string{`{"text":"private-backlog"}`}}
	f.s.opts.SubscriptionStatus = func() (int, time.Time, ledger.SubscriptionState, error) {
		return 1, time.Time{}, ledger.SubscriptionState{}, &googleapi.Error{Code: 403}
	}
	_, raw, _ := f.call(t, "/status", nil)
	if strings.Contains(string(raw), "private-backlog") || !strings.Contains(string(raw), `"cause_kind":"subscription-refused"`) || !strings.Contains(string(raw), `"backlog_count":1`) {
		t.Fatalf("status: %s", raw)
	}
}

func TestLogCauseCityRestartIsTransient(t *testing.T) {
	err := &gcclient.HTTPError{StatusCode: 404, Problem: &gcclient.Problem{Code: "city-not-found", Detail: "not_found: city not found or not running: examplecity"}}
	if kind, status := logCause(err); kind != "transient" || status != 404 {
		t.Fatalf("%s %d", kind, status)
	}
}

func TestPreparationFailureReconcilesConcurrentDeliveredReceipt(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		f := setup(t)
		p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "late-prepare"}
		if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
			t.Fatal(err)
		}
		unlock, err := f.s.lock(t.Context(), "send:"+p.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		type outcome struct {
			result CommandResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() { result, err := f.s.runCommand(ctx, p, nil, automatic); done <- outcome{result, err} }()
		waiting := false
		for ctx.Err() == nil {
			f.s.keysMu.Lock()
			waiting = f.s.keys["send:"+p.IdempotencyKey].users == 2
			f.s.keysMu.Unlock()
			if waiting {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !waiting {
			unlock()
			t.Fatal("command did not wait for callback send lock")
		}
		// The callback owns the send lock and commits its outcome before releasing it.
		if err := f.s.update(t.Context(), func(l *ledger.Ledger) error {
			l.Sends[p.IdempotencyKey].Receipt = &wire.PublishReceipt{Delivered: true, MessageID: "spaces/room/messages/late"}
			return nil
		}); err != nil {
			unlock()
			t.Fatal(err)
		}
		out := <-done
		unlock()
		got, err := out.result, out.err
		if !errors.Is(err, context.DeadlineExceeded) || !got.Delivered || got.MessageID == "" || got.FailureKind != "" {
			t.Fatalf("automatic=%v result=%+v err=%v", automatic, got, err)
		}
		saved, loadErr := ledger.Load(f.options.LedgerPath, true)
		if loadErr != nil || !saved.Sends[p.IdempotencyKey].RecordPending || !saved.Sends[p.IdempotencyKey].Receipt.Delivered {
			t.Fatalf("lost delivery: %+v %v", saved, loadErr)
		}
	}
}

func TestAutomaticCancellationAfterPreparationSaveStaysPending(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "cancel-after-save"}
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.l.SetSaveGuard(func() error { cancel(); return nil })
	got, err := f.s.runCommand(ctx, p, nil, true)
	if !errors.Is(err, context.Canceled) || got.Delivered {
		t.Fatalf("%+v %v", got, err)
	}
	f.assertPending(t, p.IdempotencyKey, true)
	f.l.SetSaveGuard(nil)
	f.gc.Bind(p.Conversation, p.SessionID)
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 || !f.l.Sends[p.IdempotencyKey].Recorded {
		t.Fatal("interrupted preparation did not recover")
	}
}

func TestAutomaticPreparationLogsRecovery(t *testing.T) {
	f := setup(t)
	var logs bytes.Buffer
	f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "prepare-recovery"}
	f.gc.Bind(p.Conversation, p.SessionID)
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	f.s.SetHealth(Health{State: "initializing"})
	if _, err := f.s.runCommand(t.Context(), p, nil, true); err == nil {
		t.Fatal("unavailable attempt succeeded")
	}
	f.assertPending(t, p.IdempotencyKey, true)
	f.s.SetHealth(Health{State: "ready"})
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), `"operation":"command_prepare"`) != 2 || !strings.Contains(logs.String(), `"level":"INFO"`) {
		t.Fatalf("preparation recovery logs: %s", logs.String())
	}
	if !retryAutomatically(true, wire.PublishReceipt{}) || retryAutomatically(false, wire.PublishReceipt{}) {
		t.Fatal("ambiguous automatic outcome must remain pending")
	}
}

func TestAutomaticUndelivered200LogsEveryAttempt(t *testing.T) {
	f := setup(t)
	var logs bytes.Buffer
	f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "ambiguous-200"}
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, 200, wire.OutboundResult{}) }))
	defer h.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	for range 2 {
		if err := f.s.RetryPending(t.Context()); err == nil {
			t.Fatal("undelivered result succeeded")
		}
		f.assertPending(t, p.IdempotencyKey, true)
	}
	if strings.Count(logs.String(), `"level":"WARN"`) != 2 || strings.Count(logs.String(), `"key":"ambiguous-200"`) != 2 {
		t.Fatalf("attempts missing: %s", logs.String())
	}
	f.s.SetClients(f.options.Chat, f.options.GC)
	f.gc.Bind(p.Conversation, p.SessionID)
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"msg":"gc_outbound recovered"`) {
		t.Fatalf("recovery missing: %s", logs.String())
	}
}

func TestDeliveredReceiptSurvivesRearmAndNoBinding(t *testing.T) {
	for _, bound := range []bool{false, true} {
		f := setup(t)
		p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "delivered-rearm"}
		first := f.send(t, p)
		if !first.Delivered {
			t.Fatal(first)
		}
		if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
			t.Fatal(err)
		}
		prepared, err := ledger.Load(f.options.LedgerPath, true)
		if err != nil {
			t.Fatal(err)
		}
		if v := prepared.Sends[p.IdempotencyKey]; v.Receipt == nil || !v.Receipt.Delivered || v.Receipt.MessageID != first.MessageID || v.RecordPending {
			t.Fatalf("prepare reset an already-delivered callback: %+v", v)
		}
		if err := f.s.update(t.Context(), func(l *ledger.Ledger) error {
			v := l.Sends[p.IdempotencyKey]
			v.NotRecordable, v.RecordingFailureKind = true, "auth"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if bound {
			f.gc.Bind(p.Conversation, p.SessionID)
		}
		got, err := f.s.command(t.Context(), p, nil)
		if err != nil || !got.Delivered || got.Recorded != bound || got.MessageID != first.MessageID || got.FailureKind != "" || f.count() != 1 {
			t.Fatalf("bound=%v got=%+v err=%v sends=%d", bound, got, err, f.count())
		}
		saved, err := ledger.Load(f.options.LedgerPath, true)
		if err != nil {
			t.Fatal(err)
		}
		v := saved.Sends[p.IdempotencyKey]
		if v.RecordingFailureKind != "" || v.NotRecordable == bound || v.RecordPending || !v.Receipt.Delivered {
			t.Fatalf("recording transition: %+v", v)
		}
	}
}
