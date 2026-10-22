// Package fakegc implements the extmsg HTTP seam used by the adapter.
package fakegc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/wire"
)

// Server scripts one inbound delivery at a time. NextDelivery is consumed by the
// next non-duplicate inbound request; an empty script defaults to delivered.
// Set fields before requests start, or use Configure while clients are running.
// Configure callbacks must not call Server methods. Use SetReceipt to timestamp
// a conclusion at the moment it is scripted, including before a polling client
// observes it. Direct Receipts edits are timestamped on the next request.
type Server struct {
	NextDelivery string
	Receipts     map[string]wire.ReceiptResponse
	// Bindings maps each active conversation binding to its owning session.
	// Outbound fixtures must populate this explicitly before publishing.
	Bindings       map[wire.Conversation]string
	FailTranscript bool
	Now            func() time.Time

	mu            sync.Mutex
	http          *httptest.Server
	client        *http.Client
	offset        time.Duration
	sequence      int
	registrations map[string]wire.Registration
	inbound       []wire.InboundMessage
	published     []wire.PublishRequest
	dedup         map[string]wire.InboundResponse
	receiptKeys   map[string]string
	concludedAt   map[string]time.Time
}

func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Now: time.Now, Receipts: map[string]wire.ReceiptResponse{},
		Bindings:      map[wire.Conversation]string{},
		registrations: map[string]wire.Registration{}, dedup: map[string]wire.InboundResponse{},
		receiptKeys: map[string]string{}, concludedAt: map[string]time.Time{},
		client: &http.Client{Timeout: 30 * time.Second}}
	s.http = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) URL() string { return s.http.URL }
func (s *Server) Close()      { s.http.Close() }
func (s *Server) Configure(fn func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
	s.observeReceipts()
}
func (s *Server) Advance(d time.Duration) {
	s.Configure(func(s *Server) { s.observeReceipts(); s.offset += d })
}
func (s *Server) now() time.Time { return s.Now().Add(s.offset) }

// Inbound returns all accepted POSTs, including duplicate attempts, in order.
func (s *Server) Inbound() []wire.InboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.inbound)
}

// Published returns every callback request made, in order, including failures.
func (s *Server) Published() []wire.PublishRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.published)
}

// clone prevents a caller mutating attachment slices or metadata in the log.
func clone[T any](value T) T {
	b, _ := json.Marshal(value)
	var copy T
	_ = json.Unmarshal(b, &copy)
	return copy
}

func (s *Server) SetReceipt(id string, receipt wire.ReceiptResponse) {
	s.Configure(func(s *Server) { s.Receipts[id] = receipt })
}

func (s *Server) EvictReceipt(id string) {
	s.Configure(func(s *Server) { s.removeReceipt(id) })
}

// removeReceipt releases only the delivery attempt indexed by this receipt.
// Historical failed attempts may coexist with a newer pending or delivered one.
func (s *Server) removeReceipt(id string) {
	if key := s.receiptKeys[id]; s.dedup[key].Delivery.ReceiptID == id {
		delete(s.dedup, key)
	}
	delete(s.Receipts, id)
	delete(s.concludedAt, id)
	delete(s.receiptKeys, id)
}

func (s *Server) observeReceipts() {
	for id, receipt := range s.Receipts {
		if receipt.State != "concluded" {
			delete(s.concludedAt, id)
			continue
		}
		when, ok := s.concludedAt[id]
		if !ok {
			s.concludedAt[id] = s.now()
			when = s.now()
		}
		if s.now().Sub(when) >= time.Hour {
			s.removeReceipt(id)
			continue
		}
		if key := s.receiptKeys[id]; key != "" {
			response := s.dedup[key]
			// A retained historical receipt must not overwrite a later attempt.
			if response.Delivery.ReceiptID != id {
				continue
			}
			response.Delivery.Status = receipt.Delivery.Status
			s.dedup[key] = response
		}
	}
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]any{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		fail(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v0" || parts[1] != "city" || parts[3] != "extmsg" {
		fail(w, 404, "route not found")
		return
	}
	city := parts[2]
	path := strings.Join(parts[4:], "/")
	if r.Method != http.MethodGet && r.Header.Get(wire.RequestHeader) == "" {
		fail(w, 403, "X-GC-Request is required")
		return
	}
	if path == "outbound" && r.Method == http.MethodPost {
		s.outbound(w, r, city)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeReceipts()
	switch {
	case path == "adapters" && r.Method == http.MethodPost:
		var registration wire.Registration
		if !decode(w, r, &registration) {
			return
		}
		callback, err := url.Parse(registration.CallbackURL)
		if registration.Provider == "" || registration.AccountID == "" || err != nil || callback.Host == "" || (callback.Scheme != "http" && callback.Scheme != "https") {
			fail(w, 400, "provider, account_id and absolute callback_url are required")
			return
		}
		s.registrations[registrationKey(city, registration.Provider, registration.AccountID)] = registration
		respond(w, 200, map[string]any{"registered": true})
	case path == "inbound" && r.Method == http.MethodPost:
		var envelope wire.InboundEnvelope
		if !decode(w, r, &envelope) {
			return
		}
		message := envelope.Message
		if !validConversation(message.Conversation) || message.ProviderMessageID == "" {
			fail(w, 400, "message identity and conversation including scope_id are required")
			return
		}
		s.inbound = append(s.inbound, message)
		// DedupKey is intentionally ignored; gc keys on conversation and message.
		keyBytes, _ := json.Marshal(struct {
			City         string
			Conversation wire.Conversation
			ID           string
		}{city, message.Conversation, message.ProviderMessageID})
		key := string(keyBytes)
		if previous, ok := s.dedup[key]; ok && previous.Delivery.Status != "failed" && previous.Delivery.Status != "partial" {
			respond(w, 200, previous)
			return
		}
		status := s.NextDelivery
		s.NextDelivery = ""
		if status == "" {
			status = "delivered"
		}
		response := wire.InboundResponse{}
		response.Delivery.Status = status
		if status == "pending" {
			s.sequence++
			id := fmt.Sprintf("receipt-%d", s.sequence)
			response.Delivery.ReceiptID = id
			s.Receipts[id] = wire.ReceiptResponse{State: "pending"}
			s.receiptKeys[id] = key
		}
		s.dedup[key] = response
		respond(w, 200, response)
	case strings.HasPrefix(path, "inbound/receipts/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "inbound/receipts/")
		receipt, ok := s.Receipts[id]
		if !ok {
			receipt.State = "unknown"
		}
		respond(w, 200, receipt)
	default:
		fail(w, 404, "route not found")
	}
}

func registrationKey(city, provider, account string) string {
	return city + "\x00" + provider + "\x00" + account
}
func validConversation(c wire.Conversation) bool {
	return c.ScopeID != "" && c.Provider != "" && c.AccountID != "" && c.ConversationID != "" && (c.Kind == "dm" || c.Kind == "room" || c.Kind == "thread")
}

// outboundReceipt mirrors gc's PascalCase result, which is distinct from the
// adapter's snake_case callback receipt. Keep the retry hints for HTTP 2xx only.
type outboundReceipt struct {
	MessageID    string
	Conversation wire.Conversation
	Delivered    bool
	FailureKind  string
	RetryAfter   int64
	Metadata     map[string]string
}

func (s *Server) outbound(w http.ResponseWriter, r *http.Request, city string) {
	var request wire.OutboundRequest
	if !decode(w, r, &request) {
		return
	}
	if request.SessionID == "" || !validConversation(request.Conversation) {
		fail(w, 422, "session_id and conversation including scope_id are required")
		return
	}
	callback := wire.PublishRequest{SessionID: request.SessionID, Conversation: request.Conversation, Text: request.Text, ReplyToMessageID: request.ReplyToMessageID, IdempotencyKey: request.IdempotencyKey}
	s.mu.Lock()
	registration, ok := s.registrations[registrationKey(city, request.Conversation.Provider, request.Conversation.AccountID)]
	owner := s.Bindings[request.Conversation]
	if ok && owner == request.SessionID {
		s.published = append(s.published, callback)
	}
	s.mu.Unlock()
	if !ok {
		fail(w, 422, "adapter not registered")
		return
	}
	if owner == "" || owner != request.SessionID {
		fail(w, 422, "session does not own an active conversation binding")
		return
	}
	body, _ := json.Marshal(callback)
	call, err := http.NewRequestWithContext(r.Context(), http.MethodPost, registration.CallbackURL+"/publish", bytes.NewReader(body))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set(wire.RequestHeader, "fakegc")
	response, err := s.client.Do(call)
	receipt := wire.PublishReceipt{Conversation: request.Conversation, FailureKind: "transient"}
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			var decoded wire.PublishReceipt
			if err := json.NewDecoder(response.Body).Decode(&decoded); err == nil {
				receipt = decoded
			}
		} else {
			// gc discards error bodies, including any callback retry hints.
			switch response.StatusCode {
			case 401, 403:
				receipt.FailureKind = "auth"
			case 404:
				receipt.FailureKind = "not_found"
			case 429:
				receipt.FailureKind = "rate_limited"
			default:
				if response.StatusCode >= 400 && response.StatusCode < 500 {
					receipt.FailureKind = "permanent"
				}
			}
		}
	}
	result := struct {
		Receipt         outboundReceipt
		TranscriptEntry *map[string]any
	}{
		Receipt: outboundReceipt{MessageID: receipt.MessageID, Conversation: receipt.Conversation, Delivered: receipt.Delivered, FailureKind: receipt.FailureKind, RetryAfter: receipt.RetryAfter, Metadata: receipt.Metadata},
	}
	s.mu.Lock()
	failTranscript := s.FailTranscript
	s.mu.Unlock()
	if receipt.Delivered && !failTranscript {
		entry := map[string]any{"session_id": request.SessionID, "message_id": receipt.MessageID, "text": request.Text}
		result.TranscriptEntry = &entry
	}
	respond(w, 200, result)
}
