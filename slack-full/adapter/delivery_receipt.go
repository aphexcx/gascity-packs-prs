package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// errDeliveryUnvouched marks a delivery gc ACCEPTED but would not vouch
// for. Deliberately not an *inboundPostError: gc did not reject the
// payload, so permanentDeliveryFailure must not dead-letter it — this
// is a last-hop failure, and the recovery is a re-post, not a
// quarantine.
var errDeliveryUnvouched = errors.New("gc accepted the inbound but did not vouch that it reached the session (no delivery receipt)")

// --- gc delivery receipts (gp-32q) ------------------------------------------
//
// The same-ts claim in channel_claims.go decides whether a twin may SKIP
// its copy, and until gp-32q it concluded on "gc's inbound endpoint
// returned 2xx". That signal cannot see the last hop. gc's
// /extmsg/inbound handler runs the member notification in the
// background (internal/api/huma_handlers_extmsg.go: runBackground →
// extmsgNotifyInboundMembers) and answers 200 before the session is
// touched, so a payload that gc accepted whole can still reach the
// session mangled — twice on 8/27-28 the winning twin committed the
// claim while the session had received only a boilerplate tail
// (pc_2e2378b9918e), and the losing twin then skipped the copy that
// would have recovered it.
//
// A delivery receipt is gc vouching for the LAST hop: the complete
// payload was written into the session. The adapter treats the receipt
// as the only evidence that authorizes a twin to skip:
//
//   - vouched  → conclude the claim; a twin may skip its copy. gc
//     delivered the payload COMPLETE: the status says delivered and no
//     count, summary or per-member, falls short of what gc built.
//   - no route → conclude the claim; there was nothing to deliver and a
//     twin's re-post would find no route either.
//   - unconfirmed → gc says something did NOT land (failed, partial, a
//     member it names, or a "delivered" whose own byte counts fall
//     short): re-post once in place, and if that still does not vouch,
//     release the claim so a parked twin re-posts.
//   - held → gc has not finished trying (pending). Conclude the claim
//     and do NOT re-post. gc waits for the session to reach an idle
//     boundary before pasting (NudgeIdleTimeout, 30s here), longer than
//     any HTTP budget this adapter can hold, so pending is the NORMAL
//     outcome for a BUSY session — and busy sessions are exactly the
//     population this bead is about. Re-posting there would deliver
//     both copies of the very messages being fixed (mayor ruling,
//     2026-08-28).
//   - unsupported → this gc does not emit receipts at all. Fail open to
//     the pre-gp-32q behavior (conclude on 2xx) and say so in the log,
//     so a receipt-less gc is legible rather than silently ungated.
//
// The unsupported arm is what lets this pack ship and be pinned BEFORE
// the gc that emits receipts (gp-2rq): behavior is byte-identical to
// today until a receipt-emitting gc is running, at which point the gate
// engages on its own with no flag day.

// deliveryReceiptGateEnv names the kill switch. "off" restores the
// pre-gp-32q commit-on-2xx behavior wholesale — the escape hatch if a
// gc build ever reports receipts wrongly and the adapter starts
// re-posting deliveries that actually landed. Anything else (including
// unset) leaves the gate armed.
const deliveryReceiptGateEnv = "SLACK_DELIVERY_RECEIPT_GATE"

// deliveryReceiptBodyLimit bounds how much of gc's inbound response the
// adapter reads looking for the receipt. The body echoes the message
// and its transcript record, so it is not tiny; 1 MiB is far past any
// real one and keeps a pathological response from being buffered whole.
const deliveryReceiptBodyLimit = 1 << 20

// deliveryReceiptRepostAttempts is how many EXTRA times the owner of a
// claim re-posts a delivery its receipt would not vouch for. One: the
// re-post exists to recover a mangled hop, and a payload that fails
// twice is a standing condition (dead session, broken transport) that a
// third copy would not fix — past this the claim is released so a
// parked twin can try, and the failure is logged at LOSS grade.
const deliveryReceiptRepostAttempts = 1

// receiptRepostAllowed reports whether an unvouched delivery may spend
// a second gc round-trip re-posting in place.
//
// Never during the shutdown drain (codex r1 P2). Each POST can burn the
// full gcForwardClient timeout, so an owner that re-posts runs for up to
// twice that — past shutdownEventDrainTimeout, which is what shutdown
// waits before it drains, spools and SEALS. A re-post still in flight at
// the seal cannot spool its own failure and can be killed mid-claim by
// process exit. While draining, an unvouched delivery therefore goes
// straight to its failure path, whose drain arm spools the message for
// startup replay — durable recovery beats one more in-process attempt.
//
// This is a check, not a barrier, and the gap between it and the POST it
// guards is real (codex r3 P2 #5, sharpened in r5): a re-post whose
// check reads TRUE microseconds before the flip runs during the drain
// after all. Closing it would mean registering the whole
// re-post-through-conclusion scope with the shutdown transition — a new
// synchronization primitive spanning the drain flip, three call sites
// and the spool seal — for a window one scheduler hop wide against a
// signal that arrives once per process lifetime. Not taken here; what
// follows is the residual, stated plainly rather than argued away.
//
// The coalesced leg is genuinely covered: its re-post runs inside a
// coalescer delivery window (take -> endDelivery), and flushAll waits
// for c.inflight to reach zero on every pass, so shutdown cannot
// snapshot or seal underneath it.
//
// The urgent and injection legs are NOT fully covered. They run inside
// an event goroutine, and shutdown joins cfg.eventWG for
// shutdownEventDrainTimeout — but that join is bounded at 20s, the same
// order as the HTTP timeout the re-post can burn, and shutdown proceeds
// when it expires. These legs also call spillBatch DIRECTLY rather than
// entering the coalescer's closed gate, so a straggler that reaches the
// spool after the seal is REFUSED and logs the LOSS line. That is a
// real, if narrow, way to lose one message at shutdown, and it is the
// same straggler contract gp-9e7 already accepted for any slow event
// goroutine — this bead adds at most one more round-trip to the window,
// and only when the check loses its race with the flip.
func receiptRepostAllowed(cfg config) bool {
	return cfg.draining == nil || !cfg.draining.Load()
}

// deliveryReceiptMember is one recipient's line in a receipt. Empty
// status means "gc did not report per-member detail", which is not
// evidence of failure — only an explicitly non-delivered member is.
type deliveryReceiptMember struct {
	SessionID string
	Status    string
	// Delivered/Expected and their OK+unit companions carry the same
	// completeness evidence as the summary counts, per member. The
	// summary is a SUM across the notify fan-out, so a member that got
	// half its payload can hide inside a total that looks right; only
	// the per-member pair can catch it.
	Delivered     int
	DeliveredOK   bool
	DeliveredUnit string
	Expected      int
	ExpectedOK    bool
	ExpectedUnit  string
	// Error is gc's per-member context string. It is NOT a verdict: the
	// emitter puts "submit unconfirmed" here for transports with no busy
	// probe (hidden-attach, NudgePane), where that is the NORMAL outcome
	// of a complete delivery (gp-2rq contract, counter 3). Read for the
	// log line only — never for the vouch.
	Error string
	// Digest is the first 16 hex of SHA-256 over the exact payload gc
	// framed, offered so an adapter log line and gc's own
	// "nudge-receipt id=… digest=…" line name the same delivery during
	// an incident. Nothing gates on it.
	Digest string
}

// deliveryReceipt is gc's answer to "did the complete payload reach the
// session?", parsed leniently out of an inbound/session-message
// response. present=false means no receipt block at all (a gc that
// predates gp-2rq).
type deliveryReceipt struct {
	present bool
	id      string
	status  string
	// delivered/expected carry gc's counts, and deliveredOK/expectedOK
	// say whether each actually PARSED as a number. The pair matters:
	// a missing, null, or renamed count must never read as a delivered
	// zero, which would make a whole payload look truncated (codex r1
	// P1).
	delivered   int
	deliveredOK bool
	expected    int
	expectedOK  bool
	// deliveredUnit/expectedUnit record WHICH unit each count was
	// spelled in ("bytes", "chars", "runes", or "" for an unqualified
	// count). The verdict compares the pair only when both agree: gc
	// measures bytes at the tmux paste buffer while an earlier draft of
	// this contract said runes, and Slack payloads carry emoji
	// constantly — so a bytes-vs-chars compare is not off by a rounding
	// error, it is a different number for most real messages.
	deliveredUnit string
	expectedUnit  string
	digest        string
	members       []deliveryReceiptMember
}

// receiptVerdict is what the claim owner is allowed to conclude.
type receiptVerdict int

const (
	// receiptUnsupported: no usable receipt. Fail open — conclude the
	// claim exactly as the adapter did before gp-32q.
	receiptUnsupported receiptVerdict = iota
	// receiptVouched: gc vouches the complete payload reached the
	// session. Conclude the claim; a twin may skip.
	receiptVouched
	// receiptNoRoute: gc had no session to deliver to. Conclude the
	// claim — a twin re-post would resolve the same empty route.
	receiptNoRoute
	// receiptUnconfirmed: gc says something did NOT land. Re-post, then
	// release.
	receiptUnconfirmed
	// receiptHeld: gc has not finished trying. Conclude the claim and do
	// NOT re-post (mayor ruling, 2026-08-28). gc waits for the session
	// to reach an idle boundary before pasting — NudgeIdleTimeout, 30s
	// on this city — which is longer than any HTTP budget the adapter
	// can hold, so a BUSY session's receipt comes back "pending" as its
	// normal outcome. Busy sessions are exactly the population this bead
	// is about: the truncation only manifests on a busy TUI. Re-posting
	// on pending would therefore fire precisely at the messages being
	// fixed and deliver both copies — gc never cancelled the first, it
	// just could not report on it yet. Truncation at least looks wrong
	// when it is caught; a duplicate reads as the founder having said
	// something twice, and gets acted on twice.
	receiptHeld
)

// parseDeliveryReceipt pulls the receipt out of a gc JSON response body.
//
// The wire shape is agreed with the emitter (gp-2rq, confirmed
// 2026-08-28):
//
//	"delivery": {
//	  "receipt_id": "rcpt_…",
//	  "status": "delivered" | "no_route" | "partial" | "failed" | "pending",
//	  "delivered_bytes": 867,     // summed over the fan-out
//	  "expected_bytes":  867,     // ditto — both measured by gc
//	  "digest": "d6d50cab6a19385b",
//	  "members": [{"session_id": …, "status": …, "delivered_bytes": …,
//	               "expected_bytes": …, "digest": …, "error": …}]
//	}
//
// The rest of the body is PascalCase — gc's InboundResult carries no
// json tags and only this block was given one — which the
// spelling-insensitive lookups below already absorb.
//
// The per-member counts are what make the receipt attest COMPLETENESS
// rather than arrival — the property the 05:39Z reproduction proved is
// the one that matters (223 bytes sent, 134 received, 89 eaten off the
// head, reading as a complete sentence that started mid-thought). The
// summary counts are SUMS across the fan-out, so a member that got half
// its payload can hide inside a total that adds up; only the per-member
// pair catches that.
//
// Three more things about that contract are load-bearing here. The block is
// ABSENT — never present-with-empty-fields — on a gc that cannot vouch,
// which is what the unsupported arm keys on. The counts are BYTES
// measured after the send path and summed across members, because one
// inbound notifies every transcript member with a per-member reminder,
// so there is no single expected value to compare against anything the
// adapter computed. And "delivered" means the complete payload was
// handed to the terminal inside ONE bracketed paste — not that the TUI
// rendered it. Submit-confirmation is deliberately NOT in the status:
// transports with no busy probe report it unconfirmed on every healthy
// delivery, so gating on it would re-post those forever. It rides in
// members[].error, which this parser reads for the log line only.
//
// Deliberately lenient about spelling. The producer is a different
// repository on a different release cadence (gascity gp-2rq), and gc's
// InboundResult carries no json tags today — so its fields serialize
// Go-style ("TargetSessionID") while a hand-written block would be
// snake_case. Keys are matched after lowercasing and stripping
// separators, so "delivery"/"Delivery", "receipt_id"/"ReceiptID" and
// "delivered_chars"/"DeliveredChars" all land. A body that is not JSON,
// or carries no delivery block, yields the zero receipt (present=false)
// and no error: absence is a legitimate answer meaning "old gc".
func parseDeliveryReceipt(body []byte) deliveryReceipt {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return deliveryReceipt{}
	}
	raw, ok := lookupNormalized(top, "delivery")
	if !ok {
		return deliveryReceipt{}
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return deliveryReceipt{}
	}
	block := normalizeReceiptMap(decoded)
	r := deliveryReceipt{present: true}
	r.id = receiptString(block, "receiptid", "id")
	r.status = strings.ToLower(strings.TrimSpace(receiptString(block, "status", "state")))
	r.digest = receiptString(block, "digest", "payloaddigest")
	r.delivered, r.deliveredUnit, r.deliveredOK = receiptCount(block, "delivered")
	r.expected, r.expectedUnit, r.expectedOK = receiptCount(block, "expected")
	r.members = receiptMembers(block)
	return r
}

// verdict classifies a receipt. gated=false forces receiptUnsupported —
// the kill switch and the pre-gate call sites share one code path so a
// disabled gate cannot diverge from the legacy behavior.
//
// The classification FAILS OPEN on anything it does not fully
// understand (codex r1 P1). The producer is a different repository on a
// different release cadence, so a renamed status, a renamed field, or a
// count that arrives as something other than a number is a schema-drift
// event — and reading drift as "not delivered" would re-post EVERY
// delivery in the workspace, an outage far worse than the mangling this
// gate exists to catch. Only an explicitly negative statement from gc
// withholds the vouch:
//
//   - a status this adapter knows to be negative, or
//   - a member gc explicitly reports as not delivered, or
//   - a known-positive status whose own counts contradict it.
//
// Everything else — unknown status, absent status, unparseable counts —
// degrades to receiptUnsupported, which is the pre-gp-32q behavior, and
// is named as such in the log field so the drift is visible rather than
// silent.
func (r deliveryReceipt) verdict(gated bool) receiptVerdict {
	if !gated || !r.present {
		return receiptUnsupported
	}
	// A block with nothing usable in it vouches for nothing AND accuses
	// nothing.
	if r.status == "" && !r.deliveredOK && !r.expectedOK && len(r.members) == 0 {
		return receiptUnsupported
	}
	// An explicitly non-delivered member is gc naming a recipient that
	// missed the payload, whatever the summary status says. So is a
	// member gc called delivered whose own counts fall short: the
	// receipt has to attest COMPLETENESS, not arrival (mayor ruling,
	// 2026-08-28, on the 05:39Z reproduction — a 223-byte founder
	// message reached the session with 89 bytes eaten off the head and
	// read as a complete sentence starting mid-thought).
	held := false
	for _, m := range r.members {
		switch {
		case receiptStatusNegative(m.Status):
			return receiptUnconfirmed
		case receiptStatusPositive(m.Status) && m.shortfall():
			return receiptUnconfirmed
		case receiptStatusPending(m.Status):
			held = true
		}
	}
	switch {
	case receiptStatusNegative(r.status):
		return receiptUnconfirmed
	case receiptStatusPending(r.status):
		return receiptHeld
	case receiptStatusNoRoute(r.status):
		return receiptNoRoute
	case receiptStatusPositive(r.status):
		// A member still waiting for its idle boundary HOLDS the whole
		// receipt, and it must be answered before the summary counts are
		// (codex r5 P1 #1). Its bytes are not in the delivered sum yet —
		// it has not been pasted — so the summary necessarily falls
		// short, and reading that shortfall as truncation would re-post
		// exactly the busy-session case the ruling exists to protect.
		// A member gc reported DELIVERED but short was already convicted
		// above, so a known-truncated member still outranks a pending
		// one.
		if held {
			return receiptHeld
		}
		// The incident shape: gc accepted 867 chars and the session read
		// a fragment. The counts are trusted to contradict the status
		// only when BOTH parsed as real numbers — a null, a bool, or a
		// renamed field must not read as "delivered 0 of 867" (codex r1
		// P1). Requiring a status this adapter knows also keeps the
		// comparison inside the contract that defines the unit: a
		// producer that shipped bytes against runes would otherwise make
		// every non-ASCII message look truncated.
		// The units must also MATCH. Both counts come from gc, so a
		// mixed pair means the producer drifted mid-schema — and
		// comparing delivered_bytes against expected_chars on an
		// emoji-carrying Slack message reads as truncation on a
		// delivery that was whole (or, in the other direction, hides a
		// real one). Fail open on the mismatch and let the status
		// stand.
		if r.shortfall() {
			return receiptUnconfirmed
		}
		return receiptVouched
	default:
		// A status this adapter has never heard of. Not a vouch — but
		// not an accusation either. Fail open.
		return receiptUnsupported
	}
}

// shortfall reports whether the counts contradict a positive status:
// gc says it delivered, and its own numbers say part of the payload did
// not go in. This is the incident shape the bead exists for, and after
// the 05:39Z reproduction it is the property the receipt is FOR — "it
// arrived" is not what has to be true, "all of it arrived" is.
//
// Trusted only when both counts parsed as real numbers in the SAME
// unit. A null, a bool, a renamed field or a bytes-vs-chars pair must
// never read as a truncation on a delivery that was whole.
func (r deliveryReceipt) shortfall() bool {
	return r.deliveredOK && r.expectedOK &&
		r.deliveredUnit == r.expectedUnit &&
		r.expected > 0 && r.delivered < r.expected
}

// shortfall is the same test for one member's own counts.
func (m deliveryReceiptMember) shortfall() bool {
	return m.DeliveredOK && m.ExpectedOK &&
		m.DeliveredUnit == m.ExpectedUnit &&
		m.Expected > 0 && m.Delivered < m.Expected
}

// normalizeReceiptStatus folds a status value the way keys are folded,
// so no_route / no-route / noRoute / "No Route" all compare equal.
func normalizeReceiptStatus(s string) string {
	return normalizeReceiptKey(strings.TrimSpace(s))
}

// The three functions below are the ONLY places a status value carries
// meaning, and between them they recognize exactly the confirmed enum —
// delivered | no_route | partial | failed | pending — and nothing else
// (codex r3 P2 #6). Words that merely sound like failures ("timeout",
// "error", "rejected") are NOT statements this gc makes, and admitting
// them would let a producer's schema drift trigger re-posts and
// dead-letters across the workspace. Drift belongs in the fail-open
// arm. Comparison is separator- and case-insensitive, so "No-Route" and
// "Delivered" still land.

// receiptStatusPositive names the status meaning "the complete payload
// reached the session".
func receiptStatusPositive(status string) bool {
	return normalizeReceiptStatus(status) == "delivered"
}

// receiptStatusNegative names the statuses meaning "it did not". This
// is the ONLY set that can withhold a claim's conclusion, so it stays a
// closed set of statements gc actually makes — never a catch-all for
// values this adapter does not recognize.
//
// "pending" is deliberately NOT here (mayor ruling, 2026-08-28). failed
// and partial mean gc KNOWS something did not land, which makes a
// re-post clean; pending means gc does not know yet and is still
// trying, which makes a re-post a duplicate.
func receiptStatusNegative(status string) bool {
	switch normalizeReceiptStatus(status) {
	case "partial", "failed":
		return true
	}
	return false
}

// receiptStatusPending names the status meaning "gc has not finished
// trying". It is NOT in the negative set: it is a HOLD, never a retry
// signal. See receiptHeld for why re-posting here would be worse than
// the bug.
func receiptStatusPending(status string) bool {
	return normalizeReceiptStatus(status) == "pending"
}

// receiptStatusNoRoute names the statuses meaning "there was nobody to
// deliver to" — concluded, because a twin's re-post resolves the same
// empty route.
func receiptStatusNoRoute(status string) bool {
	return normalizeReceiptStatus(status) == "noroute"
}

// logField renders the receipt for the inbound log line. Always a
// single token=value run so the existing `posted=NNch` greps keep
// working and an incident can be split adapter-vs-transport at a
// glance — which is what the posted= field was added for (gp-0qw).
func (r deliveryReceipt) logField(v receiptVerdict) string {
	if !r.present {
		return "receipt=unsupported"
	}
	// Every value below is written by another process; none of it may
	// forge or flood a line (codex r3 P2 #8).
	id := sanitizeReceiptLogValue(r.id, receiptLogValueLimit)
	if r.id == "" {
		id = "-"
	}
	status := sanitizeReceiptLogValue(r.status, receiptLogValueLimit)
	if r.status == "" {
		status = "empty"
	}
	out := fmt.Sprintf("receipt=%s receipt_status=%s receipt_verdict=%s", id, status, v)
	if r.digest != "" {
		// Correlates this line with gc's own "nudge-receipt id=…
		// digest=…" line for the same delivery (gp-2rq contract).
		out += " receipt_digest=" + sanitizeReceiptLogValue(r.digest, receiptLogValueLimit)
	}
	if r.deliveredOK || r.expectedOK {
		unit := r.deliveredUnit
		if unit == "" {
			unit = r.expectedUnit
		}
		if unit == "" {
			unit = "units"
		}
		out += fmt.Sprintf(" receipt_%s=%d/%d", unit, r.delivered, r.expected)
	}
	// Only the members gc named as undelivered, and at most a handful of
	// them: a fan-out room can carry dozens, and one unbounded log line
	// per failed delivery is how a log stops being readable in the
	// incident it exists for.
	shown := 0
	for _, m := range r.members {
		if !receiptStatusNegative(m.Status) && !m.shortfall() {
			continue
		}
		if shown == receiptLogMemberLimit {
			out += fmt.Sprintf(" receipt_members_undelivered_more=%d", countUndeliveredMembers(r.members)-shown)
			break
		}
		out += fmt.Sprintf(" receipt_member_undelivered=%s/%s",
			sanitizeReceiptLogValue(m.SessionID, receiptLogValueLimit),
			sanitizeReceiptLogValue(m.Status, receiptLogValueLimit))
		if m.shortfall() {
			out += fmt.Sprintf(" receipt_member_%s=%d/%d", m.DeliveredUnit, m.Delivered, m.Expected)
		}
		if m.Error != "" {
			out += " receipt_member_error=" + strconv.Quote(sanitizeReceiptLogValue(m.Error, receiptLogErrorLimit))
		}
		shown++
	}
	return out
}

func (v receiptVerdict) String() string {
	switch v {
	case receiptVouched:
		return "vouched"
	case receiptNoRoute:
		return "no_route"
	case receiptUnconfirmed:
		return "unconfirmed"
	case receiptHeld:
		return "held"
	default:
		return "unsupported"
	}
}

// --- lenient key/value helpers ----------------------------------------------

// normalizeReceiptKey folds a JSON key to its comparison form: lower
// case with separators removed, so ReceiptID / receipt_id / receipt-id
// all compare equal.
func normalizeReceiptKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// receiptLogValueLimit / receiptLogErrorLimit / receiptLogMemberLimit
// bound what a gc-supplied string can do to one log line. The values
// come from another process and, on the error field, ultimately from a
// terminal — so they are truncated, control characters are stripped,
// and only a few members are named per line.
const (
	receiptLogValueLimit  = 64
	receiptLogErrorLimit  = 120
	receiptLogMemberLimit = 4
)

// sanitizeReceiptLogValue makes a gc-supplied string safe to splat into
// a space-separated log line: control characters (a newline would forge
// a second log line) become "?", and the result is truncated with an
// explicit ellipsis so a truncated value cannot be mistaken for a
// complete one.
func sanitizeReceiptLogValue(s string, limit int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == limit {
			b.WriteString("…")
			break
		}
		switch {
		case unicode.IsSpace(r):
			// Covers the ASCII run and the Unicode ones (NBSP, the line
			// and paragraph separators) in a single arm, so nothing
			// space-like survives to split a token or a line.
			b.WriteByte('_')
		case !unicode.IsPrint(r):
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
		n++
	}
	if n == 0 {
		return "-"
	}
	return b.String()
}

func countUndeliveredMembers(members []deliveryReceiptMember) int {
	n := 0
	for _, m := range members {
		if receiptStatusNegative(m.Status) || m.shortfall() {
			n++
		}
	}
	return n
}

// receiptCountUnits are the unit suffixes a count may be spelled with,
// in the order they are preferred when a block carries more than one.
// The empty entry matches an unqualified "delivered"/"expected".
var receiptCountUnits = []string{"bytes", "chars", "runes", ""}

// receiptCount reads one count and reports the unit it was spelled in,
// so the verdict can refuse to compare a bytes count against a chars
// count. Units are tried in a FIXED order rather than by scanning the
// map, so a block carrying two spellings resolves the same way on every
// call instead of following Go's randomized map iteration.
func receiptCount(m map[string]any, prefix string) (int, string, bool) {
	for _, unit := range receiptCountUnits {
		if v, ok := receiptInt(m, prefix+unit); ok {
			return v, unit, true
		}
	}
	return 0, "", false
}

// lookupNormalized finds the one key normalizing to want. Two keys that
// both normalize to it (a body carrying "delivery" AND "Delivery")
// leave no basis for choosing, so the block reads as absent — the
// fail-open arm — rather than as whichever one map order surfaced.
func lookupNormalized(m map[string]json.RawMessage, want string) (json.RawMessage, bool) {
	var found json.RawMessage
	seen := false
	for k, v := range m {
		if normalizeReceiptKey(k) != want {
			continue
		}
		if seen {
			return nil, false
		}
		found, seen = v, true
	}
	return found, seen
}

// normalizeReceiptMap re-keys a decoded block by normalized name, and
// DROPS any name two distinct keys normalize to (codex r3 P2 #8).
// {"status":"delivered","Status":"failed"} carries two contradictory
// answers to one question and no basis for choosing between them —
// resolving it by map order would make the same body parse differently
// on two calls. Dropping the name makes it absent, which is the
// fail-open arm, and the collision is a producer bug the gate must not
// paper over in either direction.
func normalizeReceiptMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	collided := make(map[string]bool)
	for k, v := range m {
		nk := normalizeReceiptKey(k)
		if collided[nk] {
			continue
		}
		if _, dup := out[nk]; dup {
			delete(out, nk)
			collided[nk] = true
			continue
		}
		out[nk] = v
	}
	return out
}

// receiptAny resolves the first NAME that is present. Names are tried in
// the order given — a fixed precedence — against an already-normalized
// map, so resolution never depends on Go's randomized map iteration.
func receiptAny(m map[string]any, names ...string) (any, bool) {
	for _, want := range names {
		if v, ok := m[want]; ok {
			return v, true
		}
	}
	return nil, false
}

func receiptString(m map[string]any, names ...string) string {
	v, ok := receiptAny(m, names...)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// receiptInt reads a count that may arrive as a JSON number or, from a
// producer that stringifies its integers, as a decimal string. The
// second return says whether a NUMBER was actually found: absent, null,
// a bool, an object, or an unparseable string all report false so the
// caller can decline to reason about the value at all.
func receiptInt(m map[string]any, names ...string) (int, bool) {
	v, ok := receiptAny(m, names...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		// A count is a whole, non-negative number of units. A fraction,
		// a negative, a NaN or something past int range is not a count
		// this adapter can reason about, and letting one reach the
		// delivered < expected comparison would convict a delivery gc
		// never accused (codex r3 P2 #7).
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil || i < 0 || i > math.MaxInt32 {
			// Same ceiling as the number branch. Without it a
			// stringified count parses on a 64-bit build where the
			// identical JSON number is rejected, so the identical
			// receipt would classify two ways (codex r4 P2 #6).
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func receiptMembers(block map[string]any) []deliveryReceiptMember {
	v, ok := receiptAny(block, "members", "recipients")
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]deliveryReceiptMember, 0, len(list))
	for _, item := range list {
		decoded, ok := item.(map[string]any)
		if !ok {
			continue
		}
		mm := normalizeReceiptMap(decoded)
		delivered, deliveredUnit, deliveredOK := receiptCount(mm, "delivered")
		expected, expectedUnit, expectedOK := receiptCount(mm, "expected")
		out = append(out, deliveryReceiptMember{
			SessionID:     receiptString(mm, "sessionid", "session", "id"),
			Status:        receiptString(mm, "status", "state"),
			Delivered:     delivered,
			DeliveredOK:   deliveredOK,
			DeliveredUnit: deliveredUnit,
			Expected:      expected,
			ExpectedOK:    expectedOK,
			ExpectedUnit:  expectedUnit,
			Error:         receiptString(mm, "error", "err", "detail"),
			Digest:        receiptString(mm, "digest", "payloaddigest"),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
