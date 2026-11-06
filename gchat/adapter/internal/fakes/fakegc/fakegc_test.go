package fakegc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/wire"
)

func call(t *testing.T, s *Server, method, path string, body any, header bool, status int) map[string]any {
	t.Helper()
	return callCity(t, s, "examplecity", method, path, body, header, status)
}

func callCity(t *testing.T, s *Server, city, method, path string, body any, header bool, status int) map[string]any {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, s.URL()+"/v0/city/"+city+"/extmsg/"+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if header {
		req.Header.Set(wire.RequestHeader, "example")
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != status {
		t.Fatalf("%s: status %d want %d: %s", path, r.StatusCode, status, data)
	}
	if status >= 400 && r.Header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("error content type = %q", r.Header.Get("Content-Type"))
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func conversation() wire.Conversation {
	return wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: "spaces/room", Kind: "room"}
}
func inbound(id string) wire.InboundEnvelope {
	return wire.InboundEnvelope{Message: wire.InboundMessage{ProviderMessageID: id, Conversation: conversation(), Actor: wire.Actor{ID: "users/example"}, Text: id, ReceivedAt: time.Now()}}
}

func TestMutationAndScopeRequirements(t *testing.T) {
	s := New(t)
	reg := wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: "http://127.0.0.1:1/adapter", Capabilities: wire.Capabilities{SupportsAttachments: true, MaxMessageLength: 32000}}
	call(t, s, "POST", "adapters", reg, false, 403)
	registered := call(t, s, "POST", "adapters", reg, true, 201)
	if registered["name"] != "gchat/example" {
		t.Fatalf("default adapter name: %v", registered)
	}
	call(t, s, "POST", "inbound", inbound("one"), false, 403)
	bad := inbound("bad")
	bad.Message.Conversation.ScopeID = ""
	call(t, s, "POST", "inbound", bad, true, 400)
	call(t, s, "POST", "outbound", wire.OutboundRequest{Conversation: conversation()}, false, 403)
	missingScope := conversation()
	missingScope.ScopeID = ""
	call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: missingScope}, true, 422)
	reg.CallbackURL = "/relative"
	call(t, s, "POST", "adapters", reg, true, 422)
	if len(s.Inbound()) != 0 || len(s.Published()) != 0 {
		t.Fatal("invalid requests were accepted")
	}
}

func TestInboundStatusesReceiptsAndRetention(t *testing.T) {
	s := New(t)
	for _, status := range []string{"delivered", "failed", "partial", "no_route", "pending"} {
		s.Configure(func(s *Server) { s.NextDelivery = status })
		result := call(t, s, "POST", "inbound", inbound(status), true, 200)
		delivery := result["delivery"].(map[string]any)
		if delivery["status"] != status {
			t.Fatalf("status: %v", result)
		}
		if status != "pending" {
			continue
		}
		id := delivery["receipt_id"].(string)
		poll := func() map[string]any { return call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200) }
		if poll()["state"] != "pending" {
			t.Fatal("receipt not pending")
		}
		s.Advance(2 * time.Hour)
		if poll()["state"] != "pending" {
			t.Fatal("pending receipt expired")
		}
		receipt := wire.ReceiptResponse{State: "concluded"}
		receipt.Delivery.Status = "delivered"
		s.SetReceipt(id, receipt)
		s.Advance(time.Hour - time.Second)
		concluded := poll()
		if concluded["state"] != "concluded" || concluded["delivery"].(map[string]any)["status"] != "delivered" {
			t.Fatalf("concluded: %v", concluded)
		}
		s.Advance(time.Second + time.Nanosecond)
		if poll()["state"] != "unknown" {
			t.Fatal("receipt retained beyond one hour")
		}
	}
	if got := s.Inbound(); len(got) != 5 || got[0].Text != "delivered" || got[4].Text != "pending" {
		t.Fatalf("inbound order: %+v", got)
	}
	// NextDelivery is a one-shot script; the default remains delivered.
	defaultResult := call(t, s, "POST", "inbound", inbound("default"), true, 200)
	if defaultResult["delivery"].(map[string]any)["status"] != "delivered" {
		t.Fatal("script was not consumed")
	}
	s.Configure(func(s *Server) { s.NextDelivery = "pending" })
	result := call(t, s, "POST", "inbound", inbound("evicted"), true, 200)
	id := result["delivery"].(map[string]any)["receipt_id"].(string)
	s.EvictReceipt(id)
	if call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200)["state"] != "unknown" {
		t.Fatal("not evicted")
	}
	if call(t, s, "GET", "inbound/receipts/missing", nil, false, 200)["state"] != "unknown" {
		t.Fatal("missing receipt")
	}
}

func TestInboundDedupIgnoresDedupKey(t *testing.T) {
	s := New(t)
	m := inbound("same")
	m.Message.DedupKey = "first"
	call(t, s, "POST", "inbound", m, true, 200)
	s.Configure(func(s *Server) { s.NextDelivery = "failed" })
	m.Message.DedupKey = "different"
	duplicate := call(t, s, "POST", "inbound", m, true, 200)
	if duplicate["delivery"].(map[string]any)["status"] != "no_route" {
		t.Fatal("duplicate redelivered")
	}
	m.Message.ProviderMessageID = "second"
	newMessage := call(t, s, "POST", "inbound", m, true, 200)
	if newMessage["delivery"].(map[string]any)["status"] != "failed" {
		t.Fatal("different message deduplicated")
	}
	retry := call(t, s, "POST", "inbound", m, true, 200)
	if retry["delivery"].(map[string]any)["status"] != "delivered" {
		t.Fatal("failed delivery not retried")
	}
	s.Configure(func(s *Server) { s.NextDelivery = "pending" })
	m = inbound("pending")
	one := call(t, s, "POST", "inbound", m, true, 200)
	two := call(t, s, "POST", "inbound", m, true, 200)
	if one["delivery"].(map[string]any)["receipt_id"] != two["delivery"].(map[string]any)["receipt_id"] {
		t.Fatal("pending receipt changed on duplicate")
	}
}

func TestConcludedFailureDoesNotOverwriteSuccessfulRetry(t *testing.T) {
	s := New(t)
	s.NextDelivery = "pending"
	m := inbound("retry")
	result := call(t, s, "POST", "inbound", m, true, 200)
	id := result["delivery"].(map[string]any)["receipt_id"].(string)
	failed := wire.ReceiptResponse{State: "concluded"}
	failed.Delivery.Status = "failed"
	s.SetReceipt(id, failed)
	retry := call(t, s, "POST", "inbound", m, true, 200)
	if retry["delivery"].(map[string]any)["status"] != "delivered" {
		t.Fatalf("retry: %v", retry)
	}
	s.Configure(func(s *Server) { s.NextDelivery = "partial" })
	duplicate := call(t, s, "POST", "inbound", m, true, 200)
	if duplicate["delivery"].(map[string]any)["status"] != "no_route" {
		t.Fatalf("stale receipt overwrote successful retry: %v", duplicate)
	}
	if call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200)["state"] != "concluded" {
		t.Fatal("historical receipt must remain available")
	}
}

func TestConcludedPendingRemainsObservable(t *testing.T) {
	s := New(t)
	s.NextDelivery = "pending"
	m := inbound("unresolved")
	result := call(t, s, "POST", "inbound", m, true, 200)
	id := result["delivery"].(map[string]any)["receipt_id"].(string)
	concluded := wire.ReceiptResponse{State: "concluded"}
	concluded.Delivery.Status = "pending"
	s.SetReceipt(id, concluded)
	got := call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200)
	if got["state"] != "concluded" || got["delivery"].(map[string]any)["status"] != "pending" {
		t.Fatalf("concluded pending: %v", got)
	}
	duplicate := call(t, s, "POST", "inbound", m, true, 200)
	if duplicate["delivery"].(map[string]any)["receipt_id"] != id {
		t.Fatal("duplicate lost its unresolved receipt")
	}
}

func TestReceiptRemovalAllowsReplay(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "eviction", true: "expiry"}[expire], func(t *testing.T) {
			s := New(t)
			s.NextDelivery = "pending"
			m := inbound("replay")
			first := call(t, s, "POST", "inbound", m, true, 200)
			id := first["delivery"].(map[string]any)["receipt_id"].(string)
			if expire {
				concluded := wire.ReceiptResponse{State: "concluded"}
				concluded.Delivery.Status = "delivered"
				s.SetReceipt(id, concluded)
				s.Advance(time.Hour + time.Nanosecond)
			} else {
				s.EvictReceipt(id)
			}
			s.Configure(func(s *Server) { s.NextDelivery = "pending" })
			replay := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			if replay["status"] != "pending" || replay["receipt_id"] == id || replay["receipt_id"] == nil {
				t.Fatalf("replay did not start a fresh delivery: %v (old receipt %s)", replay, id)
			}
		})
	}
}

func TestOldReceiptRemovalPreservesNewerAttempt(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "eviction", true: "expiry"}[expire], func(t *testing.T) {
			s := New(t)
			s.NextDelivery = "pending"
			m := inbound("retry")
			first := call(t, s, "POST", "inbound", m, true, 200)
			id := first["delivery"].(map[string]any)["receipt_id"].(string)
			failed := wire.ReceiptResponse{State: "concluded"}
			failed.Delivery.Status = "failed"
			s.SetReceipt(id, failed)
			s.Configure(func(s *Server) { s.NextDelivery = "pending" })
			second := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			if expire {
				s.Advance(time.Hour + time.Nanosecond)
			} else {
				s.EvictReceipt(id)
			}
			duplicate := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			if duplicate["receipt_id"] != second["receipt_id"] {
				t.Fatalf("old receipt removed newer delivery: %v, want %v", duplicate, second)
			}
		})
	}
}

func TestCallbackRejectsInvalidWireValueTypes(t *testing.T) {
	for _, body := range []string{`{"delivered":true,"metadata":{"parts":1}}`, `{"delivered":true,"retry_after":"1s"}`} {
		t.Run(body, func(t *testing.T) {
			s := New(t)
			s.Bindings[conversation()] = "session"
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer callback.Close()
			call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL}, true, 201)
			result := call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: conversation()}, true, 200)
			if result["Receipt"].(map[string]any)["Delivered"] != false || result["TranscriptEntry"] != nil {
				t.Fatalf("malformed receipt accepted: %v", result)
			}
		})
	}
}

func TestOutboundCallbackAndTranscript(t *testing.T) {
	s := New(t)
	s.Bindings[conversation()] = "session"
	callbackStatus := 200
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/adapter/publish" {
			t.Errorf("callback path: %s", r.URL.Path)
		}
		var p wire.PublishRequest
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p.Conversation.ScopeID != "examplecity" || p.IdempotencyKey != "stable" || p.Text != "hello" {
			t.Errorf("callback: %+v", p)
		}
		w.WriteHeader(callbackStatus)
		_ = json.NewEncoder(w).Encode(wire.PublishReceipt{MessageID: "spaces/room/messages/one", Conversation: p.Conversation, Delivered: true, RetryAfter: int64(time.Second), Metadata: map[string]string{"parts": "1"}})
	}))
	defer callback.Close()
	call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL + "/adapter"}, true, 201)
	req := wire.OutboundRequest{SessionID: "session", Conversation: conversation(), Text: "hello", IdempotencyKey: "stable", ReplyToMessageID: "spaces/room/threads/one"}
	result := call(t, s, "POST", "outbound", req, true, 200)
	if result["Receipt"].(map[string]any)["Delivered"] != true || result["TranscriptEntry"] == nil || result["receipt"] != nil {
		t.Fatalf("outbound shape: %v", result)
	}
	entry := result["TranscriptEntry"].(map[string]any)
	for _, key := range []string{"ID", "SchemaVersion", "Conversation", "Sequence", "Kind", "Provenance", "ProviderMessageID", "Actor", "Text", "ExplicitTarget", "ReplyToMessageID", "Attachments", "SourceSessionID", "CreatedAt", "Metadata"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("transcript missing %s: %v", key, entry)
		}
	}
	if len(entry) != 15 || entry["SourceSessionID"] != "session" || entry["ProviderMessageID"] != "spaces/room/messages/one" || entry["Text"] != "hello" {
		t.Errorf("transcript shape: %v", entry)
	}
	dc, ok := result["DeliveryContext"].(map[string]any)
	if !ok || len(dc) != 9 || dc["SessionID"] != "session" || dc["LastMessageID"] != "spaces/room/messages/one" || dc["SourceSessionID"] != "session" || dc["LastPublishedAt"] == nil || dc["BindingGeneration"] == nil || dc["Conversation"] == nil || dc["ID"] == nil || dc["SchemaVersion"] == nil {
		t.Errorf("delivery context shape: %v", result)
	}
	again := call(t, s, "POST", "outbound", req, true, 200)
	if again["TranscriptEntry"].(map[string]any)["ID"] != entry["ID"] {
		t.Fatal("duplicate provider message appended a new transcript entry")
	}
	s.Configure(func(s *Server) { s.FailTranscript = true })
	failed := call(t, s, "POST", "outbound", req, true, 200)
	if failed["Receipt"].(map[string]any)["Delivered"] != true || failed["TranscriptEntry"] != nil {
		t.Fatalf("transcript failure: %v", failed)
	}
	if failed["DeliveryContext"] == nil {
		t.Fatal("transcript failure lost delivery context")
	}
	s.Configure(func(s *Server) { s.FailTranscript = false })
	callbackStatus = 429
	discarded := call(t, s, "POST", "outbound", req, true, 200)
	receipt := discarded["Receipt"].(map[string]any)
	if receipt["Delivered"] != false || receipt["MessageID"] == "spaces/room/messages/one" || receipt["RetryAfter"] == float64(time.Second) || discarded["TranscriptEntry"] != nil {
		t.Fatalf("non-2xx body used: %v", discarded)
	}
	if got := s.Published(); len(got) != 4 || got[0].ReplyToMessageID != req.ReplyToMessageID || got[2].IdempotencyKey != "stable" {
		t.Fatalf("published callbacks: %+v", got)
	}
}

func TestUndeliveredCallbackReceiptPreservesHints(t *testing.T) {
	s := New(t)
	s.Bindings[conversation()] = "session"
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.PublishReceipt{Conversation: conversation(), FailureKind: "rate_limited", RetryAfter: int64(2 * time.Second), Metadata: map[string]string{"parts": "0"}})
	}))
	defer callback.Close()
	call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL}, true, 201)
	result := call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: conversation()}, true, 200)
	receipt := result["Receipt"].(map[string]any)
	if receipt["Delivered"] != false || receipt["FailureKind"] != "rate_limited" || receipt["RetryAfter"] != float64(2*time.Second) || result["TranscriptEntry"] != nil {
		t.Fatalf("hints: %v", result)
	}
	if len(s.Published()) != 1 {
		t.Fatal("gc retried the callback")
	}
}

func TestCallbackFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   string
	}{{401, "auth"}, {403, "auth"}, {404, "not_found"}, {429, "rate_limited"}, {400, "permanent"}, {409, "permanent"}, {422, "permanent"}, {500, "transient"}, {503, "transient"}, {0, "transient"}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			s := New(t)
			s.Bindings[conversation()] = "session"
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"delivered":true,"message_id":"ignored","failure_kind":"ignored","retry_after":100,"metadata":{"secret":"ignored"}}`)
			}))
			defer callback.Close()
			if tc.status == 0 {
				callback.Close()
			}
			call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL}, true, 201)
			result := call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: conversation()}, true, 200)
			receipt := result["Receipt"].(map[string]any)
			if receipt["FailureKind"] != tc.kind || receipt["Delivered"] != false || receipt["MessageID"] != "" || receipt["RetryAfter"] != float64(0) || receipt["Metadata"] != nil || result["TranscriptEntry"] != nil {
				t.Fatalf("callback failure: %v, want %s without body data", result, tc.kind)
			}
		})
	}
}

func TestOutboundRequiresSessionBindingAndAdapter(t *testing.T) {
	for _, invalid := range []string{"session_id", "scope_id", "adapter", "binding", "owner"} {
		t.Run(invalid, func(t *testing.T) {
			s := New(t)
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(wire.PublishReceipt{Conversation: conversation(), Delivered: true})
			}))
			defer callback.Close()
			if invalid != "adapter" {
				call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL}, true, 201)
			}
			if invalid != "binding" {
				s.Bindings[conversation()] = "session"
			}
			req := wire.OutboundRequest{SessionID: "session", Conversation: conversation()}
			if invalid == "owner" {
				req.SessionID = "other-session"
			}
			if invalid == "session_id" {
				req.SessionID = ""
			}
			if invalid == "scope_id" {
				req.Conversation.ScopeID = ""
			}
			got := call(t, s, "POST", "outbound", req, true, 422)
			if got["type"] != "urn:gascity:error:validation-failed" || got["title"] != "Validation Failed" || got["status"] != float64(422) || got["code"] != "validation-failed" || got["detail"] == nil || got["error"] != nil {
				t.Errorf("problem envelope: %v", got)
			}
			if invalid == "binding" && got["detail"] != "no active binding for conversation gchat/spaces/room" {
				t.Errorf("no binding detail: %v", got)
			}
			if invalid == "owner" && got["detail"] != `session "other-session" does not own binding for conversation gchat/spaces/room (bound to session)` {
				t.Errorf("wrong owner detail: %v", got)
			}
			if len(s.Published()) != 0 {
				t.Fatal("invalid outbound invoked callback")
			}
		})
	}
}

func TestCallbackURLIsConcatenatedVerbatim(t *testing.T) {
	s := New(t)
	s.Bindings[conversation()] = "session"
	paths := make(chan string, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		_ = json.NewEncoder(w).Encode(wire.PublishReceipt{Conversation: conversation(), Delivered: true})
	}))
	defer callback.Close()
	call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL + "/adapter/"}, true, 201)
	call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: conversation()}, true, 200)
	if path := <-paths; path != "/adapter//publish" {
		t.Fatalf("callback path %q, want verbatim /adapter//publish", path)
	}
}

func TestReceiptWireShape(t *testing.T) {
	for _, status := range []string{"pending", "delivered", "failed", "partial", "no_route", "unknown"} {
		t.Run(status, func(t *testing.T) {
			s := New(t)
			id := "unknown-id"
			if status != "unknown" {
				s.NextDelivery = status
				result := call(t, s, "POST", "inbound", inbound(status), true, 200)
				var ok bool
				id, ok = result["delivery"].(map[string]any)["receipt_id"].(string)
				if !ok || id == "" {
					t.Fatalf("inbound %s missing receipt_id: %v", status, result)
				}
			}
			poll := call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200)
			if poll["receipt_id"] != id {
				t.Errorf("poll omitted receipt_id: %v", poll)
			}
			if status == "pending" || status == "unknown" {
				if _, present := poll["delivery"]; present || poll["state"] != status {
					t.Fatalf("unconcluded receipt must omit delivery: %v", poll)
				}
			} else {
				delivery, ok := poll["delivery"].(map[string]any)
				if !ok || poll["state"] != "concluded" || delivery["status"] != status || delivery["receipt_id"] != id {
					t.Fatalf("concluded receipt: %v", poll)
				}
			}
		})
	}
}

func TestSynchronousDeliveryReceiptExpires(t *testing.T) {
	s := New(t)
	m := inbound("delivered")
	first := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
	s.Advance(time.Hour + time.Nanosecond)
	s.Configure(func(s *Server) { s.NextDelivery = "pending" })
	replay := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
	if replay["status"] != "pending" || replay["receipt_id"] == first["receipt_id"] {
		t.Fatalf("expired synchronous delivery prevented replay: %v", replay)
	}
}

func TestInboundRetriesOnlyOutcomesWithoutAWholeDelivery(t *testing.T) {
	for _, status := range []string{"no_route", "failed", "partial", "pending", "delivered"} {
		t.Run(status, func(t *testing.T) {
			s := New(t)
			s.NextDelivery = status
			m := inbound("same")
			first := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			s.Configure(func(s *Server) { s.NextDelivery = "pending" })
			retry := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			if status == "delivered" {
				if retry["receipt_id"] == first["receipt_id"] || retry["status"] != "no_route" {
					t.Fatalf("suppression needs a new no_route receipt: %v", retry)
				}
				id := retry["receipt_id"].(string)
				poll := call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200)
				if poll["state"] != "concluded" || poll["delivery"].(map[string]any)["status"] != "no_route" {
					t.Fatalf("suppression not concluded: %v", poll)
				}
				s.EvictReceipt(id)
				again := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
				if again["status"] != "no_route" || again["receipt_id"] == id {
					t.Fatalf("suppression changed delivery evidence: %v", again)
				}
			} else if status == "pending" {
				if retry["receipt_id"] != first["receipt_id"] || retry["status"] != status {
					t.Fatalf("held delivery was retried: %v", retry)
				}
			} else if retry["receipt_id"] == first["receipt_id"] || retry["status"] != "pending" {
				t.Fatalf("retryable %s delivery was held: %v", status, retry)
			}
		})
	}
}

func TestRegistrationResponseAndValidation(t *testing.T) {
	for _, missing := range []string{"", "provider", "account_id"} {
		t.Run("missing/"+missing, func(t *testing.T) {
			s := New(t)
			reg := wire.Registration{Provider: "gchat", AccountID: "example", Name: "example adapter", CallbackURL: "http://127.0.0.1:1/adapter"}
			status := 201
			if missing != "" {
				status = 422
			}
			if missing == "provider" {
				reg.Provider = ""
			}
			if missing == "account_id" {
				reg.AccountID = ""
			}
			got := call(t, s, "POST", "adapters", reg, true, status)
			if status == 201 && (got["status"] != "registered" || got["provider"] != reg.Provider || got["account_id"] != reg.AccountID || got["name"] != reg.Name) {
				t.Fatalf("registration response: %v", got)
			}
		})
	}
}

func TestUnknownReceiptReleasesOnlyItsOwnAttempt(t *testing.T) {
	for _, newer := range []bool{false, true} {
		t.Run(fmt.Sprint(newer), func(t *testing.T) {
			s := New(t)
			s.NextDelivery = "pending"
			m := inbound("unknown")
			first := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			id := first["receipt_id"].(string)
			var newerID any
			if newer {
				failed := wire.ReceiptResponse{State: "concluded"}
				failed.Delivery.Status = "failed"
				s.SetReceipt(id, failed)
				s.Configure(func(s *Server) { s.NextDelivery = "pending" })
				newerID = call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)["receipt_id"]
			}
			s.SetReceipt(id, wire.ReceiptResponse{State: "unknown"})
			s.Configure(func(s *Server) { s.NextDelivery = "pending" })
			replay := call(t, s, "POST", "inbound", m, true, 200)["delivery"].(map[string]any)
			if replay["receipt_id"] == id || (newer && replay["receipt_id"] != newerID) {
				t.Fatalf("unknown receipt did not release only its attempt: %v", replay)
			}
		})
	}
}

func TestCallbackRequiresBoundedCompleteJSON(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"trailing garbage", `{"delivered":true}garbage`},
		{"second value", `{"delivered":true}{}`},
		{"oversize", `{"delivered":true,"message_id":"` + strings.Repeat("x", 1<<20) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(t)
			s.Bindings[conversation()] = "session"
			callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer callback.Close()
			call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL}, true, 201)
			result := call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: conversation()}, true, 200)
			receipt := result["Receipt"].(map[string]any)
			if receipt["Delivered"] != false || receipt["FailureKind"] != "transient" || result["TranscriptEntry"] != nil {
				t.Fatalf("invalid callback body recorded delivery (delivered=%v, failure=%v)", receipt["Delivered"], receipt["FailureKind"])
			}
		})
	}
}

func TestReceiptLookupIsScopedToIssuingCity(t *testing.T) {
	for _, status := range []string{"pending", "delivered", "suppressed"} {
		t.Run(status, func(t *testing.T) {
			s := New(t)
			if status == "pending" {
				s.NextDelivery = status
			}
			m := inbound("city")
			first := call(t, s, "POST", "inbound", m, true, 200)
			if status == "suppressed" {
				first = call(t, s, "POST", "inbound", m, true, 200)
			}
			id := first["delivery"].(map[string]any)["receipt_id"].(string)
			other := callCity(t, s, "othercity", "GET", "inbound/receipts/"+id, nil, false, 200)
			if other["state"] != "unknown" || other["delivery"] != nil {
				t.Fatalf("receipt leaked across cities: %v", other)
			}
			own := call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200)
			if own["state"] == "unknown" {
				t.Fatal("cross-city lookup changed the issuing city's receipt")
			}
		})
	}
}

func TestReceiptRetainedAtExactlyOneHour(t *testing.T) {
	s := New(t)
	s.Now = func() time.Time { return time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC) }
	first := call(t, s, "POST", "inbound", inbound("boundary"), true, 200)
	id := first["delivery"].(map[string]any)["receipt_id"].(string)
	s.Advance(time.Hour)
	if got := call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200); got["state"] != "concluded" {
		t.Fatalf("receipt expired at inclusive boundary: %v", got)
	}
	s.Advance(time.Nanosecond)
	if got := call(t, s, "GET", "inbound/receipts/"+id, nil, false, 200); got["state"] != "unknown" {
		t.Fatalf("receipt retained past boundary: %v", got)
	}
}

func TestConversationIdentityNormalization(t *testing.T) {
	s := New(t)
	c := conversation()
	c.ScopeID = " examplecity "
	c.Provider = " GCHAT "
	c.AccountID = " example "
	c.ConversationID = " spaces/room "
	c.ParentConversationID = " "
	c.Kind = " ROOM "
	s.Configure(func(s *Server) { s.Bindings[c] = "session" })
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.PublishReceipt{Conversation: conversation(), Delivered: true})
	}))
	defer callback.Close()
	call(t, s, "POST", "adapters", wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: callback.URL}, true, 201)
	for _, conv := range []wire.Conversation{conversation(), c} {
		call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "session", Conversation: conv}, true, 200)
	}
	// Bind replaces an equivalent legacy map fixture and is safe during calls.
	s.Bind(c, "new-session")
	var writers sync.WaitGroup
	writers.Go(func() {
		for range 100 {
			s.Bind(c, "new-session")
		}
	})
	for range 10 {
		call(t, s, "POST", "outbound", wire.OutboundRequest{SessionID: "new-session", Conversation: conversation()}, true, 200)
	}
	writers.Wait()
	m := inbound("normalized")
	m.Message.Conversation = c
	call(t, s, "POST", "inbound", m, true, 200)
	duplicate := call(t, s, "POST", "inbound", inbound("normalized"), true, 200)
	if duplicate["delivery"].(map[string]any)["status"] != "no_route" {
		t.Fatalf("normalized conversation was not deduplicated: %v", duplicate)
	}
}

func TestInboundMessageIDWhitespaceMatchesRealGC(t *testing.T) {
	s := New(t)
	s.Configure(func(s *Server) { s.NextDelivery = "pending" })
	first := call(t, s, "POST", "inbound", inbound(" message "), true, 200)
	second := call(t, s, "POST", "inbound", inbound("message"), true, 200)
	if first["delivery"].(map[string]any)["receipt_id"] != second["delivery"].(map[string]any)["receipt_id"] {
		t.Fatal("message id was not normalized for dedup")
	}
	call(t, s, "POST", "inbound", inbound(" \t "), true, 400)
}
func TestNotFoundAndEmptySessionProblems(t *testing.T) {
	s := New(t)
	missing := call(t, s, "GET", "absent", nil, false, 404)
	if missing["code"] != "not-found" {
		t.Fatalf("not-found problem: %v", missing)
	}
	empty := call(t, s, "POST", "outbound", wire.OutboundRequest{Conversation: conversation()}, true, 422)
	if empty["detail"] != "validation failed" {
		t.Fatalf("empty session problem: %v", empty)
	}
}
