package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func bindingStub(t *testing.T, hits *int32, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSessionBoundToConversationCapitalizedShape(t *testing.T) {
	var hits int32
	srv := bindingStub(t, &hits, `{"items": [
		{"Status": "active", "Conversation": {"Provider": "slack", "ConversationID": "C1"}},
		{"Status": "revoked", "Conversation": {"Provider": "slack", "ConversationID": "C2"}}
	]}`)
	cfg := config{gcAPIBase: srv.URL, cityName: "test", provider: "slack", bindingCheck: newBindingCheckCache()}

	if !sessionBoundToConversation(cfg, "sess-1", "C1") {
		t.Fatal("active binding on C1 must report bound")
	}
	if sessionBoundToConversation(cfg, "sess-1", "C2") {
		t.Fatal("revoked binding must not report bound")
	}
	if sessionBoundToConversation(cfg, "sess-1", "C3") {
		t.Fatal("unbound conversation must not report bound")
	}
}

func TestSessionBoundToConversationLowercaseShape(t *testing.T) {
	var hits int32
	srv := bindingStub(t, &hits, `{"items": [
		{"status": "active", "conversation": {"provider": "slack", "conversation_id": "C1"}}
	]}`)
	cfg := config{gcAPIBase: srv.URL, cityName: "test", provider: "slack", bindingCheck: newBindingCheckCache()}
	if !sessionBoundToConversation(cfg, "sess-1", "C1") {
		t.Fatal("lowercase wire shape must parse")
	}
}

func TestSessionBoundToConversationCachesOnlyNegativeVerdicts(t *testing.T) {
	// Not-bound verdicts cache (stale = harmless duplicate); bound
	// verdicts re-verify every time (stale = a suppressed-only
	// delivery after an unbind — message loss).
	var negHits int32
	negSrv := bindingStub(t, &negHits, `{"items": []}`)
	negCfg := config{gcAPIBase: negSrv.URL, cityName: "test", provider: "slack", bindingCheck: newBindingCheckCache()}
	for i := 0; i < 5; i++ {
		if sessionBoundToConversation(negCfg, "sess-1", "C1") {
			t.Fatal("want not bound")
		}
	}
	if got := atomic.LoadInt32(&negHits); got != 1 {
		t.Fatalf("gc hit %d times for not-bound, want 1 (cached)", got)
	}

	var posHits int32
	posSrv := bindingStub(t, &posHits, `{"items": [
		{"Status": "active", "Conversation": {"Provider": "slack", "ConversationID": "C1"}}
	]}`)
	posCfg := config{gcAPIBase: posSrv.URL, cityName: "test", provider: "slack", bindingCheck: newBindingCheckCache()}
	for i := 0; i < 3; i++ {
		if !sessionBoundToConversation(posCfg, "sess-1", "C1") {
			t.Fatal("want bound")
		}
	}
	if got := atomic.LoadInt32(&posHits); got != 3 {
		t.Fatalf("gc hit %d times for bound, want 3 (never cached)", got)
	}
}

func TestSessionBoundToConversationFailsOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cfg := config{gcAPIBase: srv.URL, cityName: "test", provider: "slack", bindingCheck: newBindingCheckCache()}
	if sessionBoundToConversation(cfg, "sess-1", "C1") {
		t.Fatal("lookup error must fail open (not bound → dispatch both copies)")
	}
	// Nil cache: historical behavior preserved, no network call.
	nilCfg := config{provider: "slack"}
	if sessionBoundToConversation(nilCfg, "sess-1", "C1") {
		t.Fatal("nil cache must report not bound")
	}
}
