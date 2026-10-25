package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestThreadContext400RepliesRequestCount(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		pageSize, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := min(start+pageSize, 401) // root plus 400 replies
		response := slackConversationsRepliesResp{OK: true}
		for i := start; i < end; i++ {
			response.Messages = append(response.Messages, slackThreadMessage{TS: fmt.Sprintf("100.%06d", i), User: "U_TEST", Text: "synthetic"})
		}
		if end < 401 {
			response.HasMore = true
			response.ResponseMetadata.NextCursor = strconv.Itoa(end)
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	withSlackAPIStub(t, srv)
	got, err := fetchThreadReplies(context.Background(), "test", "C1", "100.000000", 20)
	if err != nil || len(got) != 20 || got[0].TS != "100.000000" || got[1].TS != "100.000382" || got[19].TS != "100.000400" {
		t.Fatalf("retained window: %v %+v", err, got)
	}
	t.Logf("400 replies plus root: %d requests", calls.Load())
	if calls.Load() > 2 {
		t.Fatalf("400 replies required %d requests, want at most two", calls.Load())
	}
}

func TestThreadContextPartialDelivery(t *testing.T) {
	for _, failure := range []string{"timeout", "rate-limit", "slack-rate-limit"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/conversations.replies") {
					json.NewEncoder(w).Encode(map[string]any{"ok": false})
					return
				}
				calls.Add(1)
				if r.URL.Query().Get("cursor") != "" {
					switch failure {
					case "timeout":
						<-r.Context().Done()
					case "rate-limit":
						w.WriteHeader(http.StatusTooManyRequests)
					case "slack-rate-limit":
						json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
					}
					return
				}
				response := slackConversationsRepliesResp{OK: true, HasMore: true, Messages: []slackThreadMessage{
					{TS: "100.000001", User: "U_ROOT", Text: "retained root"},
					{TS: "100.000002", User: "U_PRIOR", Text: "newest fetched reply"},
				}}
				response.ResponseMetadata.NextCursor = "page2"
				json.NewEncoder(w).Encode(response)
			}))
			defer srv.Close()
			withSlackAPIStub(t, srv)
			capture := &inboundCapture{}
			gc := httptest.NewServer(capture.handler())
			defer gc.Close()
			cfg := peerTestConfig(gc.URL, nil, nil)
			cfg.slackBotToken = "test"
			cfg.threadContextCache = newThreadContextCache()
			event, _ := json.Marshal(slackMessageEvent{Type: "message", Channel: "C1", User: "U_TEST", TS: "100.000003", ThreadTS: "100.000001", Text: "new inbound"})
			processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", Event: event}, func() {})
			got := capture.snapshot()
			if len(got) != 1 {
				t.Fatalf("deliveries=%d", len(got))
			}
			for _, want := range []string{"retained root", "newest fetched reply", "new inbound", "thread context incomplete"} {
				if !strings.Contains(got[0].Text, want) {
					t.Errorf("degraded delivery missing %q", want)
				}
			}
			if strings.Contains(got[0].Text, "Thread parent could not be fetched") {
				t.Error("discarded the fetched parent")
			}
			if cfg.threadContextCache.lastDeliveredFor("", "C1", "100.000001") != "" {
				t.Error("incomplete fetch advanced the watermark past unseen replies")
			}
			t.Logf("%s: %d requests, delivered partial context", failure, calls.Load())
		})
	}
}

func TestThreadContextPaginationBound(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		response := slackConversationsRepliesResp{OK: true, HasMore: true, Messages: []slackThreadMessage{{TS: fmt.Sprintf("100.%06d", n), Text: "synthetic"}}}
		response.ResponseMetadata.NextCursor = strconv.Itoa(int(n))
		json.NewEncoder(w).Encode(response)
	}))
	defer srv.Close()
	withSlackAPIStub(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := fetchThreadReplies(ctx, "test", "C1", "100.000001", 20)
	if err == nil || len(got) != threadContextMaxPages || calls.Load() != threadContextMaxPages {
		t.Fatalf("unbounded or discarded fetch: requests=%d retained=%d err=%v", calls.Load(), len(got), err)
	}
	t.Logf("endless cursor: stopped after %d requests, retained %d messages", calls.Load(), len(got))
}

func TestThreadContextLargePage(t *testing.T) {
	response := slackConversationsRepliesResp{OK: true}
	for i := 0; i <= 400; i++ {
		response.Messages = append(response.Messages, slackThreadMessage{TS: fmt.Sprintf("100.%06d", i), User: "U_TEST", Text: strings.Repeat("synthetic body ", 200)})
	}
	data, err := json.Marshal(response)
	if err != nil || len(data) <= 1<<20 {
		t.Fatalf("fixture must exceed old response limit: bytes=%d err=%v", len(data), err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()
	withSlackAPIStub(t, srv)
	got, err := fetchThreadReplies(context.Background(), "test", "C1", "100.000000", 20)
	if err != nil || len(got) != 20 || got[0].TS != "100.000000" || got[19].TS != "100.000400" {
		t.Fatalf("valid large page was lost: count=%d err=%v", len(got), err)
	}
	t.Logf("valid page: %d bytes, retained %d messages", len(data), len(got))
}
