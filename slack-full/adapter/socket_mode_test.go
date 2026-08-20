package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/slack-go/slack/socketmode"
)

// Tests for the Socket Mode inbound transport (gp-3og): envelope →
// handler translation, ack policy, trusted-transport gating, and one
// real WebSocket round-trip against a fake Slack.

// recordingAcker captures AckCtx calls.
type recordingAcker struct {
	mu    sync.Mutex
	calls []recordedAck
}

type recordedAck struct {
	envelopeID string
	payload    []byte
}

func (a *recordingAcker) AckCtx(_ context.Context, id string, payload any) error {
	var b []byte
	if payload != nil {
		b, _ = json.Marshal(payload)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, recordedAck{envelopeID: id, payload: b})
	return nil
}

func (a *recordingAcker) acks() []recordedAck {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recordedAck(nil), a.calls...)
}

func socketTestConfig(t *testing.T, gcURL string) config {
	t.Helper()
	cfg := dedupTestConfig(t, gcURL)
	cfg.slackAppToken = "xapp-1-test"
	cfg.socketMode = socketModePolicyAuto
	return cfg
}

func eventsEnvelopeRequest(t *testing.T, envelopeID, eventID, ts, text string, retry int) socketmode.Request {
	t.Helper()
	return socketmode.Request{
		Type:         socketmode.RequestTypeEventsAPI,
		EnvelopeID:   envelopeID,
		Payload:      eventEnvelopeBody(t, eventID, ts, text),
		RetryAttempt: retry,
		RetryReason:  map[bool]string{true: "timeout", false: ""}[retry > 0],
	}
}

// An events_api envelope runs through handleSlackEvents with no HMAC,
// forwards into gc once, and is acked; a redelivery of the same event
// (retry_attempt bumped) is acked again but forwards nothing (event_id
// dedup is shared with the HTTP path).
func TestSocketEnvelopeEventsForwardedOnceAndAcked(t *testing.T) {
	gcStub, hits := countingGCStub(t)
	cfg := socketTestConfig(t, gcStub.URL)
	events := handleSlackEvents(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil)
	r := newSocketModeRunner(cfg, events, nil, nil)
	acker := &recordingAcker{}

	out := r.handleEnvelope(context.Background(), acker, eventsEnvelopeRequest(t, "env-1", "Ev100", "1.0", "hello", 0))
	if !out.acked || out.status != http.StatusOK || out.err != nil {
		t.Fatalf("first envelope outcome = %+v, want acked 200", out)
	}
	awaitInboundHits(t, hits, 1)

	out = r.handleEnvelope(context.Background(), acker, eventsEnvelopeRequest(t, "env-2", "Ev100", "1.0", "hello", 1))
	if !out.acked || out.status != http.StatusOK {
		t.Fatalf("retry envelope outcome = %+v, want acked 200", out)
	}
	awaitInboundHits(t, hits, 1)

	acks := acker.acks()
	if len(acks) != 2 || acks[0].envelopeID != "env-1" || acks[1].envelopeID != "env-2" {
		t.Fatalf("acks = %+v, want env-1 then env-2", acks)
	}
	for _, a := range acks {
		if len(a.payload) != 0 {
			t.Errorf("events ack for %s carried payload %s, want none", a.envelopeID, a.payload)
		}
	}
	if got := r.envelopesAcked.Load(); got != 2 {
		t.Errorf("envelopesAcked = %d, want 2", got)
	}
}

// The retry fields on the envelope surface to the handler as the
// X-Slack-Retry-* headers the Events API would have sent.
func TestSocketEnvelopeRetryHeadersMapped(t *testing.T) {
	var gotNum, gotReason string
	var trusted bool
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNum = r.Header.Get("X-Slack-Retry-Num")
		gotReason = r.Header.Get("X-Slack-Retry-Reason")
		trusted = isTrustedTransportRequest(r) && trustedTransportName(r) == transportSocketMode
		w.WriteHeader(http.StatusOK)
	})
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), stub, nil, nil)
	out := r.handleEnvelope(context.Background(), &recordingAcker{}, eventsEnvelopeRequest(t, "env-r", "Ev1", "1.0", "x", 2))
	if !out.acked {
		t.Fatalf("outcome = %+v, want acked", out)
	}
	if gotNum != "2" || gotReason != "timeout" {
		t.Errorf("retry headers = (%q, %q), want (2, timeout)", gotNum, gotReason)
	}
	if !trusted {
		t.Errorf("handler did not see the trusted socket_mode marker")
	}
}

// A 5xx from the handler (company gateway: store failure / startup
// barrier) leaves the envelope un-acked so Slack redelivers; 4xx is
// acked (a retry would fail identically).
func TestSocketEnvelopeAckPolicyByStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		acked  bool
	}{
		{http.StatusOK, true},
		{http.StatusServiceUnavailable, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
	} {
		stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "verdict", tc.status)
		})
		r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), stub, nil, nil)
		acker := &recordingAcker{}
		out := r.handleEnvelope(context.Background(), acker, eventsEnvelopeRequest(t, "env", "Ev", "1.0", "x", 0))
		if out.acked != tc.acked || out.status != tc.status {
			t.Errorf("status %d: outcome = %+v, want acked=%v", tc.status, out, tc.acked)
		}
		if got := len(acker.acks()); (got == 1) != tc.acked {
			t.Errorf("status %d: acks recorded = %d, want acked=%v", tc.status, got, tc.acked)
		}
		if !tc.acked && r.envelopesUnacked.Load() != 1 {
			t.Errorf("status %d: envelopesUnacked = %d, want 1", tc.status, r.envelopesUnacked.Load())
		}
	}
}

// The trusted marker is a context value: a network POST with no
// signature is still rejected with 401 by the same handler.
func TestSocketTrustedMarkerDoesNotLeakToNetworkRequests(t *testing.T) {
	gcStub, hits := countingGCStub(t)
	cfg := socketTestConfig(t, gcStub.URL)
	events := handleSlackEvents(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(eventEnvelopeBody(t, "Ev9", "9.0", "unsigned")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	events(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned network POST status = %d, want 401", w.Code)
	}
	awaitInboundHits(t, hits, 0)
	if isTrustedTransportRequest(req) {
		t.Fatal("network request must never read as trusted")
	}
}

// A slash_commands envelope becomes the form POST the interactions
// handler parses; the handler's ephemeral JSON reply rides back as the
// ack payload.
func TestSocketEnvelopeSlashCommandAckPayload(t *testing.T) {
	cfg := socketTestConfig(t, "http://127.0.0.1:0")
	dir := t.TempDir()
	mapReg, err := newChannelMappingRegistry(filepath.Join(dir, "cm.json"))
	if err != nil {
		t.Fatal(err)
	}
	rigReg, err := newRigMappingRegistry(filepath.Join(dir, "rm.json"))
	if err != nil {
		t.Fatal(err)
	}
	interactions := handleSlackInteractions(cfg, mapReg, rigReg)
	r := newSocketModeRunner(cfg, nil, interactions, nil)
	acker := &recordingAcker{}

	payload, _ := json.Marshal(map[string]any{
		"token": "t", "team_id": "T1", "team_domain": "d", "channel_id": "C77", "channel_name": "ops",
		"user_id": "U1", "user_name": "afik", "command": "/gc", "text": "status", "api_app_id": "A1",
		"is_enterprise_install": "false", "response_url": "https://hooks.slack.com/x", "trigger_id": "tr",
	})
	out := r.handleEnvelope(context.Background(), acker, socketmode.Request{
		Type: socketmode.RequestTypeSlashCommands, EnvelopeID: "env-slash", Payload: payload, AcceptsResponsePayload: true,
	})
	if !out.acked || out.status != http.StatusOK {
		t.Fatalf("outcome = %+v, want acked 200", out)
	}
	acks := acker.acks()
	if len(acks) != 1 || acks[0].envelopeID != "env-slash" {
		t.Fatalf("acks = %+v", acks)
	}
	var resp slackInteractionResponse
	if err := json.Unmarshal(acks[0].payload, &resp); err != nil {
		t.Fatalf("ack payload not JSON: %v (%s)", err, acks[0].payload)
	}
	if resp.ResponseType != "ephemeral" || !strings.Contains(resp.Text, "No binding for this channel") {
		t.Errorf("ack payload = %+v, want the unbound-channel ephemeral", resp)
	}
	if !strings.Contains(resp.Text, "C77") || !strings.Contains(resp.Text, "T1") {
		t.Errorf("ephemeral text lost channel/team from the form: %q", resp.Text)
	}
}

// An interactive envelope becomes payload=<json>; a JSON body from the
// handler (response_action) is the ack payload, `{}` / empty is a plain ack.
func TestSocketEnvelopeInteractiveAckPayload(t *testing.T) {
	var seenForm url.Values
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"response_action", `{"response_action":"clear"}`, `{"response_action":"clear"}`},
		{"empty object", `{}`, ""},
		{"empty body", ``, ""},
		{"plain text", `ok`, ""},
	} {
		stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			seenForm, _ = url.ParseQuery(string(b))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, tc.body)
		})
		r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, stub, nil)
		acker := &recordingAcker{}
		payload := json.RawMessage(`{"type":"block_actions","team":{"id":"T1"},"actions":[{"action_id":"a"}]}`)
		out := r.handleEnvelope(context.Background(), acker, socketmode.Request{
			Type: socketmode.RequestTypeInteractive, EnvelopeID: "env-i", Payload: payload,
		})
		if !out.acked {
			t.Fatalf("%s: outcome = %+v, want acked", tc.name, out)
		}
		if got := seenForm.Get("payload"); got != string(payload) {
			t.Errorf("%s: handler saw payload=%q, want the raw interaction JSON", tc.name, got)
		}
		acks := acker.acks()
		if len(acks) != 1 {
			t.Fatalf("%s: acks = %+v", tc.name, acks)
		}
		if got := strings.TrimSpace(string(acks[0].payload)); got != tc.want {
			t.Errorf("%s: ack payload = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// slack-go rejects inner event types it has no struct for and surfaces
// the raw frame as ErrorBadMessage. The runner re-decodes the frame and
// still routes + acks it, so an unknown-to-the-library event is neither
// lost nor redelivered forever.
func TestSocketErrorBadMessageStillRoutedAndAcked(t *testing.T) {
	var got atomic.Int32
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"type":"some_future_event"`) {
			got.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	})
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), stub, nil, nil)
	acker := &recordingAcker{}
	frame := []byte(`{"envelope_id":"env-bad","type":"events_api","accepts_response_payload":false,"payload":{"type":"event_callback","team_id":"T1","event_id":"EvX","event":{"type":"some_future_event","channel":"C1"}}}`)
	r.handleClientEvent(context.Background(), acker, socketmode.Event{
		Type: socketmode.EventTypeErrorBadMessage,
		Data: &socketmode.ErrorBadMessage{Cause: io.ErrUnexpectedEOF, Message: frame},
	})
	r.inflight.Wait()
	if got.Load() != 1 {
		t.Fatalf("handler saw %d deliveries of the raw frame, want 1", got.Load())
	}
	acks := acker.acks()
	if len(acks) != 1 || acks[0].envelopeID != "env-bad" {
		t.Fatalf("acks = %+v, want env-bad", acks)
	}
	// A frame that is not an envelope at all is counted bad and dropped.
	r.handleClientEvent(context.Background(), acker, socketmode.Event{
		Type: socketmode.EventTypeErrorBadMessage,
		Data: &socketmode.ErrorBadMessage{Cause: io.ErrUnexpectedEOF, Message: []byte(`{"type":"mystery"}`)},
	})
	r.inflight.Wait()
	if len(acker.acks()) != 1 || r.envelopesBad.Load() != 1 {
		t.Errorf("non-envelope frame: acks=%d bad=%d, want 1/1", len(acker.acks()), r.envelopesBad.Load())
	}
}

func TestSlashPayloadToForm(t *testing.T) {
	form, err := slashPayloadToForm(json.RawMessage(`{"command":"/gc","text":"hi there","is_enterprise_install":false,"n":3,"nested":{"a":1},"nil":null}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"command": "/gc", "text": "hi there", "is_enterprise_install": "false", "n": "3", "nested": `{"a":1}`}
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("form[%s] = %q, want %q", k, got, v)
		}
	}
	if _, ok := form["nil"]; ok {
		t.Errorf("null field should be dropped")
	}
	if _, err := slashPayloadToForm(json.RawMessage(`{}`)); err == nil {
		t.Errorf("empty payload should error")
	}
	if _, err := slashPayloadToForm(json.RawMessage(`[1]`)); err == nil {
		t.Errorf("non-object payload should error")
	}
}

func TestSocketModePolicy(t *testing.T) {
	for _, tc := range []struct {
		policy, token string
		want          bool
	}{
		{socketModePolicyAuto, "", false},
		{socketModePolicyAuto, "xapp-1", true},
		{socketModePolicyOff, "xapp-1", false},
		{socketModePolicyOn, "xapp-1", true},
	} {
		if got := socketModeEnabled(config{socketMode: tc.policy, slackAppToken: tc.token}); got != tc.want {
			t.Errorf("policy=%s token=%q: enabled=%v, want %v", tc.policy, tc.token, got, tc.want)
		}
	}
}

func TestLoadConfigSocketModeAndLiveness(t *testing.T) {
	base := baseSlackEnv()
	// Defaults.
	cfg, err := loadConfigFromEnv(stubEnv(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.socketMode != socketModePolicyAuto || cfg.slackAppToken != "" || socketModeEnabled(cfg) {
		t.Errorf("defaults: socketMode=%q token=%q enabled=%v", cfg.socketMode, cfg.slackAppToken, socketModeEnabled(cfg))
	}
	if cfg.livenessStallAfter != 10*time.Minute || cfg.backfillMaxWindow != time.Hour {
		t.Errorf("defaults: stall_after=%s backfill_window=%s", cfg.livenessStallAfter, cfg.backfillMaxWindow)
	}
	if !strings.HasSuffix(cfg.livenessStatePath, "inbound_liveness.json") {
		t.Errorf("default state path = %q", cfg.livenessStatePath)
	}

	// Token present → auto enables; lists/durations parse.
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	env["SLACK_APP_TOKEN"] = " xapp-1-abc "
	env["SLACK_LIVENESS_CHANNELS"] = "C1, C2,,C3"
	env["SLACK_LIVENESS_STALL_AFTER"] = "3m"
	env["SLACK_BACKFILL_MAX_WINDOW"] = "0"
	env["SLACK_LIVENESS_ALERT_CHANNEL"] = "C0ALERT"
	cfg, err = loadConfigFromEnv(stubEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.slackAppToken != "xapp-1-abc" || !socketModeEnabled(cfg) {
		t.Errorf("token=%q enabled=%v", cfg.slackAppToken, socketModeEnabled(cfg))
	}
	if len(cfg.livenessChannels) != 3 || cfg.livenessChannels[2] != "C3" {
		t.Errorf("channels = %v", cfg.livenessChannels)
	}
	if cfg.livenessStallAfter != 3*time.Minute || cfg.backfillMaxWindow != 0 || cfg.livenessAlertChannel != "C0ALERT" {
		t.Errorf("parsed: stall_after=%s backfill_window=%s alert=%q", cfg.livenessStallAfter, cfg.backfillMaxWindow, cfg.livenessAlertChannel)
	}
	// Set-but-empty state path disables persistence (lookup semantics,
	// like BUSY_REACTION=).
	cfg, err = loadConfigFromLookup(func(k string) (string, bool) {
		if k == "SLACK_LIVENESS_STATE_PATH" {
			return "", true
		}
		v, ok := base[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.livenessStatePath != "" {
		t.Errorf("SLACK_LIVENESS_STATE_PATH= should disable persistence, got %q", cfg.livenessStatePath)
	}

	// Rejections.
	for name, mut := range map[string]func(map[string]string){
		"bad policy":        func(m map[string]string) { m["SLACK_SOCKET_MODE"] = "maybe" },
		"on without token":  func(m map[string]string) { m["SLACK_SOCKET_MODE"] = "on" },
		"bot token as app":  func(m map[string]string) { m["SLACK_APP_TOKEN"] = "xoxb-nope" },
		"bad stall":         func(m map[string]string) { m["SLACK_LIVENESS_STALL_AFTER"] = "soon" },
		"negative backfill": func(m map[string]string) { m["SLACK_BACKFILL_MAX_WINDOW"] = "-1h" },
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		mut(env)
		if _, err := loadConfigFromEnv(stubEnv(env)); err == nil {
			t.Errorf("%s: expected config error", name)
		}
	}
}

// /healthz renders the transport + liveness lines only once wired, and
// the bare handler keeps its two-line contract.
func TestHealthzReportsTransportAndLivenessWhenWired(t *testing.T) {
	w := httptest.NewRecorder()
	handleHealthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if body := w.Body.String(); strings.Contains(body, "socket_mode=") || strings.Contains(body, "inbound_liveness=") {
		t.Fatalf("unwired healthz = %q, want no transport/liveness lines", body)
	}
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	socketModeHealth.Store(r)
	t.Cleanup(func() { socketModeHealth.Store(nil) })
	l := newInboundLiveness(livenessTestConfig(t), nil)
	livenessHealth.Store(l)
	t.Cleanup(func() { livenessHealth.Store(nil) })
	w = httptest.NewRecorder()
	handleHealthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	body := w.Body.String()
	if !strings.HasPrefix(body, "ok\ndispatch_dropped_total=") {
		t.Fatalf("healthz head = %q", body)
	}
	if !strings.Contains(body, "socket_mode=connecting") || !strings.Contains(body, "inbound_liveness=ok liveness_channels=1") {
		t.Errorf("healthz = %q", body)
	}
}

// Real round-trip: a fake Slack serves apps.connections.open and a
// WebSocket that sends hello + an events_api envelope; the runner (real
// slack-go client) must forward the payload to the handler and write
// back an ack for the envelope id.
func TestSocketModeWebSocketRoundTrip(t *testing.T) {
	if os.Getenv("CI_NO_NET") != "" {
		t.Skip("loopback websocket test disabled")
	}
	type ack struct {
		EnvelopeID string          `json:"envelope_id"`
		Payload    json.RawMessage `json:"payload"`
	}
	ackCh := make(chan ack, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var wsURL atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			if got := r.Header.Get("Authorization"); got != "Bearer xapp-1-test" {
				t.Errorf("apps.connections.open auth = %q, want the app-level token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"url":"`+*wsURL.Load()+`"}`)
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1, "connection_info": map[string]any{"app_id": "A1"}})
			env := map[string]any{
				"envelope_id": "env-ws-1", "type": "events_api", "accepts_response_payload": false,
				"payload": json.RawMessage(eventEnvelopeBody(t, "EvWS", "5.0", "over the socket")),
			}
			_ = conn.WriteJSON(env)
			for {
				var a ack
				if err := conn.ReadJSON(&a); err != nil {
					return
				}
				ackCh <- a
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	wsURL.Store(&u)

	prev := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() { slackAPIBase = prev })

	var seen atomic.Int32
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "over the socket") && isTrustedTransportRequest(r) {
			seen.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	})
	cfg := socketTestConfig(t, "http://127.0.0.1:0")
	r := newSocketModeRunner(cfg, stub, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()

	select {
	case a := <-ackCh:
		if a.EnvelopeID != "env-ws-1" {
			t.Errorf("ack envelope_id = %q, want env-ws-1", a.EnvelopeID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no ack written to the websocket within 10s")
	}
	if seen.Load() != 1 {
		t.Errorf("handler saw %d socket deliveries, want 1", seen.Load())
	}
	if !r.connected.Load() || r.connectCount.Load() != 1 {
		t.Errorf("status connected=%v connections=%d", r.connected.Load(), r.connectCount.Load())
	}
	if h := r.healthzDetail(); !strings.Contains(h, "socket_mode=connected") || !strings.Contains(h, "socket_acked=1") {
		t.Errorf("healthzDetail = %q", h)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop after cancel")
	}
}
