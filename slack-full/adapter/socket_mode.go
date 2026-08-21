package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// Socket Mode inbound transport (gp-3og / ci-mk4qj).
//
// WHY: the Events API delivers over a public HTTPS Request URL — here a
// Tailscale Funnel in front of the public listener. On 2026-08-19 that
// path died silently for ~26 minutes (Slack showed the URL verified,
// POSTs simply stopped arriving) and the adapter had no way to notice.
// Socket Mode inverts the direction: the adapter dials OUT to Slack
// over a WebSocket authenticated by an app-level token
// (SLACK_APP_TOKEN, scope connections:write), Slack pushes envelopes
// down that socket, and the adapter acks each one by envelope_id. No
// public endpoint, no funnel, no signing secret on the inbound path.
//
// Design (locked in ci-mk4qj):
//
//   - slack-go/slack's socketmode package owns the connection lifecycle
//     (apps.connections.open, dial, ping/pong deadman, reconnect on
//     Slack's `disconnect` envelope). This file owns everything above
//     it: envelope → handler translation, ack policy, status, and the
//     outer reconnect loop for the cases RunContext gives up on.
//   - Every envelope is translated into the SAME HTTP request the
//     Events API / interactions endpoints would have received and run
//     through the SAME handlers (handleSlackEvents,
//     handleSlackInteractions) via an in-memory ResponseWriter. Routing,
//     company admission, event_id dedup (hw-94w5k), busy reactions,
//     load-shedding — nothing forks. The only difference is the HMAC
//     check, skipped because the request carries the trusted-transport
//     context marker (inbound_transport.go).
//   - Ack < 3s: the handler's recorded status decides. 2xx → ack. 503
//     (company gateway: store failure / startup barrier) → deliberately
//     NOT acked, so Slack redelivers the envelope with retry_attempt
//     bumped — the socket-side twin of the Events API retry ladder the
//     gateway already relies on. Other 4xx → ack (a retry cannot help).
//     Slash commands and interactive payloads ack WITH the handler's
//     JSON body (ephemeral text, response_action) as the ack payload.
//   - retry_attempt / retry_reason on the envelope map onto the
//     X-Slack-Retry-Num / X-Slack-Retry-Reason headers so the
//     receipt-store bookkeeping and log lines read identically.
//   - Unknown inner event types: slack-go's parser rejects event types it
//     has no struct for and surfaces the raw frame as ErrorBadMessage.
//     We re-decode the frame ourselves and route it through the same
//     raw-payload path, so such envelopes are still delivered AND acked
//     (an un-acked envelope would otherwise be redelivered forever).
//
// The Events API listener stays up alongside the socket. Slack routes
// an app's events to exactly one of the two (the "Socket Mode" toggle
// in the app config), so nothing double-delivers; leaving the HTTP path
// wired keeps the rollback a UI flip away and keeps serving any other
// app (agent apps, OAuth, interactions) still on URLs.

// Socket Mode policy values for SLACK_SOCKET_MODE.
const (
	socketModePolicyAuto = "auto"
	socketModePolicyOn   = "on"
	socketModePolicyOff  = "off"
)

// socketModeEnabled reports whether main() should start the socket
// runner for this config.
func socketModeEnabled(cfg config) bool {
	switch cfg.socketMode {
	case socketModePolicyOff:
		return false
	case socketModePolicyOn:
		return true
	default:
		return cfg.slackAppToken != ""
	}
}

// socketAckBudget bounds how long the runner spends handing one ack to
// the client's send queue. Slack's window is 3s from envelope receipt.
const socketAckBudget = 5 * time.Second

// socketSlowHandlerWarn is the handler wall-time above which the runner
// logs a warning: the ack is sent only after the handler returns, so a
// handler slower than this is eating into (or past) Slack's 3s budget.
const socketSlowHandlerWarn = 2 * time.Second

// socketReconnectBackoffMin/Max bound the OUTER reconnect loop — the
// one that recreates the slack-go client after RunContext returns
// (which it does only on fatal auth errors or after its own retries
// give up). Auth failures are logged loudly each cycle but still
// retried: a rotated token shows up in the env on the next restart, and
// an adapter that stops trying is exactly the silent failure this
// transport exists to prevent.
const (
	socketReconnectBackoffMin = 5 * time.Second
	socketReconnectBackoffMax = 5 * time.Minute
)

// socketAcker is the slice of the slack-go client the runner needs to
// acknowledge an envelope. Tests substitute a recorder.
type socketAcker interface {
	AckCtx(ctx context.Context, envelopeID string, payload any) error
}

// socketDegradedAfter is the grace window before a down socket transport
// is reported as advisory-degraded on /healthz (gp-rol): slack-go's own
// reconnects resolve in seconds and the outer ladder caps at
// socketReconnectBackoffMax, so minutes of continuous downtime mean
// reconnection is failing, not in progress.
const socketDegradedAfter = 2 * time.Minute

// socketModeRunner owns one app's Socket Mode connection and its status.
type socketModeRunner struct {
	cfg          config
	events       http.Handler
	interactions http.Handler
	liveness     *inboundLiveness
	startedAt    time.Time

	// newClient builds the slack-go socket client; tests swap it.
	newClient func() *socketmode.Client

	// Status for /healthz and logs. All atomics — read from the health
	// handler concurrently with the run loop.
	connected        atomic.Bool
	everConnected    atomic.Bool
	connectCount     atomic.Int64
	connectErrors    atomic.Int64
	lastConnectedAt  atomic.Int64 // unix nanos; 0 = never
	lastDisconnectAt atomic.Int64
	lastEnvelopeAt   atomic.Int64
	envelopesAcked   atomic.Uint64
	envelopesUnacked atomic.Uint64 // deliberately left for Slack to retry
	envelopesBad     atomic.Uint64 // undecodable / unsupported frames
	lastErr          atomic.Pointer[string]

	// inflight tracks envelope goroutines so shutdown can wait briefly.
	inflight sync.WaitGroup
}

// socketModeHealth is the process-singleton runner pointer read by
// handleHealthz; nil (the default, and the only state tests see)
// reports the transport as disabled.
var socketModeHealth atomic.Pointer[socketModeRunner]

func newSocketModeRunner(cfg config, events, interactions http.Handler, liveness *inboundLiveness) *socketModeRunner {
	r := &socketModeRunner{
		cfg:          cfg,
		events:       events,
		interactions: interactions,
		liveness:     liveness,
		startedAt:    time.Now(),
	}
	r.newClient = func() *socketmode.Client {
		api := slack.New(cfg.slackBotToken,
			slack.OptionAppLevelToken(cfg.slackAppToken),
			slack.OptionAPIURL(strings.TrimRight(slackAPIBase, "/")+"/"),
			slack.OptionHTTPClient(&http.Client{Timeout: 30 * time.Second}),
		)
		return socketmode.New(api, socketmode.OptionLog(log.Default()))
	}
	return r
}

// run blocks until ctx is done, keeping a Socket Mode connection alive
// for the life of the process. Each iteration builds a fresh client,
// drives RunContext (which itself reconnects on Slack-requested
// disconnects and transient dial failures) and consumes its Events
// channel; when RunContext returns, the outer loop backs off and
// rebuilds.
func (r *socketModeRunner) run(ctx context.Context) {
	backoff := socketReconnectBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		connsBefore := r.connectCount.Load()
		client := r.newClient()
		runCtx, cancelRun := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- client.RunContext(runCtx) }()
		err := r.consume(runCtx, client, done)
		cancelRun()
		if r.connected.Load() {
			// The Connecting event stamps ordinary reconnects; this path
			// covers a client loop that ends while still connected, so
			// degradedReason measures the outage from now, not from an
			// older disconnect. Timestamp BEFORE flipping connected:
			// state transitions run on this goroutine only, but health
			// probes read concurrently and must never see
			// connected=false with a stale disconnect time (it would
			// skip the grace window).
			r.lastDisconnectAt.Store(time.Now().UnixNano())
			r.connected.Store(false)
		}
		if ctx.Err() != nil {
			r.inflight.Wait()
			return
		}
		if r.connectCount.Load() > connsBefore {
			// This cycle achieved a connection, so the failure is
			// fresh — reset the ladder for a quick recovery.
			backoff = socketReconnectBackoffMin
		} else {
			// The whole cycle failed to connect (RunContext already
			// exhausted its own retries — typically a fatal auth
			// verdict). Keep retrying — a rotated token appears on
			// restart, and giving up is the silent failure this
			// transport exists to prevent — but back off so a revoked
			// token doesn't hammer apps.connections.open.
			backoff = min(backoff*2, socketReconnectBackoffMax)
		}
		msg := "socket mode: client loop ended"
		if err != nil {
			msg = fmt.Sprintf("socket mode: client loop ended: %v", err)
			r.setLastErr(err.Error())
		}
		log.Printf("%s — reconnecting in %s", msg, backoff)
		select {
		case <-ctx.Done():
			r.inflight.Wait()
			return
		case <-time.After(backoff):
		}
	}
}

// consume drains client.Events until the run goroutine reports (via
// done) or ctx is cancelled. Returns the run error, if any.
func (r *socketModeRunner) consume(ctx context.Context, client *socketmode.Client, done <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case evt := <-client.Events:
			r.handleClientEvent(ctx, client, evt)
		}
	}
}

// handleClientEvent dispatches one slack-go client event: lifecycle
// events update status/logs; envelope events fan out to handleEnvelope
// on their own goroutine so a slow handler never blocks the socket's
// read loop (slack-go's Events channel is bounded).
func (r *socketModeRunner) handleClientEvent(ctx context.Context, acker socketAcker, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnecting:
		if r.connected.Load() {
			// Timestamp before flipping: see the run() disconnect path.
			r.lastDisconnectAt.Store(time.Now().UnixNano())
			r.connected.Store(false)
			log.Printf("socket mode: connection lost — reconnecting")
		}
		if ce, ok := evt.Data.(*slack.ConnectingEvent); ok {
			log.Printf("socket mode: connecting (attempt=%d connection=%d)", ce.Attempt, ce.ConnectionCount)
		} else {
			log.Printf("socket mode: connecting")
		}
	case socketmode.EventTypeConnectionError:
		r.connectErrors.Add(1)
		if ce, ok := evt.Data.(*slack.ConnectionErrorEvent); ok {
			r.setLastErr(ce.Error())
			log.Printf("socket mode: connection error (attempt=%d backoff=%s): %v", ce.Attempt, ce.Backoff, ce.ErrorObj)
		} else {
			log.Printf("socket mode: connection error")
		}
	case socketmode.EventTypeInvalidAuth:
		r.connectErrors.Add(1)
		r.setLastErr("invalid_auth")
		log.Printf("socket mode: INVALID AUTH — SLACK_APP_TOKEN rejected by apps.connections.open (rotate the xapp- token; the adapter keeps retrying)")
	case socketmode.EventTypeConnected:
		n := r.connectCount.Add(1)
		r.connected.Store(true)
		r.everConnected.Store(true)
		r.lastConnectedAt.Store(time.Now().UnixNano())
		reconnect := n > 1
		log.Printf("socket mode: connected (connection=%d reconnect=%v)", n, reconnect)
		if r.liveness != nil {
			// Reconnect backfill: anything posted while the socket was
			// down is fetched from conversations.history and replayed
			// through the same pipeline. The first connection after a
			// restart backfills from the persisted watermark, if any.
			go r.liveness.onTransportConnected(ctx, reconnect)
		}
	case socketmode.EventTypeHello:
		if evt.Request != nil {
			log.Printf("socket mode: hello (num_connections=%d app_id=%s host=%s)",
				evt.Request.NumConnections, evt.Request.ConnectionInfo.AppID, evt.Request.DebugInfo.Host)
		}
	case socketmode.EventTypeDisconnect:
		// slack-go consumes Slack's `disconnect` envelope internally
		// and reconnects; surfaced here only if that ever changes.
		// Timestamp before flipping: see the run() disconnect path.
		r.lastDisconnectAt.Store(time.Now().UnixNano())
		r.connected.Store(false)
		log.Printf("socket mode: disconnect requested by Slack")
	case socketmode.EventTypeIncomingError:
		if e, ok := evt.Data.(error); ok {
			r.setLastErr(e.Error())
			log.Printf("socket mode: incoming error: %v", e)
		} else {
			log.Printf("socket mode: incoming error")
		}
	case socketmode.EventTypeErrorWriteFailed:
		r.envelopesBad.Add(1)
		if e, ok := evt.Data.(*socketmode.ErrorWriteFailed); ok && e != nil {
			r.setLastErr(e.Cause.Error())
			eid := ""
			if e.Response != nil {
				eid = e.Response.EnvelopeID
			}
			log.Printf("socket mode: ack write failed envelope_id=%s: %v (Slack will redeliver)", eid, e.Cause)
		}
	case socketmode.EventTypeErrorBadMessage:
		// Usually an inner event type slack-go has no struct for. The
		// frame is still a well-formed envelope; route it ourselves.
		bad, _ := evt.Data.(*socketmode.ErrorBadMessage)
		if bad == nil {
			r.envelopesBad.Add(1)
			return
		}
		var req socketmode.Request
		if err := json.Unmarshal(bad.Message, &req); err != nil || req.EnvelopeID == "" || !isSocketEnvelopeType(req.Type) {
			r.envelopesBad.Add(1)
			log.Printf("socket mode: dropping undecodable frame (%d bytes): %v", len(bad.Message), bad.Cause)
			return
		}
		log.Printf("socket mode: routing envelope slack-go could not parse (type=%s envelope_id=%s): %v", req.Type, req.EnvelopeID, bad.Cause)
		r.spawnEnvelope(ctx, acker, req)
	case socketmode.EventTypeEventsAPI, socketmode.EventTypeSlashCommand, socketmode.EventTypeInteractive:
		if evt.Request == nil {
			r.envelopesBad.Add(1)
			return
		}
		r.spawnEnvelope(ctx, acker, *evt.Request)
	default:
		log.Printf("socket mode: ignoring client event type=%s", evt.Type)
	}
}

func isSocketEnvelopeType(t string) bool {
	switch t {
	case socketmode.RequestTypeEventsAPI, socketmode.RequestTypeSlashCommands, socketmode.RequestTypeInteractive:
		return true
	}
	return false
}

func (r *socketModeRunner) spawnEnvelope(ctx context.Context, acker socketAcker, req socketmode.Request) {
	r.inflight.Add(1)
	go func() {
		defer r.inflight.Done()
		r.handleEnvelope(ctx, acker, req)
	}()
}

// socketEnvelopeOutcome is what handleEnvelope decided for one envelope;
// returned for tests and logging.
type socketEnvelopeOutcome struct {
	status  int
	acked   bool
	payload json.RawMessage
	err     error
}

// handleEnvelope translates one Socket Mode envelope into the HTTP
// request its Events API / interactions twin would have been, runs the
// matching handler, and acks per the recorded status.
func (r *socketModeRunner) handleEnvelope(ctx context.Context, acker socketAcker, req socketmode.Request) socketEnvelopeOutcome {
	r.lastEnvelopeAt.Store(time.Now().UnixNano())
	started := time.Now()
	var out socketEnvelopeOutcome
	switch req.Type {
	case socketmode.RequestTypeEventsAPI:
		out = r.serveEventsEnvelope(ctx, req)
	case socketmode.RequestTypeSlashCommands:
		out = r.serveSlashEnvelope(ctx, req)
	case socketmode.RequestTypeInteractive:
		out = r.serveInteractiveEnvelope(ctx, req)
	default:
		r.envelopesBad.Add(1)
		log.Printf("socket mode: unsupported envelope type=%q envelope_id=%s — acking to stop redelivery", req.Type, req.EnvelopeID)
		out = socketEnvelopeOutcome{acked: true}
	}
	if took := time.Since(started); took > socketSlowHandlerWarn {
		log.Printf("socket mode: SLOW handler type=%s envelope_id=%s took=%s (Slack ack budget is 3s)", req.Type, req.EnvelopeID, took.Round(time.Millisecond))
	}
	if !out.acked {
		r.envelopesUnacked.Add(1)
		log.Printf("socket mode: NOT acking envelope_id=%s type=%s status=%d retry_attempt=%d — Slack will redeliver",
			req.EnvelopeID, req.Type, out.status, req.RetryAttempt)
		return out
	}
	ackCtx, cancel := context.WithTimeout(ctx, socketAckBudget)
	defer cancel()
	var payload any
	if len(out.payload) > 0 {
		payload = out.payload
	}
	if err := acker.AckCtx(ackCtx, req.EnvelopeID, payload); err != nil {
		out.err = err
		r.setLastErr(err.Error())
		log.Printf("socket mode: ack failed envelope_id=%s type=%s: %v", req.EnvelopeID, req.Type, err)
		return out
	}
	r.envelopesAcked.Add(1)
	return out
}

// serveEventsEnvelope runs an events_api envelope through
// handleSlackEvents. The payload IS the Events API request body.
func (r *socketModeRunner) serveEventsEnvelope(ctx context.Context, req socketmode.Request) socketEnvelopeOutcome {
	httpReq := r.buildRequest(ctx, "/slack/events", "application/json", req.Payload)
	if req.RetryAttempt > 0 {
		httpReq.Header.Set("X-Slack-Retry-Num", strconv.Itoa(req.RetryAttempt))
		if req.RetryReason != "" {
			httpReq.Header.Set("X-Slack-Retry-Reason", req.RetryReason)
		}
	}
	w := newMemResponseWriter()
	if r.events != nil {
		r.events.ServeHTTP(w, httpReq)
	}
	status := w.Status()
	out := socketEnvelopeOutcome{status: status}
	switch {
	case status >= 200 && status < 300:
		out.acked = true
	case status >= 500:
		// Retryable by contract (company gateway 503 without
		// x-slack-no-retry): leave un-acked so Slack redelivers.
		out.acked = false
	default:
		// 4xx: bad body / rejected — a redelivery would fail the same
		// way; ack so Slack stops, and log the handler's verdict.
		out.acked = true
		log.Printf("socket mode: events handler rejected envelope_id=%s status=%d body=%q", req.EnvelopeID, status, clipBodyForLog(w.Body()))
	}
	return out
}

// serveSlashEnvelope runs a slash_commands envelope through
// handleSlackInteractions as the form POST Slack would have sent, and
// returns the handler's JSON body (ephemeral reply) as the ack payload.
func (r *socketModeRunner) serveSlashEnvelope(ctx context.Context, req socketmode.Request) socketEnvelopeOutcome {
	form, err := slashPayloadToForm(req.Payload)
	if err != nil {
		log.Printf("socket mode: slash envelope_id=%s undecodable payload: %v — acking", req.EnvelopeID, err)
		return socketEnvelopeOutcome{status: http.StatusBadRequest, acked: true}
	}
	httpReq := r.buildRequest(ctx, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form.Encode()))
	w := newMemResponseWriter()
	if r.interactions != nil {
		r.interactions.ServeHTTP(w, httpReq)
	}
	out := socketEnvelopeOutcome{status: w.Status(), acked: true}
	out.payload = ackPayloadFromBody(w)
	if out.status >= 400 {
		log.Printf("socket mode: slash handler status=%d envelope_id=%s body=%q", out.status, req.EnvelopeID, clipBodyForLog(w.Body()))
	}
	return out
}

// serveInteractiveEnvelope runs an interactive envelope (block_actions,
// view_submission, shortcut, …) through handleSlackInteractions as the
// `payload=<json>` form POST, returning any JSON body (response_action)
// as the ack payload.
func (r *socketModeRunner) serveInteractiveEnvelope(ctx context.Context, req socketmode.Request) socketEnvelopeOutcome {
	form := url.Values{}
	form.Set("payload", string(req.Payload))
	httpReq := r.buildRequest(ctx, "/slack/interactions", "application/x-www-form-urlencoded", []byte(form.Encode()))
	w := newMemResponseWriter()
	if r.interactions != nil {
		r.interactions.ServeHTTP(w, httpReq)
	}
	out := socketEnvelopeOutcome{status: w.Status(), acked: true}
	out.payload = ackPayloadFromBody(w)
	if out.status >= 400 {
		log.Printf("socket mode: interactive handler status=%d envelope_id=%s body=%q", out.status, req.EnvelopeID, clipBodyForLog(w.Body()))
	}
	return out
}

// buildRequest assembles the in-process request for one envelope,
// stamped with the trusted-transport marker so the handler skips HMAC.
func (r *socketModeRunner) buildRequest(ctx context.Context, path, contentType string, body []byte) *http.Request {
	httpReq, _ := http.NewRequestWithContext(withTrustedTransport(ctx, "socket_mode"), http.MethodPost, path, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("User-Agent", "gc-slack-adapter/socket-mode")
	httpReq.ContentLength = int64(len(body))
	httpReq.RemoteAddr = "socket-mode"
	return httpReq
}

// ackPayloadFromBody returns the handler's body as the ack payload when
// it is a 2xx JSON object with content; `{}`, empty bodies, non-JSON
// error text, and non-2xx responses yield a plain ack.
func ackPayloadFromBody(w *memResponseWriter) json.RawMessage {
	if w.Status() < 200 || w.Status() >= 300 {
		return nil
	}
	b := bytes.TrimSpace(w.Body())
	if len(b) == 0 || bytes.Equal(b, []byte("{}")) {
		return nil
	}
	if !json.Valid(b) || b[0] != '{' {
		return nil
	}
	return json.RawMessage(b)
}

// slashPayloadToForm flattens a slash_commands envelope payload (a flat
// JSON object of mostly-string fields) into the
// application/x-www-form-urlencoded shape the interactions handler
// parses. Non-string scalars are stringified; nested values are
// re-encoded as JSON; nulls are dropped.
func slashPayloadToForm(payload json.RawMessage) (url.Values, error) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, errors.New("empty slash command payload")
	}
	form := url.Values{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := m[k].(type) {
		case nil:
			continue
		case string:
			form.Set(k, v)
		case bool:
			form.Set(k, strconv.FormatBool(v))
		case float64:
			form.Set(k, strconv.FormatFloat(v, 'f', -1, 64))
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", k, err)
			}
			form.Set(k, string(b))
		}
	}
	return form, nil
}

func (r *socketModeRunner) setLastErr(s string) {
	s = scrubSlackSecrets(s)
	r.lastErr.Store(&s)
}

// degradedReason reports the advisory service-health reason when the
// socket transport has been down past socketDegradedAfter, "" otherwise
// (gp-rol). Read by handleHealthz for the X-GC-Health header: gc keeps
// routing (outbound is fine) but flips `gc service list` to degraded.
// Nil receiver (transport not wired) reports nothing — the liveness
// watchdog is then the only inbound signal. Note a runner configured
// with SLACK_APP_TOKEN while the Slack app's Socket Mode toggle is off
// never connects, and correctly reports degraded: either the toggle or
// SLACK_SOCKET_MODE=off should change.
func (r *socketModeRunner) degradedReason(now time.Time) string {
	if r == nil || r.connected.Load() {
		return ""
	}
	downSince := r.startedAt
	if ts := r.lastDisconnectAt.Load(); ts > 0 {
		if t := time.Unix(0, ts); t.After(downSince) {
			downSince = t
		}
	}
	down := now.Sub(downSince)
	if down < socketDegradedAfter {
		return ""
	}
	state := "disconnected"
	if !r.everConnected.Load() {
		state = "never connected"
	}
	reason := fmt.Sprintf("socket_mode %s for %s", state, down.Round(time.Second))
	if e := r.lastErr.Load(); e != nil && *e != "" {
		reason += " (last_error=" + *e + ")"
	}
	return reason
}

// healthzDetail renders the socket transport's status lines for
// /healthz. Nil receiver (transport not wired) reports disabled.
func (r *socketModeRunner) healthzDetail() string {
	if r == nil {
		return "socket_mode=disabled\n"
	}
	state := "disconnected"
	if r.connected.Load() {
		state = "connected"
	} else if !r.everConnected.Load() {
		state = "connecting"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "socket_mode=%s socket_connections=%d socket_connect_errors=%d socket_acked=%d socket_unacked=%d socket_bad=%d",
		state, r.connectCount.Load(), r.connectErrors.Load(), r.envelopesAcked.Load(), r.envelopesUnacked.Load(), r.envelopesBad.Load())
	if ts := r.lastConnectedAt.Load(); ts > 0 {
		fmt.Fprintf(&b, " socket_last_connected=%s", time.Unix(0, ts).UTC().Format(time.RFC3339))
	}
	if ts := r.lastDisconnectAt.Load(); ts > 0 {
		fmt.Fprintf(&b, " socket_last_disconnect=%s", time.Unix(0, ts).UTC().Format(time.RFC3339))
	}
	if ts := r.lastEnvelopeAt.Load(); ts > 0 {
		fmt.Fprintf(&b, " socket_last_envelope=%s", time.Unix(0, ts).UTC().Format(time.RFC3339))
	}
	if e := r.lastErr.Load(); e != nil && *e != "" {
		fmt.Fprintf(&b, " socket_last_error=%q", *e)
	}
	b.WriteString("\n")
	return b.String()
}
