package main

import (
	"bytes"
	"context"
	"net/http"
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
