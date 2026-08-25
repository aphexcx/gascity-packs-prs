package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
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

// socketReconnectBackoffMin/Max bound EVERY wait between reconnect
// attempts (gp-bsk). Two ladders exist:
//
//   - slack-go's internal connect loop inside RunContext. Its backoff
//     struct declares Max: 5m, but in v0.29.0 that cap is applied only
//     when the bit-shift overflows — the normal path grows 100ms·2^n
//     UNBOUNDED. Observed 2026-08-23: attempt=17 backoff=1h49m, turning
//     a transient DNS blip into a 10.5-hour inbound outage. The runner
//     therefore watches each ConnectionErrorEvent and kills the cycle
//     (killCycle) the moment the reported wait exceeds
//     socketReconnectBackoffMax, handing pacing back to the outer
//     ladder.
//   - the OUTER loop in run(), which recreates the client after
//     RunContext returns and is capped here.
//
// apps.connections.open is cheap and every reconnect is loss-free via
// the watermark backfill, so hour-scale patience is never correct: the
// ceiling keeps the worst-case retry cadence to ~2 minutes. Auth
// failures are logged loudly each cycle but still retried: a rotated
// token shows up in the env on the next restart, and an adapter that
// stops trying is exactly the silent failure this transport exists to
// prevent.
const (
	socketReconnectBackoffMin = 5 * time.Second
	socketReconnectBackoffMax = 2 * time.Minute
)

// DNS self-heal thresholds (gp-bsk). On 2026-08-22 a LAN subnet change
// poisoned the process's resolver state and every apps.connections.open
// failed with "no such host" until a restart. After
// socketDNSStreakForFreshResolve consecutive DNS not-found failures the
// runner stops trusting the in-process resolver: it flips (stickily) to
// a pure-Go resolver that re-reads /etc/resolv.conf and rebuilds the
// client. Should the transport STILL stay dark for
// cfg.socketSelfRestartAfter — across at least
// socketSelfRestartMinFailures attempts, and only after having
// connected at least once this process — the adapter requests its own
// exit: main runs the orderly shutdown (drain → spool → seal, as on
// SIGTERM) and exits 1 so the service supervisor restarts it; spool
// replay + startup recovery + watermark backfill make that loss-free
// (proven in both incidents).
const (
	socketDNSStreakForFreshResolve = 3
	socketSelfRestartMinFailures   = 5
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

	// Reconnect discipline (gp-bsk).
	kick         chan struct{} // wakes run() out of its backoff sleep
	aggressive   atomic.Bool   // next cycle starts at the backoff floor
	freshResolve atomic.Bool   // sticky: dial with the pure-Go resolver
	dnsStreak    atomic.Int32  // consecutive DNS not-found connect errors
	failStreak   atomic.Int32  // connect errors since the last Connected
	// failStreakStart is when the current failure streak began, as a
	// monotonic time.Time (never round-tripped through unix nanos): the
	// self-restart window is measured against it so only awake,
	// actively-failing process time counts — monotonic clocks pause
	// across suspend, so a laptop sleep or wall-clock jump cannot spend
	// the dark-window budget. Nil while no streak is running.
	failStreakStart atomic.Pointer[time.Time]
	// Loopback-latch escape (gp-keg, see dns_latch.go).
	//
	// loopbackDNSStreak counts consecutive connect failures whose lookup
	// reported one of Go's fallback nameservers (127.0.0.1:53 / [::1]:53)
	// — the latched-resolver signature. Reset by any other failure and by
	// Connected.
	loopbackDNSStreak atomic.Int32
	// pinnedLoopbackStreak counts the consecutive latch failures observed
	// with a pin already in place at lookup time — the ones that say the
	// pin is not taking; only these count toward the self-exit. Reset
	// with loopbackDNSStreak, and whenever a lookup ran unpinned.
	pinnedLoopbackStreak atomic.Int32
	// loopbackEvents counts latch events over the process lifetime; it
	// rotates the pin across the file's usable nameservers.
	loopbackEvents atomic.Int32
	// dnsPin is the "host:port" nameserver the pure-Go resolver's Dial
	// hook targets instead of whatever net's cached config chose, set
	// from the runner's own parse of resolv.conf on a latch event; nil =
	// pass-through. Cleared on Connected (transient, re-derived on the
	// next event so it cannot go stale across a later DNS change).
	dnsPin atomic.Pointer[string]
	// resolvConfPath is the file the latch re-parse reads
	// (defaultResolvConfPath); tests point it at a fixture.
	resolvConfPath string
	// exit requests the process exit for the self-restart. main wires
	// it to its orderly shutdown (selfRestartRequests) so buffered
	// inbounds drain to gc or the spool first; the os.Exit default only
	// stands for a runner main never wired. Tests substitute a recorder.
	exit             func(int)
	restartRequested atomic.Bool // exit already called; log/request once
	cancelMu         sync.Mutex
	cycleCancel      context.CancelFunc // cancels the in-flight client cycle

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
		kick:         make(chan struct{}, 1),
		exit:         os.Exit,

		resolvConfPath: defaultResolvConfPath,
	}
	r.newClient = func() *socketmode.Client {
		httpClient, wsDialer := r.newDialers()
		api := slack.New(cfg.slackBotToken,
			slack.OptionAppLevelToken(cfg.slackAppToken),
			slack.OptionAPIURL(strings.TrimRight(slackAPIBase, "/")+"/"),
			slack.OptionHTTPClient(httpClient),
		)
		return socketmode.New(api, socketmode.OptionLog(log.Default()), socketmode.OptionDialer(wsDialer))
	}
	return r
}

// netResolver is the resolver clients dial with: nil (the process
// default) normally, the pure-Go resolver once the DNS self-heal has
// tripped — it bypasses whatever the default resolver has cached and
// re-reads /etc/resolv.conf, which is what actually recovers from the
// 8/22-style poisoned-resolver state without a process restart. The
// pure-Go resolver dials through dialNameserver, so a loopback-latch pin
// (gp-keg) redirects its DNS exchanges away from net's cached config.
func (r *socketModeRunner) netResolver() *net.Resolver {
	if r.freshResolve.Load() {
		return &net.Resolver{PreferGo: true, Dial: r.dialNameserver}
	}
	return nil
}

// dnsDialTimeout bounds one DNS transport dial from the pure-Go resolver;
// net applies its own per-exchange timeout on top.
const dnsDialTimeout = 5 * time.Second

// dialNameserver is the pure-Go resolver's Dial hook (gp-keg): with a
// loopback-latch pin in place every DNS exchange goes to the pinned
// nameserver instead of the address net's cached config chose (Go's
// fallback servers, once it has lost /etc/resolv.conf); without one it
// passes the address through. The pin is read at dial time, so one set
// mid-cycle takes effect on the next lookup without rebuilding the
// client. net only ever passes literal ip:port addresses here.
func (r *socketModeRunner) dialNameserver(ctx context.Context, network, address string) (net.Conn, error) {
	if pin := r.dnsPin.Load(); pin != nil && *pin != "" {
		address = *pin
	}
	d := net.Dialer{Timeout: dnsDialTimeout}
	return d.DialContext(ctx, network, address)
}

// pinnedNameserver returns the current loopback-latch pin, "" if none.
func (r *socketModeRunner) pinnedNameserver() string {
	if pin := r.dnsPin.Load(); pin != nil {
		return *pin
	}
	return ""
}

// newNetDialer builds the one net.Dialer a client cycle dials through —
// the single point where the DNS self-heal's resolver choice takes
// effect, for every lookup the transport performs.
func (r *socketModeRunner) newNetDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  r.netResolver(),
	}
}

// newDialers builds the HTTP client (apps.connections.open) and the
// WebSocket dialer for one client cycle, both routed through the same
// newNetDialer so the DNS self-heal covers every lookup the transport
// performs. The WebSocket HandshakeTimeout and WriteBufferSize mirror
// slack-go's own dialer: its 32KiB write buffer is load-bearing (Slack
// silently drops continuation frames, so acks above gorilla's 4KiB
// default would vanish).
func (r *socketModeRunner) newDialers() (*http.Client, *websocket.Dialer) {
	netDialer := r.newNetDialer()
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           netDialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	wsDialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 45 * time.Second,
		WriteBufferSize:  32 * 1024,
		NetDialContext:   netDialer.DialContext,
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}, wsDialer
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
		r.setCycleCancel(cancelRun)
		done := make(chan error, 1)
		go func() { done <- client.RunContext(runCtx) }()
		err := r.consume(runCtx, client, done)
		r.setCycleCancel(nil)
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
		backoff = r.nextBackoff(backoff, r.connectCount.Load() > connsBefore)
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
		case <-r.kick:
			// Aggressive reconnect (liveness alarm): skip the wait.
		}
	}
}

// nextBackoff advances the outer ladder after one client cycle. Back to
// the floor when the cycle achieved a connection (the failure is fresh)
// or when the liveness alarm demanded aggressive reconnects (positive
// evidence messages are being missed — gp-bsk); otherwise doubled and
// capped: a whole cycle that never connected means RunContext exhausted
// its own retries (typically a fatal auth verdict), and we keep
// retrying — a rotated token appears on restart, and giving up is the
// silent failure this transport exists to prevent — but back off so a
// revoked token doesn't hammer apps.connections.open.
func (r *socketModeRunner) nextBackoff(prev time.Duration, cycleConnected bool) time.Duration {
	if r.aggressive.Swap(false) || cycleConnected {
		return socketReconnectBackoffMin
	}
	return min(prev*2, socketReconnectBackoffMax)
}

func (r *socketModeRunner) setCycleCancel(cancel context.CancelFunc) {
	r.cancelMu.Lock()
	r.cycleCancel = cancel
	r.cancelMu.Unlock()
}

// killCycle cancels the in-flight client cycle, forcing RunContext to
// return so run() rebuilds the client — the only way to interrupt
// slack-go's internal retry loop once it is parked in a long backoff
// sleep. No-op when no cycle is live.
func (r *socketModeRunner) killCycle() {
	r.cancelMu.Lock()
	cancel := r.cycleCancel
	r.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// onInboundAlarm is the liveness watchdog's escalation hook (gp-bsk):
// an alarm is positive evidence that messages are sitting in channel
// history undelivered, so a patient backoff is exactly wrong. If the
// socket is down, reset the outer ladder to the floor, abandon any
// backoff sleep in progress, and kill the in-flight cycle (which may be
// parked in slack-go's internal ladder — 2026-08-23 it was 1h49m into
// one when the alarm fired, and the alarm's log line was the only
// consumer). A connected socket means the stall is elsewhere (event
// subscriptions, channel membership) and a reconnect would not help.
// Nil-safe.
func (r *socketModeRunner) onInboundAlarm() {
	if r == nil || r.connected.Load() {
		return
	}
	log.Printf("socket mode: inbound-liveness alarm while disconnected — aggressive reconnect (backoff reset to %s)", socketReconnectBackoffMin)
	r.aggressive.Store(true)
	select {
	case r.kick <- struct{}{}:
	default:
	}
	r.killCycle()
}

// noteConnectionError applies the gp-bsk reconnect discipline to one
// connect failure reported by slack-go's internal retry loop:
//
//  1. Backoff cap — v0.29.0's internal ladder is effectively unbounded
//     (its Max applies only on overflow), so once the reported wait
//     passes socketReconnectBackoffMax the cycle is killed and the
//     capped outer ladder takes over.
//  2. DNS self-heal — socketDNSStreakForFreshResolve consecutive
//     not-found failures flip the runner (stickily) to the pure-Go
//     resolver and rebuild the client immediately.
//  3. Self-restart — a transport that once worked but has stayed dark
//     past cfg.socketSelfRestartAfter requests the process exit (via
//     main's orderly drain); the service supervisor restarts it and
//     spool replay + watermark backfill make the bounce loss-free.
//  4. Loopback latch (gp-keg) — a lookup reporting one of Go's fallback
//     nameservers means the pure-Go resolver lost /etc/resolv.conf and
//     will not recover on its own: re-parse the file here and pin the
//     resolver to a nameserver from it; if the signature still persists
//     cfg.dnsLoopbackExitAfter attempts with the pin in place, request
//     the same orderly exit so the restarted process parses the healthy
//     file. Not gated on everConnected: a process that starts latched
//     would otherwise stay dark for good.
func (r *socketModeRunner) noteConnectionError(err error, reportedBackoff time.Duration) {
	if r.failStreak.Add(1) == 1 {
		now := time.Now() // monotonic reading — see failStreakStart
		r.failStreakStart.Store(&now)
	}
	if isDNSNotFound(err) {
		if streak := r.dnsStreak.Add(1); int(streak) >= socketDNSStreakForFreshResolve && !r.freshResolve.Swap(true) {
			log.Printf("socket mode: %d consecutive DNS not-found failures — in-process resolver looks poisoned; switching to the pure-Go resolver and rebuilding the client", streak)
			r.killCycle()
		}
	} else {
		r.dnsStreak.Store(0)
	}
	if isLoopbackDNSServerError(err) {
		r.onLoopbackLatch(r.loopbackDNSStreak.Add(1), dnsServerOf(err))
	} else {
		r.loopbackDNSStreak.Store(0)
		r.pinnedLoopbackStreak.Store(0)
	}
	if reportedBackoff > socketReconnectBackoffMax {
		log.Printf("socket mode: internal reconnect backoff %s exceeds the %s ceiling — restarting the client loop instead of waiting it out", reportedBackoff, socketReconnectBackoffMax)
		r.killCycle()
	}
	r.maybeSelfRestart(time.Now())
}

// onLoopbackLatch handles one loopback-latch failure (gp-keg, see
// dns_latch.go): the streak-th consecutive lookup that reported Go's
// fallback nameserver `reported`. It re-parses resolv.conf right now and
// pins the pure-Go resolver to a usable nameserver from it — rotating
// across the file's candidates on repeated events, and rebuilding the
// client if it was still dialing with the process-default resolver (on
// this signature that IS Go's resolver; the flip just routes it through
// the Dial hook). The exit decision: only a signature that persists for
// cfg.dnsLoopbackExitAfter consecutive lookups made WITH a pin already in
// place (the failure that triggers the first pin does not count) requests
// the orderly exit — a fresh process then parses the healthy file the way
// net expects, and cannot loop (it reproduces the signature only if the
// file yields no nameserver, in which case there is no pin and no exit).
// Nothing to pin — unreadable, no nameserver line, or only Go's own
// fallback addresses — never exits: a fresh process would latch on the
// same file and restart-loop the adapter, taking outbound /publish down
// with it; every further attempt re-parses instead, so the pin applies
// the moment the file is usable, and an earlier pin is kept meanwhile.
// A zero knob disables the exit, never the pin.
func (r *socketModeRunner) onLoopbackLatch(streak int32, reported string) {
	withPin := int32(0)
	if r.pinnedNameserver() != "" {
		withPin = r.pinnedLoopbackStreak.Add(1)
	} else {
		r.pinnedLoopbackStreak.Store(0)
	}
	events := r.loopbackEvents.Add(1)
	servers, readErr := readResolvConfNameservers(r.resolvConfPath)
	usable := usableNameservers(servers)
	pinned := ""
	if len(usable) > 0 {
		pinned = usable[int(events-1)%len(usable)]
		r.dnsPin.Store(&pinned)
		if !r.freshResolve.Swap(true) {
			r.killCycle()
		}
	}
	effective := r.pinnedNameserver()
	kept := ""
	if pinned == "" && effective != "" {
		kept = fmt.Sprintf(" (keeping the earlier pin %s)", effective)
	}
	switch {
	case pinned != "":
		log.Printf("socket mode: DNS resolver latched on Go's fallback nameserver %s (failure %d; it lost %s mid-rewrite and will not re-read it) — pinning the pure-Go resolver to %s from a fresh parse of the file", reported, streak, r.resolvConfPath, pinned)
	case readErr != nil:
		log.Printf("socket mode: DNS resolver latched on Go's fallback nameserver %s (failure %d) — nothing to pin: %s unreadable (%v)%s; re-parsing on every attempt, no self-exit (a fresh process would latch on the same file)", reported, streak, r.resolvConfPath, readErr, kept)
	case len(servers) == 0:
		log.Printf("socket mode: DNS resolver latched on Go's fallback nameserver %s (failure %d) — nothing to pin: %s lists no nameserver%s; re-parsing on every attempt, no self-exit (a fresh process would latch on the same file)", reported, streak, r.resolvConfPath, kept)
	default:
		log.Printf("socket mode: lookups failing against %s (failure %d) — %s lists only Go's fallback addresses %v, so this is a configured local resolver that is down (or the latch); nothing to pin%s, no self-exit (a fresh process would read the same file)", reported, streak, r.resolvConfPath, servers, kept)
	}
	if limit := r.cfg.dnsLoopbackExitAfter; limit > 0 && effective != "" && int(withPin) >= limit {
		r.requestSelfRestart(fmt.Sprintf("DNS RESOLVER LATCHED — %d consecutive lookups failed against Go's fallback nameserver %s with the pure-Go resolver pinned to %s from %s (%d failures in all); a fresh process parses that file the way net expects", withPin, reported, effective, r.resolvConfPath, streak))
	}
}

// isDNSNotFound reports whether err is a DNS name-resolution failure
// ("lookup slack.com: no such host"). The typed check covers errors
// that keep their chain; the string check covers slack-go paths that
// flatten the error into text.
func isDNSNotFound(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return strings.Contains(err.Error(), "no such host")
}

// maybeSelfRestart requests the process exit when the socket transport
// has been failing continuously past cfg.socketSelfRestartAfter despite having
// once been connected — the 8/22+8/23 incident class where in-process
// state was poisoned and only a restart recovered. Gated on
// everConnected so a never-connecting misconfiguration (Socket Mode
// toggle off, bad app token) degrades health but does not restart-loop
// the adapter. The dark window is measured from the start of the
// current failure streak with MONOTONIC time arithmetic, so a laptop
// sleep or wall-clock jump cannot spend the budget (monotonic clocks
// pause across suspend) — only ≥socketSelfRestartAfter of awake,
// actively-failing time fires. The exit is loss-free: r.exit hands the
// request to main's orderly shutdown (buffered inbounds drain to gc or
// the durable spool — bare os.Exit would strand them below the
// admission-time watermark), the service supervisor restarts the
// adapter, and spool replay + startup recovery + watermark backfill
// replay anything missed — proven in both incidents. Fires once.
func (r *socketModeRunner) maybeSelfRestart(now time.Time) {
	after := r.cfg.socketSelfRestartAfter
	if after <= 0 || r.connected.Load() || !r.everConnected.Load() {
		return
	}
	if int(r.failStreak.Load()) < socketSelfRestartMinFailures {
		return
	}
	start := r.failStreakStart.Load()
	if start == nil {
		return
	}
	failingFor := now.Sub(*start) // monotonic when both carry readings
	if failingFor < after {
		return
	}
	r.requestSelfRestart(fmt.Sprintf("transport failing for %s (limit %s, down %s total) across %d consecutive connect failures",
		failingFor.Round(time.Second), after, now.Sub(r.downSince()).Round(time.Second), r.failStreak.Load()))
}

// requestSelfRestart asks main for the orderly exit (code 1) that has
// the service supervisor restart the adapter, logging one line that
// says why. Shared by the gp-bsk dark-window self-restart and the
// gp-keg loopback latch; fires once per process — later calls are
// no-ops while main's drain is on its way.
func (r *socketModeRunner) requestSelfRestart(why string) {
	if r.restartRequested.Swap(true) {
		return // already requested; main's drain is on its way
	}
	log.Printf("socket mode: SELF-RESTART — %s; requesting process exit (orderly drain first) so the service supervisor restarts the process (spool replay + startup recovery + watermark backfill make this loss-free)", why)
	r.liveness.saveState()
	r.exit(1)
}

// downSince is the start of the current disconnected period: the last
// disconnect timestamp, or process start if the transport has not yet
// connected.
func (r *socketModeRunner) downSince() time.Time {
	since := r.startedAt
	if ts := r.lastDisconnectAt.Load(); ts > 0 {
		if t := time.Unix(0, ts); t.After(since) {
			since = t
		}
	}
	return since
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
			r.noteConnectionError(ce.ErrorObj, ce.Backoff)
		} else {
			log.Printf("socket mode: connection error")
			r.noteConnectionError(nil, 0)
		}
	case socketmode.EventTypeInvalidAuth:
		r.connectErrors.Add(1)
		r.setLastErr("invalid_auth")
		log.Printf("socket mode: INVALID AUTH — SLACK_APP_TOKEN rejected by apps.connections.open (rotate the xapp- token; the adapter keeps retrying)")
	case socketmode.EventTypeConnected:
		n := r.connectCount.Add(1)
		r.connected.Store(true)
		r.everConnected.Store(true)
		r.dnsStreak.Store(0)
		r.loopbackDNSStreak.Store(0)
		r.pinnedLoopbackStreak.Store(0)
		r.dnsPin.Store(nil) // transient: re-derived on the next latch event
		r.failStreak.Store(0)
		r.failStreakStart.Store(nil)
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
	down := now.Sub(r.downSince())
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
	if r.freshResolve.Load() {
		b.WriteString(" socket_fresh_resolve=true")
	}
	if pin := r.pinnedNameserver(); pin != "" {
		fmt.Fprintf(&b, " socket_dns_pin=%s", pin)
	}
	if n := r.loopbackDNSStreak.Load(); n > 0 {
		fmt.Fprintf(&b, " socket_dns_loopback_streak=%d", n)
	}
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
