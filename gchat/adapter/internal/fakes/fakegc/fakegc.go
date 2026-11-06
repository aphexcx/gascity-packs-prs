// Package fakegc implements the extmsg HTTP seam used by the adapter.
package fakegc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	// Outbound fixtures must populate this before publishing. Use Bind for
	// synchronized updates while clients are running.
	Bindings       map[wire.Conversation]string
	FailTranscript bool
	// CityNotRunning models the supervisor serving before its city starts.
	CityNotRunning bool
	Now            func() time.Time

	mu            sync.Mutex
	http          *httptest.Server
	client        *http.Client
	offset        time.Duration
	sequence      int
	registrations map[string]wire.Registration
	inbound       []wire.InboundMessage
	published     []wire.PublishRequest
	transcripts   map[string]*transcriptEntry
	dedup         map[string]wire.InboundResponse
	receiptKeys   map[string]string
	receiptCities map[string]string
	concludedAt   map[string]time.Time
}

func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Now: time.Now, Receipts: map[string]wire.ReceiptResponse{},
		Bindings:      map[wire.Conversation]string{},
		registrations: map[string]wire.Registration{}, dedup: map[string]wire.InboundResponse{},
		receiptKeys: map[string]string{}, receiptCities: map[string]string{}, concludedAt: map[string]time.Time{},
		transcripts: map[string]*transcriptEntry{}, client: &http.Client{Timeout: 30 * time.Second}}
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

// ForgetRegistrations simulates a controller restart's loss of its in-memory
// adapter registry while preserving durable bindings and the request history.
func (s *Server) ForgetRegistrations() {
	s.Configure(func(s *Server) { s.registrations = map[string]wire.Registration{} })
}

func (s *Server) now() time.Time { return s.Now().Add(s.offset) }

// Bind sets the owning session, replacing any equivalent conversation binding.
func (s *Server) Bind(conv wire.Conversation, sessionID string) {
	s.Configure(func(s *Server) {
		conv = normalizeConversation(conv)
		for existing := range s.Bindings {
			if normalizeConversation(existing) == conv {
				delete(s.Bindings, existing)
			}
		}
		s.Bindings[conv] = sessionID
	})
}

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

// SetReceipt changes a receipt previously issued by POST /inbound. A new ID
// has no issuing city and remains unknown over HTTP until an inbound creates it.
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
	delete(s.receiptCities, id)
}

func (s *Server) observeReceipts() {
	for id, receipt := range s.Receipts {
		if receipt.State == "unknown" {
			s.removeReceipt(id)
			continue
		}
		if receipt.State != "concluded" {
			delete(s.concludedAt, id)
			continue
		}
		when, ok := s.concludedAt[id]
		if !ok {
			s.concludedAt[id] = s.now()
			when = s.now()
		}
		if s.now().Sub(when) > time.Hour {
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
	code, title := "invalid-request", "Invalid Request"
	switch status {
	case 403:
		code, title = "forbidden", "Forbidden"
	case 404:
		code, title = "not-found", "Not Found"
	case 422:
		code, title = "validation-failed", "Validation Failed"
	case 500:
		code, title = "internal", "Internal Server Error"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:gascity:error:" + code, "title": title, "status": status, "detail": message, "code": code})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if err := json.NewDecoder(r.Body).Decode(value); err != nil {
		fail(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// receiptResponse is the HTTP shape; the wire package's value-shaped fixture
// type remains convenient for SetReceipt, but must not emit empty delivery data.
type receiptResponse struct {
	ReceiptID string           `json:"receipt_id"`
	State     string           `json:"state"`
	Delivery  *receiptDelivery `json:"delivery,omitempty"`
}

type receiptDelivery struct {
	Status    string `json:"status"`
	ReceiptID string `json:"receipt_id"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v0" || parts[1] != "city" || parts[3] != "extmsg" {
		fail(w, 404, "route not found")
		return
	}
	city := parts[2]
	s.mu.Lock()
	notRunning := s.CityNotRunning
	s.mu.Unlock()
	if notRunning {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "urn:gascity:error:city-not-found", "title": "City Not Found",
			"status": 404, "code": "city-not-found",
			"detail": "not_found: city not found or not running: " + city,
		})
		return
	}
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
			fail(w, 422, "provider, account_id and absolute callback_url are required")
			return
		}
		if registration.Name == "" {
			registration.Name = registration.Provider + "/" + registration.AccountID
		}
		s.registrations[registrationKey(city, registration.Provider, registration.AccountID)] = registration
		respond(w, 201, map[string]any{"status": "registered", "provider": registration.Provider, "account_id": registration.AccountID, "name": registration.Name})
	case path == "inbound" && r.Method == http.MethodPost:
		var envelope wire.InboundEnvelope
		if !decode(w, r, &envelope) {
			return
		}
		message := envelope.Message
		message.ProviderMessageID = strings.TrimSpace(message.ProviderMessageID)
		message.Conversation = normalizeConversation(message.Conversation)
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
		if previous, ok := s.dedup[key]; ok && (previous.Delivery.Status == "pending" || previous.Delivery.Status == "delivered") {
			if previous.Delivery.Status == "delivered" {
				// Suppression has its own receipt, without replacing the evidence
				// that a whole copy of this message was already delivered.
				s.sequence++
				id := fmt.Sprintf("receipt-%d", s.sequence)
				previous.Delivery.Status = "no_route"
				previous.Delivery.ReceiptID = id
				receipt := wire.ReceiptResponse{State: "concluded"}
				receipt.Delivery.Status = "no_route"
				s.Receipts[id] = receipt
				s.receiptCities[id] = city
				s.concludedAt[id] = s.now()
			}
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
		s.sequence++
		id := fmt.Sprintf("receipt-%d", s.sequence)
		response.Delivery.ReceiptID = id
		s.receiptKeys[id] = key
		s.receiptCities[id] = city
		receipt := wire.ReceiptResponse{State: "pending"}
		if status == "pending" {
			s.Receipts[id] = receipt
		} else {
			receipt.State = "concluded"
			receipt.Delivery.Status = status
			s.Receipts[id] = receipt
			s.concludedAt[id] = s.now()
		}
		s.dedup[key] = response
		respond(w, 200, response)
	case strings.HasPrefix(path, "inbound/receipts/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "inbound/receipts/")
		receipt, ok := s.Receipts[id]
		if !ok || s.receiptCities[id] != city {
			receipt.State = "unknown"
		}
		response := receiptResponse{ReceiptID: id, State: receipt.State}
		if receipt.State == "concluded" {
			response.Delivery = &receiptDelivery{Status: receipt.Delivery.Status, ReceiptID: id}
		}
		respond(w, 200, response)
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

func normalizeConversation(c wire.Conversation) wire.Conversation {
	c.ScopeID = strings.TrimSpace(c.ScopeID)
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.AccountID = strings.TrimSpace(c.AccountID)
	c.ConversationID = strings.TrimSpace(c.ConversationID)
	c.ParentConversationID = strings.TrimSpace(c.ParentConversationID)
	c.Kind = strings.ToLower(strings.TrimSpace(c.Kind))
	return c
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

// These untagged records mirror gc's public OutboundResult fields.
type deliveryContext struct {
	ID                string
	SchemaVersion     int
	SessionID         string
	Conversation      wire.Conversation
	BindingGeneration int64
	LastPublishedAt   time.Time
	LastMessageID     string
	SourceSessionID   string
	Metadata          map[string]string
}

type transcriptEntry struct {
	ID                string
	SchemaVersion     int
	Conversation      wire.Conversation
	Sequence          int64
	Kind              string
	Provenance        string
	ProviderMessageID string
	Actor             wire.Actor
	Text              string
	ExplicitTarget    string
	ReplyToMessageID  string
	Attachments       []wire.Attachment
	SourceSessionID   string
	CreatedAt         time.Time
	Metadata          map[string]string
}

func (s *Server) outbound(w http.ResponseWriter, r *http.Request, city string) {
	var request wire.OutboundRequest
	if !decode(w, r, &request) {
		return
	}
	request.Conversation = normalizeConversation(request.Conversation)
	if request.SessionID == "" {
		fail(w, 422, "validation failed")
		return
	}
	if !validConversation(request.Conversation) {
		fail(w, 422, "session_id and conversation including scope_id are required")
		return
	}
	callback := wire.PublishRequest{SessionID: request.SessionID, Conversation: request.Conversation, Text: request.Text, ReplyToMessageID: request.ReplyToMessageID, IdempotencyKey: request.IdempotencyKey}
	s.mu.Lock()
	registration, ok := s.registrations[registrationKey(city, request.Conversation.Provider, request.Conversation.AccountID)]
	owner := ""
	for conv, session := range s.Bindings {
		if normalizeConversation(conv) == request.Conversation {
			owner = session
			break
		}
	}
	if ok && owner == request.SessionID {
		s.published = append(s.published, callback)
	}
	s.mu.Unlock()
	if owner == "" {
		fail(w, 422, fmt.Sprintf("no active binding for conversation %s/%s", request.Conversation.Provider, request.Conversation.ConversationID))
		return
	}
	if owner != request.SessionID {
		fail(w, 422, fmt.Sprintf("session %q does not own binding for conversation %s/%s (bound to %s)", request.SessionID, request.Conversation.Provider, request.Conversation.ConversationID, owner))
		return
	}
	if !ok {
		fail(w, 422, fmt.Sprintf("no adapter for %s/%s", request.Conversation.Provider, request.Conversation.AccountID))
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
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			var decoded wire.PublishReceipt
			if readErr == nil && json.Unmarshal(body, &decoded) == nil {
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
		DeliveryContext *deliveryContext
		TranscriptEntry *transcriptEntry
	}{
		Receipt: outboundReceipt{MessageID: receipt.MessageID, Conversation: receipt.Conversation, Delivered: receipt.Delivered, FailureKind: receipt.FailureKind, RetryAfter: receipt.RetryAfter, Metadata: receipt.Metadata},
	}
	s.mu.Lock()
	failTranscript := s.FailTranscript
	now := s.now()
	if receipt.Delivered {
		result.DeliveryContext = &deliveryContext{SessionID: request.SessionID, Conversation: request.Conversation, BindingGeneration: 1, LastPublishedAt: now, LastMessageID: receipt.MessageID, SourceSessionID: request.SessionID}
	}
	if receipt.Delivered && !failTranscript {
		keyData, _ := json.Marshal(struct {
			City         string
			Conversation wire.Conversation
			MessageID    string
		}{city, request.Conversation, receipt.MessageID})
		key := string(keyData)
		result.TranscriptEntry = s.transcripts[key]
		if result.TranscriptEntry == nil || receipt.MessageID == "" {
			s.sequence++
			result.TranscriptEntry = &transcriptEntry{ID: fmt.Sprintf("transcript-%d", s.sequence), SchemaVersion: 1, Conversation: request.Conversation, Sequence: int64(s.sequence), Kind: "outbound", Provenance: "live", ProviderMessageID: receipt.MessageID, Text: request.Text, SourceSessionID: request.SessionID, CreatedAt: now}
			if receipt.MessageID != "" {
				s.transcripts[key] = result.TranscriptEntry
			}
		}
	}
	s.mu.Unlock()
	respond(w, 200, result)
}
