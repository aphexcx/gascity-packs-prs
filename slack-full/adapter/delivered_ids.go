package main

import "sync"

// --- delivered-message-id tracking (gp-729 item 2) --------------------------
//
// The thread-context preamble (gc-px8.5/8.6) re-quotes a thread's prior
// messages on an agent's first visit — including the thread parent,
// which for channel-bound sessions was almost always already delivered
// as its own inbound moments earlier. This tracker remembers which
// message timestamps have actually been delivered per audience so the
// preamble can collapse already-seen priors to a one-line note and keep
// the full quote only for messages the audience genuinely hasn't seen.
//
// Audience keys mirror threadContextCache: (target, channel), where
// target "" is the channel-bound session(s) receiving postInbound
// deliveries and a non-empty target is a handle receiving alias
// dispatches. Lookups use the exact key only — a handle's alias-
// dispatched session may not be channel-bound, so channel-key hits must
// never suppress context for it. The conservative failure direction is
// always the full quote.
//
// In-memory like peerContextBuffer and threadContextCache: a restart
// forgets delivery history and the next preamble re-quotes in full — a
// one-time duplicate, not context loss.
//
// Known limitation: keys name audiences (handle, channel), not session
// identities. Rebinding a channel or reassigning a handle to a NEW
// session mid-lifetime leaves the old audience history in place, so
// the new session may see collapse lines for messages it never
// received until the thread ages out or the adapter restarts (`gc
// slack read` recovers the originals). Binding changes are rare
// operator actions; restart the adapter after one to reset history.

// deliveredIDsPerKeyCap bounds one audience's remembered timestamps.
// FIFO eviction: a busy channel forgets its oldest ids first, which
// only re-enables full quoting for ancient threads.
const deliveredIDsPerKeyCap = 2048

// deliveredIDsMaxKeys bounds the number of tracked audiences, evicting
// the least-recently-recorded audience wholesale on overflow.
const deliveredIDsMaxKeys = 1024

type deliveredIDSet struct {
	order []string
	set   map[string]bool
}

// deliveredIDs tracks delivered provider-message timestamps per
// (target, channel) audience. Nil-safe: nil record is a no-op and nil
// seen reports false (never delivered → full quote).
type deliveredIDs struct {
	mu       sync.Mutex
	perKey   map[string]*deliveredIDSet
	keyOrder []string
}

func newDeliveredIDs() *deliveredIDs {
	return &deliveredIDs{perKey: make(map[string]*deliveredIDSet)}
}

func deliveredIDsKey(target, channel string) string {
	return target + "|" + channel
}

// record remembers each ts as delivered to the (target, channel)
// audience.
func (d *deliveredIDs) record(target, channel string, tss ...string) {
	if d == nil || channel == "" || len(tss) == 0 {
		return
	}
	key := deliveredIDsKey(target, channel)
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.perKey[key]
	if !ok {
		if len(d.keyOrder) >= deliveredIDsMaxKeys {
			oldest := d.keyOrder[0]
			d.keyOrder = d.keyOrder[1:]
			delete(d.perKey, oldest)
		}
		s = &deliveredIDSet{set: make(map[string]bool)}
		d.perKey[key] = s
		d.keyOrder = append(d.keyOrder, key)
	}
	for _, ts := range tss {
		if ts == "" || s.set[ts] {
			continue
		}
		if len(s.order) >= deliveredIDsPerKeyCap {
			drop := s.order[0]
			s.order = s.order[1:]
			delete(s.set, drop)
		}
		s.set[ts] = true
		s.order = append(s.order, ts)
	}
}

// seen reports whether ts was recorded as delivered to the exact
// (target, channel) audience.
func (d *deliveredIDs) seen(target, channel, ts string) bool {
	if d == nil || channel == "" || ts == "" {
		return false
	}
	key := deliveredIDsKey(target, channel)
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.perKey[key]
	return ok && s.set[ts]
}
