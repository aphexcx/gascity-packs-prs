package main

import (
	"bytes"
	"context"
	"net/http"
	"sync"
)

// Trusted-transport plumbing shared by the Socket Mode runner and the
// inbound-liveness backfill (gp-3og).
//
// Both deliver Slack envelopes that did NOT arrive over the public
// listener: Socket Mode envelopes come down an app-level-token
// WebSocket (the token IS the authentication — Slack signs nothing),
// and backfill envelopes are synthesized in-process from
// conversations.history reads made with the bot token. Rather than
// fork the event/interaction pipelines, both sources build an
// in-memory *http.Request carrying the trusted marker below and run it
// through the SAME handlers the Events API uses — every routing,
// dedup, company-admission, and dispatch rule stays byte-for-byte
// identical; only the HMAC check is skipped.
//
// The marker lives in the request context, which an over-the-wire
// request can never populate, so there is no header to forge: a
// network client that omits X-Slack-Signature still gets its 401.

// trustedTransportKey is the context key for the trusted-transport
// marker. Unexported struct type so no other package can collide.
type trustedTransportKey struct{}

// trustedTransportName is the value stored under trustedTransportKey:
// a short label ("socket_mode", "backfill") used only for logging.
func withTrustedTransport(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, trustedTransportKey{}, name)
}

// isTrustedTransportRequest reports whether r was built in-process by a
// trusted transport. Nil-safe.
func isTrustedTransportRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	v, _ := r.Context().Value(trustedTransportKey{}).(string)
	return v != ""
}

// trustedTransportName returns the marker label, or "" for a network
// request.
func trustedTransportName(r *http.Request) string {
	if r == nil {
		return ""
	}
	v, _ := r.Context().Value(trustedTransportKey{}).(string)
	return v
}

// backfillVerdict is how the inbound-liveness backfill learns what became
// of the envelope it ran through the events handler (gf-xid finding 3g).
// The handler's own answer is the 200 Slack gets before the forward to gc
// runs: the dispatch continues in its own goroutine, and the forward's
// outcome is recorded on the event's dedup claim — committed when the event
// was delivered to gc or durably queued with retry ownership, forgotten when
// the forward failed and a redelivery must take over. The handler attaches
// that claim here (watch) the moment it takes it, synchronously, before its
// 200; the backfill then waits on it (settle) instead of trusting the
// status. An envelope the handler answered without taking a claim — a
// challenge, a company-gateway admission, a drop — leaves the verdict
// unwatched, and the status stays the whole answer.
type backfillVerdict struct {
	mu        sync.Mutex
	done      <-chan struct{}
	committed func() bool
}

// backfillVerdictKey is the context key under which deliverViaHandler hands
// the verdict to the handler. Unexported struct type; an over-the-wire
// request can never carry one.
type backfillVerdictKey struct{}

func withBackfillVerdict(ctx context.Context, v *backfillVerdict) context.Context {
	return context.WithValue(ctx, backfillVerdictKey{}, v)
}

// backfillVerdictFrom returns the verdict a backfill request carries, or nil
// for every other request.
func backfillVerdictFrom(r *http.Request) *backfillVerdict {
	if r == nil {
		return nil
	}
	v, _ := r.Context().Value(backfillVerdictKey{}).(*backfillVerdict)
	return v
}

// watch binds the verdict to id's claim in cache. A claim the cache does not
// track (nil cache, no entry) leaves the verdict unwatched. Nil-safe.
func (v *backfillVerdict) watch(cache *eventDedupCache, id string) {
	if v == nil {
		return
	}
	done, ok := cache.await(id)
	if !ok {
		return
	}
	v.mu.Lock()
	v.done = done
	v.committed = func() bool { return cache.committed(id) }
	v.mu.Unlock()
}

// settle waits for the watched claim to conclude or ctx to end. watched is
// false when no claim was attached (the status is final); settled is false
// when ctx ended before the claim concluded; committed is the claim's
// verdict once settled. Nil-safe: a nil verdict is unwatched.
func (v *backfillVerdict) settle(ctx context.Context) (committed, settled, watched bool) {
	if v == nil {
		return false, true, false
	}
	v.mu.Lock()
	done, isCommitted := v.done, v.committed
	v.mu.Unlock()
	if done == nil {
		return false, true, false
	}
	select {
	case <-done:
		return isCommitted(), true, true
	case <-ctx.Done():
		return false, false, true
	}
}

// backfillDelivery is what the backfill's deliver hook answers: the handler's
// immediate status, and the verdict of the dispatch it accepted (nil from a
// hook that runs no handler, such as a test recorder: the status is final).
type backfillDelivery struct {
	status  int
	verdict *backfillVerdict
}

// accepted reports whether the handler took the envelope at all.
func (d backfillDelivery) accepted() bool {
	return d.status >= 200 && d.status < 300
}

// memResponseWriter is the minimal in-memory http.ResponseWriter the
// trusted transports hand to the HTTP handlers. It records what the
// handler would have sent to Slack so the caller can translate it into
// a Socket Mode ack (status → ack/no-ack, body → ack payload). Not
// safe for concurrent use; each envelope gets its own.
type memResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newMemResponseWriter() *memResponseWriter {
	return &memResponseWriter{header: make(http.Header)}
}

func (w *memResponseWriter) Header() http.Header { return w.header }

func (w *memResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *memResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

// Status returns the recorded status, defaulting to 200 when the
// handler returned without writing anything (net/http does the same).
func (w *memResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Body returns the bytes the handler wrote.
func (w *memResponseWriter) Body() []byte { return w.body.Bytes() }
