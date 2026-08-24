package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/slack-go/slack"
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

// Advisory degraded-health headers (gp-rol): /healthz stays 200 — gc
// must keep routing outbound /publish — but carries X-GC-Health:
// degraded when the liveness watchdog has a confirmed stall or the
// socket transport is down past its grace window, so `gc service list`
// stops reporting ready while the event stream is dead.
func TestHealthzAdvisoryDegradedHeaders(t *testing.T) {
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handleHealthz(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return w
	}

	// Unwired handler: 200, no advisory headers.
	w := get()
	if w.Code != http.StatusOK {
		t.Fatalf("unwired healthz code = %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-GC-Health"); got != "" {
		t.Fatalf("unwired X-GC-Health = %q, want unset", got)
	}

	// Confirmed liveness stall → degraded, still 200.
	l := newInboundLiveness(livenessTestConfig(t), nil)
	l.mu.Lock()
	l.stalledSince = time.Now().Add(-5 * time.Minute)
	l.lastInboundAt = time.Now().Add(-30 * time.Minute)
	l.mu.Unlock()
	l.lastMissed.Store(7)
	livenessHealth.Store(l)
	t.Cleanup(func() { livenessHealth.Store(nil) })
	w = get()
	if w.Code != http.StatusOK {
		t.Fatalf("stalled healthz code = %d, want 200 (advisory only)", w.Code)
	}
	if got := w.Header().Get("X-GC-Health"); got != "degraded" {
		t.Fatalf("stalled X-GC-Health = %q, want degraded", got)
	}
	reason := w.Header().Get("X-GC-Health-Reason")
	if !strings.Contains(reason, "inbound_liveness stalled") || !strings.Contains(reason, "missed=7") {
		t.Fatalf("stalled reason = %q", reason)
	}

	// Socket down past its grace window → its reason joins the header.
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	r.startedAt = time.Now().Add(-10 * time.Minute)
	socketModeHealth.Store(r)
	t.Cleanup(func() { socketModeHealth.Store(nil) })
	reason = get().Header().Get("X-GC-Health-Reason")
	if !strings.Contains(reason, "inbound_liveness stalled") || !strings.Contains(reason, "socket_mode never connected") {
		t.Fatalf("combined reason = %q", reason)
	}

	// Recovery on both fronts clears the headers.
	l.mu.Lock()
	l.stalledSince = time.Time{}
	l.mu.Unlock()
	r.connected.Store(true)
	w = get()
	if got := w.Header().Get("X-GC-Health"); got != "" {
		t.Fatalf("recovered X-GC-Health = %q, want unset (reason=%q)", got, w.Header().Get("X-GC-Health-Reason"))
	}
}

// headerSafe strips control characters (remote error text must not be
// able to corrupt the health response) and bounds the value.
func TestHeaderSafe(t *testing.T) {
	if got := headerSafe("a\r\nb\x00c\x7fd", 512); got != "a  b c d" {
		t.Fatalf("headerSafe = %q", got)
	}
	if got := headerSafe(strings.Repeat("x", 600), 512); len(got) != 512 {
		t.Fatalf("headerSafe len = %d, want 512", len(got))
	}
}

// degradedReason's grace window: transient disconnects (the normal
// slack-go reconnect churn) report nothing; only sustained downtime —
// measured from the LATER of runner start and last disconnect — trips
// the advisory signal.
func TestSocketDegradedReasonGraceWindow(t *testing.T) {
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	now := time.Now()

	if got := r.degradedReason(now); got != "" {
		t.Fatalf("fresh runner degradedReason = %q, want empty (grace)", got)
	}
	r.startedAt = now.Add(-3 * time.Minute)
	if got := r.degradedReason(now); !strings.Contains(got, "never connected") {
		t.Fatalf("never-connected degradedReason = %q, want never connected", got)
	}
	r.setLastErr("invalid_auth")
	if got := r.degradedReason(now); !strings.Contains(got, "invalid_auth") {
		t.Fatalf("degradedReason = %q, want last_error included", got)
	}

	r.connected.Store(true)
	r.everConnected.Store(true)
	if got := r.degradedReason(now); got != "" {
		t.Fatalf("connected degradedReason = %q, want empty", got)
	}

	r.startedAt = now.Add(-10 * time.Minute)
	r.connected.Store(false)
	r.lastDisconnectAt.Store(now.Add(-time.Minute).UnixNano())
	if got := r.degradedReason(now); got != "" {
		t.Fatalf("1m-disconnected degradedReason = %q, want empty (grace)", got)
	}
	r.lastDisconnectAt.Store(now.Add(-3 * time.Minute).UnixNano())
	if got := r.degradedReason(now); !strings.Contains(got, "socket_mode disconnected for 3m0s") {
		t.Fatalf("3m-disconnected degradedReason = %q", got)
	}

	var nilRunner *socketModeRunner
	if got := nilRunner.degradedReason(now); got != "" {
		t.Fatalf("nil runner degradedReason = %q, want empty", got)
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

// gp-bsk: the outer reconnect ladder never exceeds the ceiling under
// repeated failed cycles, resets on a connected cycle, and resets
// (one-shot) when the liveness alarm has demanded aggressive reconnects.
func TestSocketOuterBackoffCeilingAndResets(t *testing.T) {
	if socketReconnectBackoffMax > 2*time.Minute {
		t.Fatalf("socketReconnectBackoffMax = %s, want <= 2m (gp-bsk: hour-scale waits turned a DNS blip into a 10.5h outage)", socketReconnectBackoffMax)
	}
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	b := socketReconnectBackoffMin
	for i := 0; i < 20; i++ {
		b = r.nextBackoff(b, false)
		if b > socketReconnectBackoffMax {
			t.Fatalf("iteration %d: backoff %s exceeds ceiling %s", i, b, socketReconnectBackoffMax)
		}
	}
	if b != socketReconnectBackoffMax {
		t.Fatalf("ladder converged to %s, want the %s ceiling", b, socketReconnectBackoffMax)
	}
	if got := r.nextBackoff(b, true); got != socketReconnectBackoffMin {
		t.Fatalf("connected cycle backoff = %s, want floor %s", got, socketReconnectBackoffMin)
	}
	r.aggressive.Store(true)
	if got := r.nextBackoff(socketReconnectBackoffMax, false); got != socketReconnectBackoffMin {
		t.Fatalf("aggressive backoff = %s, want floor %s", got, socketReconnectBackoffMin)
	}
	if got := r.nextBackoff(socketReconnectBackoffMax, false); got != socketReconnectBackoffMax {
		t.Fatalf("aggressive flag not one-shot: backoff = %s, want %s", got, socketReconnectBackoffMax)
	}
}

// gp-bsk: a ConnectionError reporting an internal backoff above the
// ceiling kills the client cycle (slack-go v0.29.0's internal Max is
// broken — observed backoff=1h49m on 2026-08-23); a reasonable backoff
// leaves the cycle alone.
func TestSocketInternalBackoffOverCeilingKillsCycle(t *testing.T) {
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)

	cycleCtx, cancel := context.WithCancel(context.Background())
	r.setCycleCancel(cancel)
	r.handleClientEvent(context.Background(), &recordingAcker{}, socketmode.Event{
		Type: socketmode.EventTypeConnectionError,
		Data: &slack.ConnectionErrorEvent{Attempt: 5, Backoff: 30 * time.Second, ErrorObj: errors.New("dial tcp: i/o timeout")},
	})
	if cycleCtx.Err() != nil {
		t.Fatal("30s internal backoff killed the cycle; want it left alone")
	}

	r.handleClientEvent(context.Background(), &recordingAcker{}, socketmode.Event{
		Type: socketmode.EventTypeConnectionError,
		Data: &slack.ConnectionErrorEvent{Attempt: 17, Backoff: 109 * time.Minute, ErrorObj: errors.New("dial tcp: i/o timeout")},
	})
	if cycleCtx.Err() == nil {
		t.Fatalf("109m internal backoff (over the %s ceiling) did not kill the cycle", socketReconnectBackoffMax)
	}
	if got := r.failStreak.Load(); got != 2 {
		t.Errorf("failStreak = %d, want 2", got)
	}
}

// gp-bsk: consecutive DNS not-found failures flip the sticky
// fresh-resolve mode (pure-Go resolver) and rebuild the client; an
// interleaved non-DNS failure resets the streak; a Connected event
// resets both streaks.
func TestSocketDNSStreakTriggersFreshResolve(t *testing.T) {
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	if res := r.newNetDialer().Resolver; res != nil {
		t.Fatalf("fresh runner dials with resolver %v, want nil (process default)", res)
	}

	dnsErr := &net.DNSError{Err: "no such host", Name: "slack.com", IsNotFound: true}
	if !isDNSNotFound(dnsErr) {
		t.Fatal("typed *net.DNSError(IsNotFound) not classified as DNS not-found")
	}
	// slack-go can flatten the chain into text (apps.connections.open
	// ok=false surfaces the raw string): the classifier must still trip.
	if !isDNSNotFound(errors.New("connection failed: lookup slack.com: no such host")) {
		t.Fatal("flattened no-such-host string not classified as DNS not-found")
	}
	if isDNSNotFound(errors.New("dial tcp: i/o timeout")) || isDNSNotFound(nil) {
		t.Fatal("non-DNS error classified as DNS not-found")
	}

	// Two DNS failures then a non-DNS failure: streak resets, no flip.
	r.noteConnectionError(dnsErr, 0)
	r.noteConnectionError(dnsErr, 0)
	r.noteConnectionError(errors.New("dial tcp: i/o timeout"), 0)
	if r.freshResolve.Load() {
		t.Fatal("fresh-resolve flipped after an interrupted DNS streak")
	}

	cycleCtx, cancel := context.WithCancel(context.Background())
	r.setCycleCancel(cancel)
	r.noteConnectionError(dnsErr, 0)
	r.noteConnectionError(dnsErr, 0)
	if r.freshResolve.Load() {
		t.Fatal("fresh-resolve flipped before the streak threshold")
	}
	r.noteConnectionError(dnsErr, 0)
	if !r.freshResolve.Load() {
		t.Fatalf("fresh-resolve not flipped after %d consecutive DNS failures", socketDNSStreakForFreshResolve)
	}
	if cycleCtx.Err() == nil {
		t.Fatal("cycle not killed on fresh-resolve flip (client must be rebuilt with the new resolver)")
	}
	// The flip must reach the wire: the net.Dialer both the HTTP
	// transport and the WebSocket dialer are built from now carries the
	// pure-Go resolver.
	res := r.newNetDialer().Resolver
	if res == nil || !res.PreferGo {
		t.Fatalf("dialer resolver after flip = %+v, want PreferGo", res)
	}

	// A Connected event clears the streaks (and fresh-resolve stays
	// sticky — the pure-Go resolver is a fine steady state).
	r.handleClientEvent(context.Background(), &recordingAcker{}, socketmode.Event{Type: socketmode.EventTypeConnected})
	if r.dnsStreak.Load() != 0 || r.failStreak.Load() != 0 {
		t.Fatalf("streaks after Connected = dns %d fail %d, want 0 0", r.dnsStreak.Load(), r.failStreak.Load())
	}
	if !r.freshResolve.Load() {
		t.Fatal("fresh-resolve did not stay sticky across a reconnect")
	}
}

// gp-bsk: the self-restart fires only when the transport once worked
// and the CURRENT failure streak has been running past the configured
// window — never for a never-connected misconfiguration (which would
// restart-loop), never keyed off the wall-clock disconnect timestamp
// (a laptop that slept mid-outage carries an hours-old lastDisconnectAt
// on wake; the streak-start time.Time keeps its monotonic reading, and
// monotonic clocks pause across suspend, so only awake failing time
// spends the budget).
func TestSocketSelfRestartGating(t *testing.T) {
	cfg := socketTestConfig(t, "http://127.0.0.1:0")
	cfg.socketSelfRestartAfter = 10 * time.Minute
	r := newSocketModeRunner(cfg, nil, nil, nil)
	var exits atomic.Int32
	r.exit = func(code int) { exits.Add(1) }
	now := time.Now()
	streakSince := func(d time.Duration) {
		ts := now.Add(-d) // Add preserves the monotonic reading
		r.failStreakStart.Store(&ts)
	}
	r.failStreak.Store(socketSelfRestartMinFailures)
	streakSince(11 * time.Minute)

	r.startedAt = now.Add(-2 * time.Hour)
	r.maybeSelfRestart(now)
	if exits.Load() != 0 {
		t.Fatal("never-connected runner self-restarted (would restart-loop on a misconfig)")
	}

	r.everConnected.Store(true)
	streakSince(5 * time.Minute)
	r.maybeSelfRestart(now)
	if exits.Load() != 0 {
		t.Fatal("self-restart fired at 5m of failing, want only past the 10m window")
	}

	// The codex-gate scenario: an ancient disconnect timestamp (e.g.
	// wall clock spanning a sleep) with a young failure streak must NOT
	// fire — the gate keys off the streak start, not lastDisconnectAt.
	r.lastDisconnectAt.Store(now.Add(-2 * time.Hour).UnixNano())
	streakSince(time.Minute)
	r.maybeSelfRestart(now)
	if exits.Load() != 0 {
		t.Fatal("self-restart keyed off the wall-clock disconnect time, want the failure-streak start")
	}

	// No streak-start recorded (e.g. streak counter carried over a
	// Connected reset race): never fire.
	r.failStreakStart.Store(nil)
	r.maybeSelfRestart(now)
	if exits.Load() != 0 {
		t.Fatal("self-restart fired with no streak start recorded")
	}

	streakSince(11 * time.Minute)
	r.failStreak.Store(1)
	r.maybeSelfRestart(now)
	if exits.Load() != 0 {
		t.Fatal("self-restart fired below the failure-streak threshold")
	}

	r.failStreak.Store(socketSelfRestartMinFailures)
	r.maybeSelfRestart(now)
	if exits.Load() != 1 {
		t.Fatalf("exits = %d, want 1 (11m failing + streak + everConnected)", exits.Load())
	}

	r.connected.Store(true)
	r.maybeSelfRestart(now)
	if exits.Load() != 1 {
		t.Fatal("self-restart fired while connected")
	}

	r.connected.Store(false)
	r.cfg.socketSelfRestartAfter = 0
	r.maybeSelfRestart(now)
	if exits.Load() != 1 {
		t.Fatal("self-restart fired with the knob disabled (0)")
	}
}

// gp-bsk: the liveness alarm flips a down socket runner into aggressive
// reconnect — backoff floor, kick past the sleep, in-flight cycle
// killed — and leaves a connected runner alone.
func TestSocketInboundAlarmAggressiveReconnect(t *testing.T) {
	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	cycleCtx, cancel := context.WithCancel(context.Background())
	r.setCycleCancel(cancel)

	r.connected.Store(true)
	r.onInboundAlarm()
	if r.aggressive.Load() || len(r.kick) != 0 || cycleCtx.Err() != nil {
		t.Fatal("alarm while connected must be a no-op (the stall is elsewhere)")
	}

	r.connected.Store(false)
	r.onInboundAlarm()
	if !r.aggressive.Load() {
		t.Fatal("alarm did not arm the aggressive-backoff reset")
	}
	if len(r.kick) != 1 {
		t.Fatal("alarm did not queue a kick to skip the backoff sleep")
	}
	if cycleCtx.Err() == nil {
		t.Fatal("alarm did not kill the in-flight cycle")
	}
	var nilRunner *socketModeRunner
	nilRunner.onInboundAlarm() // must not panic

	// End-to-end: the watchdog's alarm() reaches the runner through the
	// socketModeHealth singleton.
	prev := socketModeHealth.Load()
	socketModeHealth.Store(r)
	t.Cleanup(func() { socketModeHealth.Store(prev) })
	r.aggressive.Store(false)
	l := newInboundLiveness(socketTestConfig(t, "http://127.0.0.1:0"), nil)
	l.alarm("test", []missedMessage{{channel: "C1", msg: slackHistoryMessage{TS: "1.0"}}}, map[string]int{"C1": 1})
	if !r.aggressive.Load() {
		t.Fatal("liveness alarm did not reach the socket runner")
	}
}

// gp-bsk: end-to-end recovery FLOW for the DNS-poison incident class,
// against a fake Slack. apps.connections.open fails three times with
// the exact error text the poisoned resolver produced on 8/22+8/23
// ("no such host", surfaced through slack-go's flattening ok=false
// path), which must flip fresh-resolve and rebuild the client; the
// liveness alarm then skips the outer backoff (the escalation under
// test — without it the reconnect waits out the 5s outer floor), and
// the runner must be connected again well inside the backoff ceiling.
// Deliberately hermetic: no real DNS lookup happens (the fake is a
// loopback IP), so the resolver actually reaching the dialers is
// pinned separately in TestSocketDNSStreakTriggersFreshResolve via
// newNetDialer; bypassing a genuinely poisoned OS resolver is not
// testable from inside the process.
func TestSocketDNSPoisonRecoveryReconnectsWithinCeiling(t *testing.T) {
	if os.Getenv("CI_NO_NET") != "" {
		t.Skip("loopback websocket test disabled")
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var wsURL atomic.Pointer[string]
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			w.Header().Set("Content-Type", "application/json")
			if opens.Add(1) <= 3 {
				_, _ = io.WriteString(w, `{"ok":false,"error":"connection failed: lookup slack.com: no such host"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"url":"`+*wsURL.Load()+`"}`)
		case "/ws":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteJSON(map[string]any{"type": "hello", "num_connections": 1, "connection_info": map[string]any{"app_id": "A1"}})
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
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

	r := newSocketModeRunner(socketTestConfig(t, "http://127.0.0.1:0"), nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() { r.run(ctx); close(done) }()

	await := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (opens=%d)", what, opens.Load())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	await("fresh-resolve flip after the DNS streak", func() bool { return r.freshResolve.Load() })
	// The watchdog alarm escalation: skip the outer backoff sleep.
	r.onInboundAlarm()
	await("reconnect", func() bool { return r.connected.Load() })

	if elapsed := time.Since(start); elapsed > socketReconnectBackoffMax {
		t.Errorf("recovery took %s, want well inside the %s ceiling", elapsed, socketReconnectBackoffMax)
	}
	if got := opens.Load(); got < 4 {
		t.Errorf("apps.connections.open calls = %d, want >= 4 (3 poisoned + 1 clean)", got)
	}
	if h := r.healthzDetail(); !strings.Contains(h, "socket_fresh_resolve=true") {
		t.Errorf("healthzDetail = %q, want socket_fresh_resolve=true", h)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop after cancel")
	}
}
