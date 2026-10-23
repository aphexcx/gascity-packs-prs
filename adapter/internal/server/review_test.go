package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
		// The callback has committed while the command was waiting for the send lock.
		if !f.send(t, p).Delivered {
			t.Fatal("callback failed")
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		got, err := f.s.failPreparedCommand(ctx, p, context.DeadlineExceeded, automatic)
		if !errors.Is(err, context.DeadlineExceeded) || !got.Delivered || got.MessageID == "" || got.FailureKind != "" {
			t.Fatalf("automatic=%v result=%+v err=%v", automatic, got, err)
		}
		saved, loadErr := ledger.Load(f.options.LedgerPath, true)
		if loadErr != nil || !saved.Sends[p.IdempotencyKey].RecordPending || !saved.Sends[p.IdempotencyKey].Receipt.Delivered {
			t.Fatalf("lost delivery: %+v %v", saved, loadErr)
		}
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
