package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"google.golang.org/api/googleapi"
)

func TestDrainWaitsForActiveWorkAndHonorsCancellation(t *testing.T) {
	f := setup(t)
	f.s.stateGate.RLock()
	locked := true
	defer func() {
		if locked {
			f.s.stateGate.RUnlock()
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.s.Drain(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("drain skipped active work: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	resp := httptest.NewRecorder()
	f.s.ServeHTTP(resp, httptest.NewRequest("GET", "/healthz", nil))
	if resp.Code != 200 {
		t.Fatal("drain blocked health")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("drain cancellation: %v", err)
	}
	done = make(chan error, 1)
	go func() { done <- f.s.Drain(t.Context()) }()
	f.s.stateGate.RUnlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("released work did not drain")
	}
}

func TestSubscriptionRetryStatusSnapshot(t *testing.T) {
	f := setup(t)
	next := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	f.s.opts.SubscriptionStatus = func() (int, time.Time, ledger.SubscriptionState, error) {
		return 3, next, ledger.SubscriptionState{State: "SUSPENDED"}, &googleapi.Error{Code: 403, Message: "private-token-secret"}
	}
	code, data, _ := f.call(t, "/status", nil)
	if code != 200 || strings.Contains(string(data), "private-token-secret") {
		t.Fatalf("status %d %s", code, data)
	}
	var got struct {
		Retry struct {
			Failures int
			Next     time.Time `json:"next_attempt"`
			Kind     string    `json:"cause_kind"`
			Code     int       `json:"http_status"`
			State    string
		} `json:"subscription_retry"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Retry.Failures != 3 || !got.Retry.Next.Equal(next) || got.Retry.Kind != "subscription-refused" || got.Retry.Code != 403 || got.Retry.State != "SUSPENDED" {
		t.Fatalf("retry snapshot: %s", data)
	}
}
