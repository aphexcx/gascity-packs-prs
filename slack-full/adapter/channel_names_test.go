package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelDisplayNilCacheKeepsRawID(t *testing.T) {
	cfg := config{} // nil channelNames, empty token — network-inert
	if got := channelDisplay(cfg, "C1"); got != "C1" {
		t.Fatalf("channelDisplay = %q, want raw id", got)
	}
	if got := resolveChannelName(cfg, "C1"); got != "" {
		t.Fatalf("resolveChannelName = %q, want empty", got)
	}
}

func TestChannelDisplayResolvesAndCaches(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if got := r.URL.Query().Get("channel"); got != "C0BKF28CYUE" {
			t.Errorf("channel param = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true, "channel": {"name": "fundraising-dataroom"}}`))
	}))
	t.Cleanup(srv.Close)
	prev := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() { slackAPIBase = prev })

	cfg := config{slackBotToken: "xoxb-test", channelNames: newChannelNameCache()}
	want := "#fundraising-dataroom (C0BKF28CYUE)"
	for i := 0; i < 3; i++ {
		if got := channelDisplay(cfg, "C0BKF28CYUE"); got != want {
			t.Fatalf("channelDisplay = %q, want %q", got, want)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("conversations.info hit %d times, want 1 (cached)", got)
	}
}

func TestChannelDisplayNegativeCachesFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": false, "error": "channel_not_found"}`))
	}))
	t.Cleanup(srv.Close)
	prev := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() { slackAPIBase = prev })

	cfg := config{slackBotToken: "xoxb-test", channelNames: newChannelNameCache()}
	for i := 0; i < 3; i++ {
		if got := channelDisplay(cfg, "CMISSING"); got != "CMISSING" {
			t.Fatalf("failed lookup must fall back to raw id, got %q", got)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("conversations.info hit %d times, want 1 (failure negative-cached)", got)
	}
}

func TestChannelNameCacheTTLExpiry(t *testing.T) {
	c := newChannelNameCache()
	now := time.Now()
	c.put("C1", "general", now, time.Minute)
	if name, ok := c.get("C1", now.Add(30*time.Second)); !ok || name != "general" {
		t.Fatalf("get inside TTL = (%q, %v)", name, ok)
	}
	if _, ok := c.get("C1", now.Add(2*time.Minute)); ok {
		t.Fatal("entry must expire past TTL")
	}
}
