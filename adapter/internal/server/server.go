// Package server serves the local command surface and gc's publish callback.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/markdown"
	"gchat-adapter/internal/wire"
	"github.com/BurntSushi/toml"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

const publishBudget = 25 * time.Second
const maxFailureLogKeys = 1024

type Health struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type Options struct {
	Config *config.Config
	Ledger *ledger.Ledger
	// LedgerMu must be shared with inbound, catch-up, and the subscription manager.
	// Consumers must release it for I/O and between batches; continuous re-locking
	// can starve the server's deadline-aware TryLock polling.
	LedgerMu     *sync.Mutex
	LedgerPath   string
	SettingsPath string
	Chat         *chat.Client
	GC           *gcclient.Client
	Logger       *slog.Logger
	// Poll is inbound.Processor.Poll; replay persists an intent before invoking it.
	Poll func(context.Context) error
	// LastEvent returns the app's last observed event time, including skipped
	// events. It must be safe to call concurrently with event processing.
	LastEvent func() time.Time
	// SubscriptionStatus is a nonblocking snapshot from the subscription manager.
	SubscriptionStatus func() (int, time.Time, ledger.SubscriptionState, error)
	// StateChanged asks the app to reconcile after a restore or reset. The app
	// must quiesce its other ledger consumers while invoking the state route.
	// It runs under the state gate and must tolerate retries: after an error,
	// the same /state request retries reconciliation without replacing the ledger.
	StateChanged func(context.Context) error
}

type Server struct {
	opts            Options
	mu              sync.RWMutex // config snapshot, clients, health, poll
	cfg             config.Config
	health          Health
	chat            *chat.Client
	gc              *gcclient.Client
	poll            func(context.Context) error
	mux             *http.ServeMux
	ctx             context.Context
	cancel          context.CancelFunc
	regGate         chan struct{}
	registering     bool
	regFailed       bool // protected by regGate
	logMu           sync.Mutex
	publishFailures map[string]time.Time
	stateGate       sync.RWMutex
	pendingState    *stateRequest // protected by stateGate
	keysMu          sync.Mutex
	keys            map[string]*keyLock
}

type keyLock struct {
	gate  chan struct{}
	users int
}

func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.Ledger == nil || opts.LedgerMu == nil || opts.LedgerPath == "" {
		return nil, errors.New("server: config, ledger, shared mutex and ledger path are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opts: opts, cfg: *opts.Config, health: Health{State: "initializing", Reason: "initialization in progress"}, chat: opts.Chat, gc: opts.GC, poll: opts.Poll, ctx: ctx, cancel: cancel, regGate: make(chan struct{}, 1), keys: map[string]*keyLock{}, publishFailures: map[string]time.Time{}, mux: http.NewServeMux()}
	s.cfg.Settings.Spaces = append([]config.Space(nil), opts.Config.Settings.Spaces...)
	s.cfg.Settings.Identities = append([]config.Identity(nil), opts.Config.Settings.Identities...)
	if s.gc != nil {
		s.gc.SetConnectionRefusedHandler(s.ObserveGCError)
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("POST /publish", s.publishHTTP)
	s.mux.HandleFunc("POST /commands/publish", s.commandHTTP)
	s.mux.HandleFunc("POST /commands/upload", s.commandHTTP)
	s.mux.HandleFunc("POST /react", s.react)
	s.mux.HandleFunc("GET /read", s.read)
	s.mux.HandleFunc("GET /spaces", s.spaces)
	s.mux.HandleFunc("POST /spaces", s.spaces)
	s.mux.HandleFunc("GET /status", s.status)
	s.mux.HandleFunc("POST /resolve", s.resolve)
	s.mux.HandleFunc("POST /state", s.state)
	return s, nil
}

func (s *Server) Close()             { s.cancel() }
func (s *Server) SetHealth(h Health) { s.mu.Lock(); defer s.mu.Unlock(); s.health = h }

// Drain waits for requests and an already-running recovery sweep to finish.
// The app sets initializing health before calling it, so no new delivery starts.
// Health and status remain reachable while credential clients are replaced.
func (s *Server) Drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.stateGate.TryLock() {
			s.stateGate.Unlock()
			return nil
		}
		t := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// SetConfig publishes a validated config reload to future requests. The app
// separately rebuilds clients and reloads inbound policy using the same config.
func (s *Server) SetConfig(cfg config.Config) {
	cfg.Settings.Spaces = append([]config.Space(nil), cfg.Settings.Spaces...)
	cfg.Settings.Identities = append([]config.Identity(nil), cfg.Settings.Identities...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}
func (s *Server) SetClients(c *chat.Client, g *gcclient.Client) {
	if g != nil {
		g.SetConnectionRefusedHandler(s.ObserveGCError)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chat = c
	s.gc = g
}
func (s *Server) SetPoll(p func(context.Context) error) { s.mu.Lock(); defer s.mu.Unlock(); s.poll = p }
func (s *Server) Health() Health                        { s.mu.RLock(); defer s.mu.RUnlock(); return s.health }
func (s *Server) clients() (*chat.Client, *gcclient.Client) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chat, s.gc
}
func (s *Server) configuration() config.Config { s.mu.RLock(); defer s.mu.RUnlock(); return s.cfg }

func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("unix", s.configuration().Secrets.ServiceSocket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.configuration().Secrets.ServiceSocket, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Listen binds before asynchronous initialization. Serve shuts down on ctx.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	h := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: publishBudget, IdleTimeout: time.Minute}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = h.Close()
			s.Close()
		case <-done:
		}
	}()
	err := h.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), publishBudget)
	defer cancel()
	if r.URL.Path != "/state" && r.URL.Path != "/healthz" && r.URL.Path != "/status" {
		if !s.stateGate.TryRLock() {
			if r.URL.Path == "/publish" {
				var p wire.PublishRequest
				_ = body(w, r, &p)
				_ = s.normalize(&p)
				err := errors.New("state recovery in progress")
				receipt := s.failure(p, err)
				s.logPublish(p, receipt, err)
				respond(w, 200, receipt)
			} else {
				problem(w, 503, errors.New("state recovery in progress"))
			}
			return
		}
		defer s.stateGate.RUnlock()
		if s.Health().State == "recovery-required" && r.URL.Path != "/publish" {
			problem(w, 503, errors.New("ledger recovery required"))
			return
		}
	}
	s.mux.ServeHTTP(w, r.WithContext(ctx))
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}
func body(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	h := s.Health()
	v := "degraded"
	if h.State == "ready" {
		v = "ok"
	}
	w.Header().Set("X-GC-Health", v)
	respond(w, 200, h)
}
func (s *Server) available() error {
	h := s.Health()
	if h.State != "ready" && h.State != "catching-up" {
		return fmt.Errorf("%s: %s", h.State, h.Reason)
	}
	c, _ := s.clients()
	if c == nil {
		return errors.New("chat client is initializing")
	}
	return nil
}

func copyLedger(l *ledger.Ledger) (*ledger.Ledger, error) {
	return l.Clone(), nil
}

// Other consumers share a sync.Mutex, so use TryLock with a cancellable wait
// instead of spawning a waiter that could outlive the request and take the lock.
// This is bounded, not FIFO: consumers must release for I/O and between batches.
// Fair acquisition would require changing the mutex shared by every consumer.
func (s *Server) lockLedger(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.opts.LedgerMu.TryLock() {
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Server) snapshot(ctx context.Context) (*ledger.Ledger, error) {
	if err := s.lockLedger(ctx); err != nil {
		return nil, err
	}
	defer s.opts.LedgerMu.Unlock()
	return copyLedger(s.opts.Ledger)
}
func (s *Server) update(ctx context.Context, fn func(*ledger.Ledger) error) error {
	if err := s.lockLedger(ctx); err != nil {
		return err
	}
	defer s.opts.LedgerMu.Unlock()
	l, err := copyLedger(s.opts.Ledger)
	if err != nil {
		return err
	}
	if err = fn(l); err != nil {
		return err
	}
	if err = l.Save(s.opts.LedgerPath); err != nil {
		return err
	}
	*s.opts.Ledger = *l
	return nil
}

// Outcome writes must survive a canceled command request. This extra bounded
// budget is local only: it never permits another network call or delivery.
func (s *Server) settle(ctx context.Context, fn func(*ledger.Ledger) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	return s.update(ctx, fn)
}

// Separate command/send locks avoid holding the send lock over gc's callback.
func (s *Server) lock(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.keysMu.Lock()
	k := s.keys[key]
	if k == nil {
		k = &keyLock{gate: make(chan struct{}, 1)}
		s.keys[key] = k
	}
	k.users++
	s.keysMu.Unlock()
	release := func() {
		s.keysMu.Lock()
		defer s.keysMu.Unlock()
		k.users--
		if k.users == 0 {
			delete(s.keys, key)
		}
	}
	select {
	case k.gate <- struct{}{}:
		return func() { <-k.gate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func keyFor(p wire.PublishRequest) string {
	if p.IdempotencyKey != "" {
		return p.IdempotencyKey
	}
	// Length-delimited JSON avoids collisions when input contains separators.
	b, _ := json.Marshal([]any{p.SessionID, p.Conversation, p.ReplyToMessageID, p.Text})
	sum := sha256.Sum256(b)
	return "derived-" + hex.EncodeToString(sum[:])
}
func (s *Server) normalize(p *wire.PublishRequest) error {
	cfg := s.configuration()
	if p.Conversation.ScopeID == "" {
		p.Conversation.ScopeID = cfg.Secrets.CityName
	}
	if p.Conversation.Provider == "" {
		p.Conversation.Provider = "gchat"
	}
	if p.Conversation.AccountID == "" {
		p.Conversation.AccountID = cfg.Identity().Name
	}
	if p.Conversation.ScopeID != cfg.Secrets.CityName || p.Conversation.Provider != "gchat" || p.Conversation.AccountID != cfg.Identity().Name || !validSpace(p.Conversation.ConversationID) {
		return errors.New("invalid conversation for this adapter")
	}
	if p.Conversation.Kind == "" {
		p.Conversation.Kind = "room"
	}
	if p.Conversation.Kind != "room" && p.Conversation.Kind != "dm" && p.Conversation.Kind != "thread" {
		return errors.New("invalid conversation kind")
	}
	p.IdempotencyKey = keyFor(*p)
	return nil
}
func validSpace(space string) bool {
	parts := strings.Split(space, "/")
	return len(parts) == 2 && parts[0] == "spaces" && parts[1] != "" && parts[1] != "." && parts[1] != ".."
}
func (s *Server) failure(p wire.PublishRequest, err error) wire.PublishReceipt {
	kind, delay := chat.Classify(err)
	var gcErr *gcclient.HTTPError
	if errors.As(err, &gcErr) {
		kind, delay = chat.Classify(&googleapi.Error{Code: gcErr.StatusCode, Header: http.Header{"Retry-After": []string{gcErr.RetryAfter}}})
		if gcErr.StatusCode == http.StatusRequestTimeout || s.isNoAdapter(err) || gcclient.IsCityNotRunning(err) {
			kind = "transient"
		}
	}
	if kind == "transient" && delay == 0 {
		delay = time.Second
	}
	return wire.PublishReceipt{Conversation: p.Conversation, FailureKind: kind, RetryAfter: int64(delay)}
}
func permanent(p wire.PublishRequest) wire.PublishReceipt {
	return wire.PublishReceipt{Conversation: p.Conversation, FailureKind: "permanent"}
}

// No caller has accepted responsibility for an automatic attempt's transient
// failure. Keep its durable intent until a sweep succeeds or definitely fails.
// An absent classification is ambiguous, not evidence of a definite rejection.
func retryAutomatically(automatic bool, receipt wire.PublishReceipt) bool {
	return automatic && !receipt.Delivered && (receipt.FailureKind == "" || receipt.FailureKind == "transient" || receipt.FailureKind == "rate_limited")
}

// Log identifiers and classification only. Error strings and HTTP bodies may
// contain credentials, URLs with tokens, or message text and must never be logged.
func logCause(err error) (string, int) {
	kind, _ := chat.Classify(err)
	var googleErr *googleapi.Error
	if errors.As(err, &googleErr) {
		return kind, googleErr.Code
	}
	var gcErr *gcclient.HTTPError
	if errors.As(err, &gcErr) {
		switch {
		case gcErr.StatusCode == 429:
			kind = "rate_limited"
		case gcErr.StatusCode == 401 || gcErr.StatusCode == 403:
			kind = "auth"
		case gcclient.IsCityNotRunning(err):
			kind = "transient"
		case gcErr.StatusCode == 404:
			kind = "not_found"
		case gcErr.StatusCode >= 400 && gcErr.StatusCode < 500 && gcErr.StatusCode != 408 && gcErr.StatusCode != 409:
			kind = "permanent"
		}
		return kind, gcErr.StatusCode
	}
	return kind, 0
}

func (s *Server) logPublish(p wire.PublishRequest, receipt wire.PublishReceipt, err error) {
	s.logOutcome("publish", p, receipt, err)
}

func (s *Server) logOutcome(operation string, p wire.PublishRequest, receipt wire.PublishReceipt, err error) {
	failed := err != nil || !receipt.Delivered
	key := operation + ":" + p.IdempotencyKey
	s.logMu.Lock()
	_, previouslyFailed := s.publishFailures[key]
	recovered := previouslyFailed && !failed
	if failed {
		// WARNs are never suppressed. Bound only the best-effort INFO recovery
		// history, evicting the oldest failure when callers abandon their keys.
		if !previouslyFailed && len(s.publishFailures) >= maxFailureLogKeys {
			var oldest string
			var at time.Time
			for k, when := range s.publishFailures {
				if at.IsZero() || when.Before(at) {
					oldest, at = k, when
				}
			}
			delete(s.publishFailures, oldest)
		}
		s.publishFailures[key] = time.Now()
	} else {
		delete(s.publishFailures, key)
	}
	s.logMu.Unlock()
	attrs := []any{"operation", operation, "space", p.Conversation.ConversationID, "key", p.IdempotencyKey, "message_id", receipt.MessageID}
	if failed {
		_, status := logCause(err)
		s.opts.Logger.Warn(operation+" failed", append(attrs, "cause_kind", receipt.FailureKind, "http_status", status)...)
	} else if recovered {
		s.opts.Logger.Info(operation+" recovered", append(attrs, "cause_kind", "", "http_status", http.StatusOK)...)
	}
}

func (s *Server) publishHTTP(w http.ResponseWriter, r *http.Request) {
	var p wire.PublishRequest
	if err := body(w, r, &p); err != nil {
		s.logPublish(p, permanent(p), err)
		respond(w, 200, permanent(p))
		return
	}
	if err := s.normalize(&p); err != nil {
		s.logPublish(p, permanent(p), err)
		respond(w, 200, permanent(p))
		return
	}
	receipt, _ := s.publish(r.Context(), p, nil)
	respond(w, 200, receipt)
}

// prepare runs under the send lock. A receipt is durable only after update
// succeeds; failed saves never make in-memory progress look committed.
func (s *Server) prepare(ctx context.Context, p wire.PublishRequest, upload *ledger.SendUpload, record bool) error {
	return s.update(ctx, func(l *ledger.Ledger) error {
		if old := l.Sends[p.IdempotencyKey]; old != nil {
			if old.Space != p.Conversation.ConversationID {
				return errors.New("idempotency key already belongs to another conversation")
			}
			if old.Request != nil && old.Request.SessionID != p.SessionID {
				return errors.New("idempotency key already belongs to another session")
			}
			if record && !old.Recorded && (old.Receipt == nil || !old.Receipt.Delivered) {
				// A command retry or recovery sweep re-arms an undelivered
				// intent. A session may have gained a binding since the last
				// attempt; ask gc again, retaining the diagnostic until then.
				old.RecordPending = true
				old.Receipt = nil
				if old.Request != nil && old.Request.SessionID != "" {
					old.NotRecordable = false
				}
			}
			return nil
		}
		parts := markdown.Split(markdown.ToChat(p.Text), 32000)
		if len(parts) == 0 {
			parts = []string{""}
		}
		send := &ledger.SendState{Key: p.IdempotencyKey, Space: p.Conversation.ConversationID, Thread: p.ReplyToMessageID, Created: time.Now().UTC(), Request: &p, Upload: upload, RecordPending: record}
		for i, text := range parts {
			send.Parts = append(send.Parts, ledger.SendPart{RequestID: fmt.Sprintf("%s-%d", p.IdempotencyKey, i+1), Text: text})
		}
		l.Sends[send.Key] = send
		return nil
	})
}

func (s *Server) publish(ctx context.Context, p wire.PublishRequest, upload *ledger.SendUpload) (receipt wire.PublishReceipt, err error) {
	defer func() { s.logPublish(p, receipt, err) }()
	unlock, err := s.lock(ctx, "send:"+p.IdempotencyKey)
	if err != nil {
		return s.failure(p, err), err
	}
	defer unlock()
	l, err := s.snapshot(ctx)
	if err != nil {
		return s.failure(p, err), err
	}
	if old := l.Sends[p.IdempotencyKey]; old != nil {
		if old.Space != p.Conversation.ConversationID || (old.Request != nil && old.Request.SessionID != p.SessionID) {
			return permanent(p), errors.New("idempotency key belongs to another conversation or session")
		}
		if old.Receipt != nil && old.Receipt.Delivered && s.Health().State != "recovery-required" {
			return *old.Receipt, nil
		}
	}
	if err = s.available(); err != nil {
		return s.failure(p, err), err
	}
	if err = s.prepare(ctx, p, upload, false); err != nil {
		return s.failure(p, err), err
	}
	l, err = s.snapshot(ctx)
	if err != nil {
		return s.failure(p, err), err
	}
	send := l.Sends[p.IdempotencyKey]
	if send.Request != nil {
		p = *send.Request
	}
	if send.Receipt != nil && send.Receipt.Delivered {
		return *send.Receipt, nil
	}
	c, _ := s.clients()
	named, err := s.named(ctx, send.Space)
	if err != nil {
		return s.failure(p, err), err
	}
	if send.Upload != nil && send.Parts[0].MessageName == "" && send.Upload.ResourceName == "" && send.Upload.UploadToken == "" {
		ref, e := c.UploadAttachment(ctx, send.Space, send.Upload.Path)
		if e != nil {
			return s.failure(p, e), e
		}
		send.Upload.ResourceName = ref.ResourceName
		send.Upload.UploadToken = ref.AttachmentUploadToken
		if err = s.update(ctx, func(l *ledger.Ledger) error { l.Sends[send.Key].Upload = send.Upload; return nil }); err != nil {
			return s.failure(p, err), err
		}
	}
	for i, part := range send.Parts {
		if part.MessageName != "" {
			continue
		}
		var attachments []*api.Attachment
		if i == 0 && send.Upload != nil {
			attachments = []*api.Attachment{{AttachmentDataRef: &api.AttachmentDataRef{ResourceName: send.Upload.ResourceName, AttachmentUploadToken: send.Upload.UploadToken}}}
		}
		name, thread, e := c.CreateMessageWithAttachments(ctx, send.Space, part.Text, send.Thread, part.RequestID, named, attachments)
		if e != nil {
			return s.failure(p, e), e
		}
		send.Parts[i].MessageName = name
		send.Thread = thread
		if err = s.update(ctx, func(l *ledger.Ledger) error {
			current := l.Sends[send.Key]
			current.Parts[i].MessageName = name
			current.Thread = thread
			m := l.Messages[name]
			if m == nil {
				m = &ledger.MessageState{}
				l.Messages[name] = m
			}
			m.Mine = true
			m.SendKey = send.Key
			m.Thread = thread
			return nil
		}); err != nil {
			return s.failure(p, err), err
		}
	}
	receipt = wire.PublishReceipt{MessageID: send.Parts[0].MessageName, Conversation: p.Conversation, Delivered: true, Metadata: map[string]string{"thread": send.Thread, "parts": strconv.Itoa(len(send.Parts))}}
	if err = s.update(ctx, func(l *ledger.Ledger) error {
		v := l.Sends[send.Key]
		// A callback may finish after the command returned a gc failure.
		// Its late success must revive transcript recovery, not delivery.
		if v.Receipt != nil && !v.Receipt.Delivered && !v.NotRecordable && !v.Recorded {
			v.RecordPending = true
		}
		v.Receipt = &receipt
		if v.NotRecordable || v.Recorded {
			v.RecordPending = false
		}
		return nil
	}); err != nil {
		return s.failure(p, err), err
	}
	return receipt, nil
}

func (s *Server) named(ctx context.Context, space string) (bool, error) {
	l, err := s.snapshot(ctx)
	if err != nil {
		return false, err
	}
	if v := l.Spaces[space]; v != nil && v.ChatType != "" {
		return v.ChatType == "SPACE", nil
	}
	c, _ := s.clients()
	spaces, err := c.ListSpaces(ctx)
	if err != nil {
		return false, err
	}
	for _, v := range spaces {
		if v.Name == space {
			kind := v.SpaceType
			if kind == "" {
				return false, errors.New("space type unavailable")
			}
			err = s.update(ctx, func(l *ledger.Ledger) error {
				v := l.Spaces[space]
				if v == nil {
					v = &ledger.SpaceState{}
					l.Spaces[space] = v
				}
				v.ChatType = kind
				v.Kind = "room"
				if kind == "DIRECT_MESSAGE" {
					v.Kind = "dm"
				}
				return nil
			})
			return kind == "SPACE", err
		}
	}
	return false, &googleapi.Error{Code: http.StatusNotFound, Message: "space not found in identity inventory"}
}

type commandRequest struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	Thread         string `json:"thread"`
	Text           string `json:"text"`
	Path           string `json:"path"`
	IdempotencyKey string `json:"idempotency_key"`
}
type CommandResult struct {
	Delivered   bool   `json:"delivered"`
	Recorded    bool   `json:"recorded"`
	MessageID   string `json:"message_id"`
	Key         string `json:"key"`
	Reason      string `json:"reason,omitempty"`
	FailureKind string `json:"failure_kind,omitempty"`
	RetryAfter  int64  `json:"retry_after,omitempty"`
}

func (s *Server) commandHTTP(w http.ResponseWriter, r *http.Request) {
	var cmd commandRequest
	if err := body(w, r, &cmd); err != nil {
		problem(w, 400, err)
		return
	}
	p := wire.PublishRequest{SessionID: cmd.SessionID, Conversation: wire.Conversation{ConversationID: cmd.ConversationID}, Text: cmd.Text, ReplyToMessageID: cmd.Thread, IdempotencyKey: cmd.IdempotencyKey}
	if err := s.normalize(&p); err != nil {
		problem(w, 400, err)
		return
	}
	var upload *ledger.SendUpload
	if r.URL.Path == "/commands/upload" {
		if cmd.Path == "" {
			problem(w, 400, errors.New("upload path is required"))
			return
		}
		upload = &ledger.SendUpload{Path: cmd.Path}
	}
	// A derived key includes the conversation kind. Probe both possible kinds
	// before inventory so a saved DM outcome survives missing credentials or
	// stale space inventory as well as one with an explicit key.
	l, err := s.snapshot(r.Context())
	if err != nil {
		respond(w, 200, CommandResult{Key: p.IdempotencyKey, Reason: err.Error()})
		return
	}
	found := false
	for _, kind := range []string{"room", "dm"} {
		p.Conversation.Kind = kind
		p.IdempotencyKey = commandKey(p, cmd.IdempotencyKey, upload)
		if l.Sends[p.IdempotencyKey] != nil {
			found = true
			break
		}
	}
	if !found {
		if err := s.available(); err != nil {
			respond(w, 200, CommandResult{Key: p.IdempotencyKey, Reason: err.Error()})
			return
		}
		// A DM's gc binding is distinct from a room binding.
		if _, err := s.named(r.Context(), p.Conversation.ConversationID); err != nil {
			r := s.failure(p, err)
			respond(w, 200, CommandResult{Key: p.IdempotencyKey, Reason: err.Error(), FailureKind: r.FailureKind, RetryAfter: r.RetryAfter})
			return
		}
		l, err = s.snapshot(r.Context())
		if err != nil {
			problem(w, 500, err)
			return
		}
		p.Conversation.Kind = l.Spaces[p.Conversation.ConversationID].Kind
		p.IdempotencyKey = commandKey(p, cmd.IdempotencyKey, upload)
	}
	result, _ := s.command(r.Context(), p, upload)
	respond(w, 200, result)
}

func commandKey(p wire.PublishRequest, explicit string, upload *ledger.SendUpload) string {
	if explicit != "" {
		return explicit
	}
	p.IdempotencyKey = ""
	key := keyFor(p)
	if upload != nil {
		sum := sha256.Sum256([]byte(key + "\x00" + upload.Path))
		key = "derived-" + hex.EncodeToString(sum[:])
	}
	return key
}

func commandOutcome(send *ledger.SendState) CommandResult {
	result := CommandResult{Key: send.Key, Recorded: send.Recorded, Reason: send.RecordingReason}
	if r := send.Receipt; r != nil {
		result.Delivered, result.MessageID = r.Delivered, r.MessageID
		result.FailureKind, result.RetryAfter = r.FailureKind, r.RetryAfter
	}
	if send.RecordingFailureKind != "" {
		result.FailureKind = send.RecordingFailureKind
	}
	return result
}

func (s *Server) command(ctx context.Context, p wire.PublishRequest, upload *ledger.SendUpload) (CommandResult, error) {
	return s.runCommand(ctx, p, upload, false)
}

func (s *Server) runCommand(ctx context.Context, p wire.PublishRequest, upload *ledger.SendUpload, automatic bool) (CommandResult, error) {
	result := CommandResult{Key: p.IdempotencyKey}
	unlock, err := s.lock(ctx, "command:"+p.IdempotencyKey)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	defer unlock()
	l, err := s.snapshot(ctx)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	send := l.Sends[p.IdempotencyKey]
	if send != nil {
		if send.Space != p.Conversation.ConversationID || (send.Request != nil && send.Request.SessionID != p.SessionID) {
			err := errors.New("idempotency key belongs to another conversation or session")
			result.Reason = err.Error()
			return result, err
		}
		result = commandOutcome(send)
		// Repair receipts written by older versions between their receipt
		// commit and the separate pending-flag clear.
		if send.RecordPending && (send.Recorded || (result.Delivered && send.NotRecordable)) {
			err := s.update(ctx, func(l *ledger.Ledger) error {
				l.Sends[p.IdempotencyKey].RecordPending = false
				return nil
			})
			return result, err
		}
		// Re-check under the command lock: a caller may have finished or
		// rejected this intent since RetryPending took its snapshot.
		if (automatic && !send.RecordPending) || send.Recorded || (result.Delivered && send.NotRecordable && send.RecordingFailureKind == "") {
			return result, nil
		}
		if send.Request != nil {
			p = *send.Request
		}
	}
	if !result.Delivered {
		if err := s.available(); err != nil {
			if send != nil {
				return s.failPreparedCommand(ctx, p, err, automatic)
			}
			result.Reason = err.Error()
			return result, err
		}
		// Only hold the send lock for preparation; gc recursively calls /publish.
		sendUnlock, err := s.lock(ctx, "send:"+p.IdempotencyKey)
		if err == nil {
			err = s.prepare(ctx, p, upload, true)
			sendUnlock()
		}
		if err != nil {
			if send != nil {
				return s.failPreparedCommand(ctx, p, err, automatic)
			}
			result.Reason = err.Error()
			return result, err
		}
		l, err = s.snapshot(ctx)
		if err != nil {
			return s.failPreparedCommand(ctx, p, err, automatic)
		}
		send = l.Sends[p.IdempotencyKey]
		result = commandOutcome(send)
		if send.Request != nil {
			p = *send.Request
		}
	}
	s.logOutcome("command_prepare", p, wire.PublishReceipt{Delivered: true}, nil)
	if p.SessionID == "" {
		return s.direct(ctx, p, send.Upload, noBindingReason(""), result, automatic)
	}
	if send.NotRecordable && send.RecordingFailureKind == "" {
		return s.direct(ctx, p, send.Upload, noBindingReason(send.RecordingReason), result, automatic)
	}
	if send.Upload != nil && !result.Delivered {
		r, e := s.publish(ctx, p, send.Upload)
		result.Delivered = r.Delivered
		result.MessageID = r.MessageID
		result.FailureKind = r.FailureKind
		result.RetryAfter = r.RetryAfter
		if e != nil || !r.Delivered {
			if e != nil {
				result.Reason = e.Error()
			}
			if retryAutomatically(automatic, r) {
				return result, e
			}
			return result, errors.Join(e, s.stopRetry(ctx, p, r, result.Reason))
		}
	}
	_, g := s.clients()
	if g == nil {
		err = errors.New("gc client is initializing")
	} else {
		var out wire.OutboundResult
		out, err = g.Outbound(ctx, wire.OutboundRequest{SessionID: p.SessionID, Conversation: p.Conversation, Text: p.Text, ReplyToMessageID: p.ReplyToMessageID, IdempotencyKey: p.IdempotencyKey})
		if err == nil {
			// Direct uploads are already delivered, even when gc's recording
			// callback cannot retrieve their receipt. Keep the durable outcome.
			if !result.Delivered {
				result.Delivered = out.Receipt.Delivered
				result.MessageID = out.Receipt.MessageID
				result.FailureKind = out.Receipt.FailureKind
				result.RetryAfter = out.Receipt.RetryAfter
			}
			result.Recorded = out.TranscriptEntry != nil
		}
	}
	if !gcclient.IsNoBinding(err) {
		if err != nil {
			s.logOutcome("gc_outbound", p, s.failure(p, err), err)
		} else {
			// A 200 result can carry a failed callback even when that callback
			// never reached this process. Account for every failed attempt.
			s.logOutcome("gc_outbound", p, wire.PublishReceipt{Delivered: result.Delivered, MessageID: result.MessageID, FailureKind: result.FailureKind}, nil)
		}
	}
	if gcclient.IsNoBinding(err) {
		return s.direct(ctx, p, send.Upload, detail(err), result, automatic)
	}
	if err != nil {
		s.ObserveGCError(err)
		result.Reason = detail(err)
		// A caller owns its returned failures; automatic attempts retain
		// transient failures for the next sweep. Settlement is bounded and
		// independent of the request, with no network calls or delivery.
		settleErr := s.settle(ctx, func(l *ledger.Ledger) error {
			v := l.Sends[p.IdempotencyKey]
			// Read the callback outcome and settle under the same ledger lock.
			if r := v.Receipt; r != nil && r.Delivered {
				result.Delivered = true
				result.MessageID = r.MessageID
				result.FailureKind, result.RetryAfter = "", 0
			}
			if !result.Delivered {
				r := s.failure(p, err)
				result.FailureKind, result.RetryAfter = r.FailureKind, r.RetryAfter
				if !retryAutomatically(automatic, r) {
					v.Receipt, v.RecordPending, v.RecordingReason = &r, false, result.Reason
				}
			} else {
				// Delivery is durable; only recording failed. Definite rejections
				// belong to the caller, while ambiguous recording failures recover.
				var gcErr *gcclient.HTTPError
				definite := errors.As(err, &gcErr) && gcErr.StatusCode >= 400 && gcErr.StatusCode < 500 && gcErr.StatusCode != 408 && gcErr.StatusCode != 429 && !s.isNoAdapter(err) && !gcclient.IsCityNotRunning(err)
				if definite {
					result.FailureKind = s.failure(p, err).FailureKind
				}
				v.RecordPending = !definite && !v.Recorded
				v.NotRecordable = definite
				v.RecordingReason = result.Reason
				v.RecordingFailureKind = result.FailureKind
			}
			return nil
		})
		return result, errors.Join(err, settleErr)
	}
	err = s.settle(ctx, func(l *ledger.Ledger) error {
		v := l.Sends[p.IdempotencyKey]
		// gc can return HTTP 200 with Delivered:false after losing the
		// callback response. Reconcile under the ledger lock so that answer
		// can never overwrite a success committed by the callback.
		if r := v.Receipt; r != nil && r.Delivered {
			result.Delivered, result.MessageID = true, r.MessageID
			result.FailureKind, result.RetryAfter = "", 0
		}
		result.Recorded = result.Recorded || v.Recorded
		result.Reason = ""
		if !result.Recorded {
			if result.Delivered {
				result.Reason = "delivered but gc did not record a transcript entry"
			} else {
				result.Reason = "gc did not deliver the message"
			}
		}
		if retryAutomatically(automatic, wire.PublishReceipt{Delivered: result.Delivered, FailureKind: result.FailureKind}) {
			// A 200 gc result can carry a transient callback failure. Reconcile
			// the durable receipt above before leaving this intent pending.
			return errors.New(result.Reason)
		}
		v.Recorded = result.Recorded
		v.RecordPending = result.Delivered && !result.Recorded
		v.NotRecordable = false
		v.RecordingFailureKind = ""
		v.RecordingReason = result.Reason
		if !result.Delivered {
			v.Receipt = &wire.PublishReceipt{Conversation: p.Conversation, FailureKind: result.FailureKind, RetryAfter: result.RetryAfter}
		}
		return nil
	})
	if err != nil {
		result.Reason = err.Error()
	}
	return result, err
}

// Preparation failures follow the same retry ownership rule as delivery errors,
// including cancellation just after prepare saved the intent.
func (s *Server) failPreparedCommand(ctx context.Context, p wire.PublishRequest, cause error, automatic bool) (CommandResult, error) {
	receipt := s.failure(p, cause)
	result := CommandResult{Key: p.IdempotencyKey, Reason: cause.Error(), FailureKind: receipt.FailureKind, RetryAfter: receipt.RetryAfter}
	s.logOutcome("command_prepare", p, receipt, cause)
	err := s.settle(ctx, func(l *ledger.Ledger) error {
		v := l.Sends[p.IdempotencyKey]
		if v.Receipt != nil && v.Receipt.Delivered {
			// A concurrent callback may have completed while preparation waited.
			result = commandOutcome(v)
			return nil
		}
		if retryAutomatically(automatic, receipt) {
			return nil
		}
		v.Receipt = &receipt
		v.RecordPending = false
		v.RecordingReason = result.Reason
		return nil
	})
	return result, errors.Join(cause, err)
}

// Receipt plus RecordPending distinguish interrupted sends (pending, no receipt),
// returned failures (not pending), and delivered sends awaiting recording
// (pending, delivered receipt). A caller can retry a returned failure explicitly.
func (s *Server) stopRetry(ctx context.Context, p wire.PublishRequest, receipt wire.PublishReceipt, reason string) error {
	return s.settle(ctx, func(l *ledger.Ledger) error {
		v := l.Sends[p.IdempotencyKey]
		if v.Receipt == nil || !v.Receipt.Delivered {
			v.Receipt = &receipt
			v.RecordPending = false
		}
		v.RecordingReason = reason
		return nil
	})
}
func noBindingReason(reason string) string {
	if reason != "" {
		return reason
	}
	return "no active binding: session_id is empty"
}
func detail(err error) string {
	var e *gcclient.HTTPError
	if errors.As(err, &e) && e.Problem != nil {
		return e.Problem.Detail
	}
	return err.Error()
}
func (s *Server) direct(ctx context.Context, p wire.PublishRequest, upload *ledger.SendUpload, reason string, result CommandResult, automatic bool) (CommandResult, error) {
	result.Reason = reason
	if err := s.settle(ctx, func(l *ledger.Ledger) error {
		v := l.Sends[p.IdempotencyKey]
		v.NotRecordable = true
		v.RecordingFailureKind = ""
		v.RecordPending = v.Receipt == nil || !v.Receipt.Delivered
		v.RecordingReason = reason
		return nil
	}); err != nil {
		result.Reason = err.Error()
		return result, err
	}
	if result.Delivered {
		result.FailureKind, result.RetryAfter = "", 0
		return result, nil
	}
	r, err := s.publish(ctx, p, upload)
	result.Delivered = r.Delivered
	result.MessageID = r.MessageID
	result.FailureKind = r.FailureKind
	result.RetryAfter = r.RetryAfter
	if err != nil {
		result.Reason = err.Error()
	}
	if !retryAutomatically(automatic, r) {
		err = errors.Join(err, s.stopRetry(ctx, p, r, result.Reason))
	}
	return result, err
}

// RetryPending repairs persisted command sends and transcript writes after a
// restart. The app calls it after readiness and periodically; callback-only
// sends remain caller-driven because gc owns their transcript operation.
func (s *Server) RetryPending(ctx context.Context) error {
	if !s.stateGate.TryRLock() {
		return errors.New("state recovery in progress")
	}
	defer s.stateGate.RUnlock()
	if err := s.available(); err != nil {
		return err
	}
	l, err := s.snapshot(ctx)
	if err != nil {
		return err
	}
	var keys []string
	for key, v := range l.Sends {
		if v.Request != nil && v.RecordPending {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var failures []error
	for _, key := range keys {
		ctx, cancel := context.WithTimeout(ctx, publishBudget)
		_, err := s.runCommand(ctx, *l.Sends[key].Request, l.Sends[key].Upload, true)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Registration builds the production callback contract for a validated base.
// Tests may supply an isolated HTTP callback; the service uses its gc proxy URL.
func Registration(cfg *config.Config, base string) (wire.Registration, error) {
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if cfg == nil || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return wire.Registration{}, errors.New("registrar: absolute gc callback URL required")
	}
	return wire.Registration{Provider: "gchat", AccountID: cfg.Identity().Name, CallbackURL: base, Capabilities: wire.Capabilities{SupportsAttachments: true, MaxMessageLength: 32000}, ReplyInstructions: "gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"}, nil
}

// Register retries with bounded backoff until successful or canceled. The base
// includes gc's service proxy prefix; gc itself appends /publish.
func (s *Server) Register(ctx context.Context) error {
	select {
	case s.regGate <- struct{}{}:
		defer func() { <-s.regGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	cfg := s.configuration()
	base := strings.TrimRight(cfg.Secrets.GCAPIBase, "/") + "/" + strings.Trim(cfg.Secrets.ServiceURLPrefix, "/")
	base = strings.TrimRight(base, "/")
	reg, err := Registration(&cfg, base)
	if err != nil {
		return err
	}
	for delay := time.Second; ; {
		_, g := s.clients()
		err := errors.New("gc client is initializing")
		if g != nil {
			err = g.Register(ctx, reg)
		}
		if err == nil {
			if s.regFailed {
				s.opts.Logger.Info("gc registration recovered", "operation", "register", "account", reg.AccountID, "cause_kind", "", "http_status", http.StatusOK)
				s.regFailed = false
			}
			return nil
		}
		kind, status := logCause(err)
		s.opts.Logger.Warn("gc registration failed", "operation", "register", "account", reg.AccountID, "cause_kind", kind, "http_status", status)
		s.regFailed = true
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-s.ctx.Done():
			t.Stop()
			return s.ctx.Err()
		case <-t.C:
		}
		if delay < time.Minute {
			delay *= 2
			if delay > time.Minute {
				delay = time.Minute
			}
		}
	}
}

// Use the configured identity, not the account persisted in an older intent.
func (s *Server) isNoAdapter(err error) bool {
	cfg := s.configuration()
	return gcclient.IsNoAdapter(err, cfg.Identity().Name)
}

// ObserveGCError is also available to app-owned gc call sites (inbound/poll).
// Re-registration uses service lifetime, not the failed request's deadline.
func (s *Server) ObserveGCError(err error) {
	if !errors.Is(err, syscall.ECONNREFUSED) && !s.isNoAdapter(err) && !gcclient.IsCityNotRunning(err) {
		return
	}
	// Coalesce without blocking the failed command or accumulating waiters.
	s.mu.Lock()
	if s.registering {
		s.mu.Unlock()
		return
	}
	s.registering = true
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); s.registering = false; s.mu.Unlock() }()
		_ = s.Register(s.ctx)
	}()
}

func (s *Server) react(w http.ResponseWriter, r *http.Request) {
	if err := s.available(); err != nil {
		problem(w, 503, err)
		return
	}
	var v struct {
		Message string `json:"message"`
		Emoji   string `json:"emoji"`
		Remove  bool   `json:"remove"`
	}
	if err := body(w, r, &v); err != nil || v.Message == "" || v.Emoji == "" {
		problem(w, 400, errors.New("message and emoji are required"))
		return
	}
	c, _ := s.clients()
	var name string
	var err error
	if v.Remove {
		err = c.DeleteReaction(r.Context(), v.Message, v.Emoji)
	} else {
		name, err = c.CreateReaction(r.Context(), v.Message, v.Emoji)
	}
	if err != nil {
		problem(w, 502, err)
		return
	}
	respond(w, 200, map[string]any{"name": name, "removed": v.Remove})
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if err := s.available(); err != nil {
		problem(w, 503, err)
		return
	}
	q := r.URL.Query()
	if !validSpace(q.Get("space")) {
		problem(w, 400, errors.New("space is required"))
		return
	}
	var since time.Time
	var err error
	if q.Get("since") != "" {
		since, err = time.Parse(time.RFC3339Nano, q.Get("since"))
		if err != nil {
			problem(w, 400, err)
			return
		}
	}
	limit := 100
	if q.Get("limit") != "" {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 1000 {
			problem(w, 400, errors.New("limit must be between 1 and 1000"))
			return
		}
	}
	c, _ := s.clients()
	var messages []*api.Message
	err = c.ListMessages(r.Context(), q.Get("space"), since, q.Get("thread"), func(page []*api.Message) error {
		messages = append(messages, page...)
		sort.SliceStable(messages, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339Nano, messages[i].CreateTime)
			b, _ := time.Parse(time.RFC3339Nano, messages[j].CreateTime)
			return a.Before(b)
		})
		if len(messages) > limit {
			messages = messages[len(messages)-limit:]
		}
		return nil
	})
	if err != nil {
		problem(w, 502, err)
		return
	}
	if _, err := s.named(r.Context(), q.Get("space")); err != nil {
		problem(w, 502, err)
		return
	}
	l, err := s.snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	cfg := s.configuration()
	kind := "room"
	if l.Spaces[q.Get("space")].Kind == "dm" {
		kind = "dm"
	}
	result := make([]wire.InboundMessage, 0, len(messages))
	for _, m := range messages {
		at, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
		v := wire.InboundMessage{ProviderMessageID: m.Name, Conversation: wire.Conversation{ScopeID: cfg.Secrets.CityName, Provider: "gchat", AccountID: cfg.Identity().Name, ConversationID: q.Get("space"), Kind: kind}, Text: m.Text, ReceivedAt: at}
		if m.Sender != nil {
			v.Actor = wire.Actor{ID: m.Sender.Name, DisplayName: m.Sender.DisplayName, IsBot: m.Sender.Type == "BOT"}
			if v.Actor.DisplayName == "" {
				v.Actor.DisplayName = v.Actor.ID
			}
		}
		if m.Thread != nil {
			v.ReplyToMessageID = m.Thread.Name
		}
		if saved, ok := historyContent(l, m); ok {
			v.Text, v.Attachments = saved.Text, saved.Attachments
		} else {
			v.Text, v.Attachments = inbound.NormalizeContent(r.Context(), c, cfg.Endpoints, m)
		}
		result = append(result, v)
	}
	respond(w, 200, result)
}

// Reuse the turn's exact normalized content for the same revision, including
// its local media paths. Locally skipped messages still need normalization.
func historyContent(l *ledger.Ledger, m *api.Message) (wire.InboundMessage, bool) {
	var saved wire.InboundMessage
	revision := m.LastUpdateTime
	if revision == "" {
		revision = m.CreateTime
	}
	at, err := time.Parse(time.RFC3339Nano, revision)
	state := l.Messages[m.Name]
	if err != nil || state == nil || state.Deleted || !state.Revision.Equal(at) {
		return saved, false
	}
	ids := []string{m.Name + "#updated#" + m.LastUpdateTime, m.Name}
	for _, id := range ids {
		change := l.Changes[id]
		if change == nil {
			continue
		}
		if id == m.Name && m.LastUpdateTime != "" && change.Kind != "message.updated" {
			return saved, false
		}
		if strings.HasPrefix(change.Reason, "space unavailable") {
			return saved, false
		}
		switch change.Reason {
		case "local", "own", "wake_policy":
			return saved, false
		}
		if json.Unmarshal([]byte(change.Event), &saved) == nil && saved.ProviderMessageID == id {
			if change.CatchUp {
				saved.Text = strings.TrimPrefix(saved.Text, "[catch-up]\n")
			}
			if change.Kind == "message.updated" && id != m.Name {
				saved.Text = strings.TrimPrefix(saved.Text, "edited: ")
			}
			return saved, true
		}
	}
	return saved, false
}

func (s *Server) spaces(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.setSpace(w, r)
		return
	}
	if err := s.available(); err != nil {
		problem(w, 503, err)
		return
	}
	c, _ := s.clients()
	spaces, err := c.ListSpaces(r.Context())
	if err != nil {
		problem(w, 502, err)
		return
	}
	cfg := s.configuration()
	type view struct {
		ID   string            `json:"id"`
		Name string            `json:"name"`
		Kind string            `json:"kind"`
		Wake config.WakePolicy `json:"wake"`
	}
	result := make([]view, 0, len(spaces))
	for _, v := range spaces {
		result = append(result, view{v.Name, v.DisplayName, v.SpaceType, cfg.Wake(v.Name)})
	}
	respond(w, 200, result)
}

func (s *Server) setSpace(w http.ResponseWriter, r *http.Request) {
	var v config.Space
	var input struct {
		ID   string            `json:"id"`
		Wake config.WakePolicy `json:"wake"`
	}
	if err := body(w, r, &input); err != nil || !validSpace(input.ID) || (input.Wake != "" && input.Wake != config.WakeAll && input.Wake != config.WakeMentions) {
		problem(w, 400, errors.New("valid id and wake (all, mentions, or empty for default) required"))
		return
	}
	v = config.Space{ID: input.ID, Wake: input.Wake}
	unlock, err := s.lock(r.Context(), "settings")
	if err != nil {
		problem(w, 503, err)
		return
	}
	defer unlock()
	var settings config.Settings
	meta, err := toml.DecodeFile(s.opts.SettingsPath, &settings)
	if err != nil {
		problem(w, 500, err)
		return
	}
	if len(meta.Undecoded()) != 0 {
		problem(w, 400, errors.New("settings contains unknown fields; refusing to discard them"))
		return
	}
	found := false
	for i := range settings.Spaces {
		if settings.Spaces[i].ID == v.ID {
			settings.Spaces[i] = v
			found = true
		}
	}
	if !found {
		settings.Spaces = append(settings.Spaces, v)
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(settings); err != nil {
		problem(w, 500, err)
		return
	}
	if err := writeSettings(s.opts.SettingsPath, b.Bytes()); err != nil {
		problem(w, 500, err)
		return
	}
	if err := s.update(r.Context(), func(l *ledger.Ledger) error {
		if space := l.Spaces[v.ID]; space != nil {
			space.Wake = string(v.Wake)
		}
		return nil
	}); err != nil {
		problem(w, 500, err)
		return
	}
	s.mu.Lock()
	s.cfg.Settings = settings
	s.mu.Unlock()
	respond(w, 200, input)
}
func writeSettings(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".gchat-settings-*")
	if err != nil {
		return err
	}
	// Preserve a failed temporary write for diagnosis, never replace good settings.
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	l, err := s.snapshot(r.Context())
	if err != nil {
		problem(w, 500, err)
		return
	}
	backlog := 0
	pending := 0
	type spaceStatus struct {
		*ledger.SpaceState
		Backlog      []string `json:"backlog,omitempty"`
		BacklogCount int      `json:"backlog_count"`
	}
	spaces := make(map[string]spaceStatus, len(l.Spaces))
	for name, v := range l.Spaces {
		backlog += len(v.Backlog)
		spaces[name] = spaceStatus{SpaceState: v, BacklogCount: len(v.Backlog)}
	}
	for _, v := range l.Sends {
		if v.RecordPending {
			pending++
		}
	}
	// Status, rather than reason text, determines what is still in flight and
	// what an operator can resolve. Do not include the persisted message body.
	type delivery struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Reason    string `json:"reason"`
		ReceiptID string `json:"receipt_id,omitempty"`
	}
	deliveries := make([]delivery, 0)
	inFlight := 0
	for _, change := range l.Changes {
		switch change.Status {
		case "intent", "pending":
			inFlight++
		case "unresolved":
		default:
			continue
		}
		deliveries = append(deliveries, delivery{change.ID, change.Status, change.Reason, change.ReceiptID})
	}
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].ID < deliveries[j].ID })
	var lastEvent any
	if s.opts.LastEvent != nil {
		if at := s.opts.LastEvent(); !at.IsZero() {
			lastEvent = at.UTC().Format(time.RFC3339Nano)
		}
	}
	status := map[string]any{"health": s.Health(), "subscription": l.Subscription, "spaces": spaces, "backlog": backlog, "counters": l.Counters, "unresolved": len(l.Unresolved), "unrecorded": pending, "quarantine": len(l.Quarantine), "last_event_time": lastEvent}
	status["pending"], status["deliveries"] = inFlight, deliveries
	if s.opts.SubscriptionStatus != nil {
		failures, next, state, err := s.opts.SubscriptionStatus()
		kind, code := logCause(err)
		if code == http.StatusForbidden || code == http.StatusNotFound {
			kind = "subscription-refused"
		}
		status["subscription_retry"] = map[string]any{"failures": failures, "next_attempt": next, "cause_kind": kind, "http_status": code, "state": state.State}
	}
	respond(w, 200, status)
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	var v struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := body(w, r, &v); err != nil || (v.Action != "accept" && v.Action != "replay") {
		problem(w, 400, errors.New("id and action accept or replay required"))
		return
	}
	if err := s.available(); err != nil {
		problem(w, 503, err)
		return
	}
	s.mu.RLock()
	poll := s.poll
	s.mu.RUnlock()
	if v.Action == "replay" && poll == nil {
		problem(w, 503, errors.New("inbound processor is initializing"))
		return
	}
	err := s.update(r.Context(), func(l *ledger.Ledger) error {
		c := l.Unresolved[v.ID]
		if c == nil {
			return errors.New("unresolved delivery not found")
		}
		if v.Action == "replay" {
			var m wire.InboundMessage
			if json.Unmarshal([]byte(c.Event), &m) != nil || m.ProviderMessageID != v.ID {
				return errors.New("stored turn is malformed; cannot replay")
			}
			c.Status = "intent"
			c.Attempts = 0
			c.ReceiptID = ""
			c.Created = time.Now().UTC()
			c.Reason = "operator replay"
		} else {
			c.Status = "committed"
			c.Reason = "operator accepted"
		}
		l.Changes[v.ID] = c
		delete(l.Unresolved, v.ID)
		return nil
	})
	if err != nil {
		problem(w, 400, err)
		return
	}
	if v.Action == "replay" {
		if err := poll(r.Context()); err != nil {
			s.ObserveGCError(err)
			problem(w, 502, err)
			return
		}
	}
	respond(w, 200, map[string]string{"id": v.ID, "action": v.Action})
}

type stateRequest struct {
	Action string `json:"action"`
	File   string `json:"file"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	var v stateRequest
	if err := body(w, r, &v); err != nil || (v.Action != "restore" && v.Action != "reset") {
		problem(w, 400, errors.New("action restore or reset required"))
		return
	}
	if !s.stateGate.TryLock() {
		problem(w, 409, errors.New("delivery is still in flight; retry state recovery"))
		return
	}
	defer s.stateGate.Unlock()
	// Check under the exclusive gate: another recovery may have completed
	// since this request arrived. Never authorize a reset from stale health.
	if s.Health().State != "recovery-required" {
		problem(w, 409, errors.New("state restore/reset requires recovery-required health"))
		return
	}
	if v.Action == "reset" {
		v.File = ""
	}
	if s.pendingState != nil && *s.pendingState != v {
		problem(w, 409, errors.New("state reconciliation pending; retry the same restore/reset request"))
		return
	}
	var err error
	if s.pendingState == nil {
		l := ledger.New()
		if v.Action == "restore" {
			l, err = ledger.Load(v.File, true)
			if err != nil {
				problem(w, 400, err)
				return
			}
		}
		err = s.update(r.Context(), func(current *ledger.Ledger) error { current.Replace(l); return nil })
		if err != nil {
			problem(w, 500, err)
			return
		}
		s.pendingState = &v
	}
	if err = ledger.MarkEstablished(s.configuration().SecretsDir); err != nil {
		problem(w, 500, err)
		return
	}
	s.SetHealth(Health{State: "initializing", Reason: "state restored; reconciliation required"})
	if s.opts.StateChanged != nil {
		if err = s.opts.StateChanged(r.Context()); err != nil {
			s.SetHealth(Health{State: "recovery-required", Reason: "state reconciliation failed; retry the same restore/reset request"})
			problem(w, 503, err)
			return
		}
	}
	s.pendingState = nil
	reason := "state restored"
	if v.Action == "reset" {
		reason = "state reset; messages from the gap may be lost or duplicated"
	}
	respond(w, 200, map[string]string{"state": s.Health().State, "reason": reason})
}
