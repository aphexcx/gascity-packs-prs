package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for the delivery-receipt gate (gp-32q, pc_2e2378b9918e).
//
// Live shape being defended: 8/28 04:08Z, C0ASAPRETDK ts
// 1787890080.963799 — a bot-mention twin pair where the winning twin
// posted 867 chars, gc answered 2xx, the session received only the
// reply boilerplate tail, and the parked twin then logged "channel copy
// already delivered by same-ts twin — skipping channel post". The
// claim's verdict must come from gc vouching for the last hop, not from
// the 2xx.

// --- parser + verdict --------------------------------------------------------

func TestParseDeliveryReceipt_SnakeCaseBlock(t *testing.T) {
	body := []byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"rcpt_1","status":"delivered","delivered_chars":867,"expected_chars":867}}`)
	got := parseDeliveryReceipt(body)
	if !got.present {
		t.Fatal("receipt not detected in body carrying a delivery block")
	}
	if got.id != "rcpt_1" || got.status != "delivered" || got.delivered != 867 || got.expected != 867 {
		t.Fatalf("parsed receipt = %+v, want id=rcpt_1 status=delivered 867/867", got)
	}
	if v := got.verdict(true); v != receiptVouched {
		t.Errorf("verdict = %v, want vouched", v)
	}
}

// gc's InboundResult carries no json tags, so a field added to it
// serializes Go-style. The parser must not care which convention the
// core side ships — a spelling mismatch would silently disarm the gate.
func TestParseDeliveryReceipt_GoStyleKeys(t *testing.T) {
	body := []byte(`{"Delivery":{"ReceiptID":"rcpt_2","Status":"Delivered","DeliveredChars":867,"ExpectedChars":867}}`)
	got := parseDeliveryReceipt(body)
	if !got.present || got.id != "rcpt_2" || got.delivered != 867 {
		t.Fatalf("parsed receipt = %+v, want the Go-style block parsed", got)
	}
	if v := got.verdict(true); v != receiptVouched {
		t.Errorf("verdict = %v, want vouched (status match must be case-insensitive)", v)
	}
}

func TestParseDeliveryReceipt_StringifiedCounts(t *testing.T) {
	body := []byte(`{"delivery":{"receipt_id":"rcpt_3","status":"delivered","delivered_chars":"12","expected_chars":"867"}}`)
	got := parseDeliveryReceipt(body)
	if got.delivered != 12 || got.expected != 867 {
		t.Fatalf("parsed counts = %d/%d, want 12/867", got.delivered, got.expected)
	}
	if v := got.verdict(true); v != receiptUnconfirmed {
		t.Errorf("verdict = %v, want unconfirmed (delivered < expected is the incident shape)", v)
	}
}

// A count that arrives as null / a bool / an object must not read as
// "delivered 0", which would make a whole payload look truncated.
func TestParseDeliveryReceipt_UnparseableCountsAreNotZero(t *testing.T) {
	for name, body := range map[string]string{
		"null":   `{"delivery":{"receipt_id":"r","status":"delivered","delivered_chars":null,"expected_chars":867}}`,
		"bool":   `{"delivery":{"receipt_id":"r","status":"delivered","delivered_chars":true,"expected_chars":867}}`,
		"object": `{"delivery":{"receipt_id":"r","status":"delivered","delivered_chars":{"n":867},"expected_chars":867}}`,
		"words":  `{"delivery":{"receipt_id":"r","status":"delivered","delivered_chars":"all of them","expected_chars":867}}`,
	} {
		got := parseDeliveryReceipt([]byte(body))
		if got.deliveredOK {
			t.Errorf("%s: delivered count reported as parsed", name)
		}
		if v := got.verdict(true); v != receiptVouched {
			t.Errorf("%s: verdict = %v, want vouched (an unparseable count is not evidence of truncation)", name, v)
		}
	}
}

func TestParseDeliveryReceipt_AbsentAndUnparseable(t *testing.T) {
	for name, body := range map[string]string{
		"no block":      `{"TargetSessionID":"sess-1"}`,
		"not json":      `accepted`,
		"empty body":    ``,
		"null delivery": `{"delivery":null}`,
	} {
		got := parseDeliveryReceipt([]byte(body))
		if got.present {
			t.Errorf("%s: receipt reported present, want absent", name)
		}
		if v := got.verdict(true); v != receiptUnsupported {
			t.Errorf("%s: verdict = %v, want unsupported (fail open to the pre-gp-32q path)", name, v)
		}
	}
}

func TestDeliveryReceiptVerdict(t *testing.T) {
	cases := []struct {
		name    string
		receipt deliveryReceipt
		gated   bool
		want    receiptVerdict
	}{
		{"gate disarmed ignores a failing receipt",
			deliveryReceipt{present: true, status: "failed"}, false, receiptUnsupported},
		{"empty block vouches for nothing and accuses nothing",
			deliveryReceipt{present: true}, true, receiptUnsupported},
		{"no route concludes the claim",
			deliveryReceipt{present: true, id: "r", status: "no_route"}, true, receiptNoRoute},
		{"delivered with matching counts",
			deliveryReceipt{present: true, id: "r", status: "delivered",
				delivered: 867, deliveredOK: true, expected: 867, expectedOK: true}, true, receiptVouched},
		{"delivered with no counts at all",
			deliveryReceipt{present: true, id: "r", status: "delivered"}, true, receiptVouched},
		{"truncated payload is not a delivery",
			deliveryReceipt{present: true, id: "r", status: "delivered",
				delivered: 41, deliveredOK: true, expected: 867, expectedOK: true}, true, receiptUnconfirmed},
		{"delivered overall but a member missed it",
			deliveryReceipt{present: true, id: "r", status: "delivered", members: []deliveryReceiptMember{
				{SessionID: "s1", Status: "delivered"}, {SessionID: "s2", Status: "failed"}}}, true, receiptUnconfirmed},
		{"pending is a hold, never a retry signal",
			deliveryReceipt{present: true, id: "r", status: "pending"}, true, receiptHeld},
		{"a member still pending holds the whole receipt",
			deliveryReceipt{present: true, id: "r", status: "delivered", members: []deliveryReceiptMember{
				{SessionID: "s1", Status: "delivered"}, {SessionID: "s2", Status: "pending"}}}, true, receiptHeld},
		// A pending member has not been pasted yet, so its bytes are not
		// in the delivered sum — the summary NECESSARILY falls short
		// while it waits. Reading that as truncation would re-post the
		// busy-session case the hold exists to protect (codex r5 P1 #1).
		{"a pending member outranks a short summary",
			deliveryReceipt{present: true, id: "r", status: "delivered",
				delivered: 400, deliveredOK: true, deliveredUnit: "bytes",
				expected: 800, expectedOK: true, expectedUnit: "bytes",
				members: []deliveryReceiptMember{
					{SessionID: "s1", Status: "delivered"}, {SessionID: "s2", Status: "pending"}}}, true, receiptHeld},
		// But a member gc itself reports short is a statement that
		// something did not land, and outranks the hold.
		{"a truncated member outranks a pending one",
			deliveryReceipt{present: true, id: "r", status: "delivered",
				members: []deliveryReceiptMember{
					{SessionID: "s1", Status: "delivered", Delivered: 134, DeliveredOK: true, DeliveredUnit: "bytes",
						Expected: 223, ExpectedOK: true, ExpectedUnit: "bytes"},
					{SessionID: "s2", Status: "pending"}}}, true, receiptUnconfirmed},
		{"a failed member outranks a pending one",
			deliveryReceipt{present: true, id: "r", status: "delivered", members: []deliveryReceiptMember{
				{SessionID: "s1", Status: "pending"}, {SessionID: "s2", Status: "failed"}}}, true, receiptUnconfirmed},
		{"partial is not a vouch",
			deliveryReceipt{present: true, id: "r", status: "partial"}, true, receiptUnconfirmed},
		// Fail-open arms (codex r1 P1). Reading schema drift as "not
		// delivered" would re-post every delivery in the workspace — an
		// outage strictly worse than the mangling this gate catches — so
		// only statements this adapter recognizes may withhold a vouch.
		{"a status this adapter has never heard of falls back to legacy",
			deliveryReceipt{present: true, id: "r", status: "quantum"}, true, receiptUnsupported},
		{"a renamed status field leaves the status empty",
			deliveryReceipt{present: true, id: "r", delivered: 867, deliveredOK: true, expected: 867, expectedOK: true}, true, receiptUnsupported},
		{"unknown status must not be convicted by its own counts",
			deliveryReceipt{present: true, id: "r", status: "delivered_to_session",
				delivered: 41, deliveredOK: true, expected: 867, expectedOK: true}, true, receiptUnsupported},
		{"a count that did not parse is not a truncation",
			deliveryReceipt{present: true, id: "r", status: "delivered", expected: 867, expectedOK: true}, true, receiptVouched},
		{"a member status this adapter does not know is not an accusation",
			deliveryReceipt{present: true, id: "r", status: "delivered", members: []deliveryReceiptMember{
				{SessionID: "s1", Status: "queued_for_paste"}}}, true, receiptVouched},
		{"separator and case drift in a negative status still convicts",
			deliveryReceipt{present: true, id: "r", status: "No_Route"}, true, receiptNoRoute},
		{"case drift in a negative status still convicts",
			deliveryReceipt{present: true, id: "r", status: "PARTIAL"}, true, receiptUnconfirmed},
		// Only the confirmed enum (delivered | no_route | partial |
		// failed | pending) carries a verdict. A value that merely
		// SOUNDS like a failure is not a statement gc makes, and
		// admitting it would let schema drift re-post and dead-letter
		// across the workspace.
		{"a failure-sounding status outside the enum still fails open",
			deliveryReceipt{present: true, id: "r", status: "timeout"}, true, receiptUnsupported},
		{"a success-sounding status outside the enum does not vouch either",
			deliveryReceipt{present: true, id: "r", status: "ok"}, true, receiptUnsupported},
		{"a member status outside the enum is not an accusation",
			deliveryReceipt{present: true, id: "r", status: "delivered", members: []deliveryReceiptMember{
				{SessionID: "s1", Status: "rejected"}}}, true, receiptVouched},
		{"separator drift in no_route still concludes",
			deliveryReceipt{present: true, id: "r", status: "No-Route"}, true, receiptNoRoute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.receipt.verdict(tc.gated); got != tc.want {
				t.Errorf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// The log line is how an incident gets split adapter-vs-transport, so
// the receipt id must reach it (bead: "log the receipt id").
func TestDeliveryReceiptLogField(t *testing.T) {
	r := deliveryReceipt{present: true, id: "rcpt_9", status: "partial",
		delivered: 41, deliveredOK: true, deliveredUnit: "bytes",
		expected: 867, expectedOK: true, expectedUnit: "bytes"}
	got := r.logField(r.verdict(true))
	for _, want := range []string{"receipt=rcpt_9", "receipt_status=partial", "receipt_verdict=unconfirmed", "receipt_bytes=41/867"} {
		if !strings.Contains(got, want) {
			t.Errorf("log field %q missing %q", got, want)
		}
	}
	if got := (deliveryReceipt{}).logField(receiptUnsupported); got != "receipt=unsupported" {
		t.Errorf("absent-receipt log field = %q, want receipt=unsupported", got)
	}
}

// gp-2rq counter 1: gc measures BYTES at the tmux paste buffer, and an
// earlier draft of this contract said runes. Slack payloads carry emoji
// constantly, so a bytes-vs-chars compare is not a rounding error — it
// is a different number for most real messages, in whichever direction
// happens to be wrong. A mixed pair is schema drift, and drift fails
// open.
func TestDeliveryReceiptVerdict_MixedUnitsAreNotCompared(t *testing.T) {
	mixed := deliveryReceipt{present: true, id: "r", status: "delivered",
		delivered: 41, deliveredOK: true, deliveredUnit: "bytes",
		expected: 867, expectedOK: true, expectedUnit: "chars"}
	if got := mixed.verdict(true); got != receiptVouched {
		t.Errorf("mixed-unit counts verdict = %v, want vouched (a bytes/chars compare must not convict)", got)
	}
	matched := mixed
	matched.expectedUnit = "bytes"
	if got := matched.verdict(true); got != receiptUnconfirmed {
		t.Errorf("matched-unit counts verdict = %v, want unconfirmed (this is the incident shape)", got)
	}
}

// The confirmed wire contract (gp-2rq counters 1-2) is delivered_bytes /
// expected_bytes summed over the fan-out. Parsing it is what makes the
// gate engage at all once the receipt-emitting gc ships.
func TestParseDeliveryReceipt_ConfirmedByteContract(t *testing.T) {
	body := []byte(`{"delivery":{"receipt_id":"rcpt_b","status":"partial","delivered_bytes":41,"expected_bytes":867,` +
		`"digest":"d6d50cab6a19385b","members":[` +
		`{"session_id":"s1","status":"delivered","delivered_bytes":41,"digest":"d6d50cab6a19385b"},` +
		`{"session_id":"s2","status":"failed","delivered_bytes":0,"error":"pane gone"}]}}`)
	got := parseDeliveryReceipt(body)
	if got.deliveredUnit != "bytes" || got.expectedUnit != "bytes" {
		t.Fatalf("units = %q/%q, want bytes/bytes", got.deliveredUnit, got.expectedUnit)
	}
	if got.delivered != 41 || got.expected != 867 || got.digest != "d6d50cab6a19385b" {
		t.Fatalf("parsed receipt = %+v, want 41/867 with the payload digest", got)
	}
	if len(got.members) != 2 || got.members[1].Error != "pane gone" || got.members[0].Digest != "d6d50cab6a19385b" {
		t.Fatalf("parsed members = %+v, want the error and digest detail carried", got.members)
	}
	if v := got.verdict(true); v != receiptUnconfirmed {
		t.Errorf("verdict = %v, want unconfirmed", v)
	}
	line := got.logField(receiptUnconfirmed)
	for _, want := range []string{"receipt_bytes=41/867", "receipt_digest=d6d50cab6a19385b",
		"receipt_member_undelivered=s2/failed", `receipt_member_error="pane_gone"`} {
		if !strings.Contains(line, want) {
			t.Errorf("log field %q missing %q", line, want)
		}
	}
}

// gp-2rq counter 3: transports with no busy probe (hidden-attach,
// NudgePane) report "submit unconfirmed" as their NORMAL outcome, and it
// rides in members[].error. Convicting on a non-empty error would hand
// those paths a permanent false failure and re-post forever.
func TestDeliveryReceiptVerdict_MemberErrorOnDeliveredIsNotAnAccusation(t *testing.T) {
	r := deliveryReceipt{present: true, id: "r", status: "delivered", members: []deliveryReceiptMember{
		{SessionID: "s1", Status: "delivered", Error: "submit unconfirmed (no busy probe)"}}}
	if got := r.verdict(true); got != receiptVouched {
		t.Errorf("verdict = %v, want vouched (an error string on a delivered member is context, not a verdict)", got)
	}
}

// A block that spells a count two ways must resolve the same way on
// every call — Go randomizes map iteration, so a key-first scan would
// pick a different one run to run and make the gate nondeterministic.
func TestParseDeliveryReceipt_UnitPrecedenceIsStable(t *testing.T) {
	body := []byte(`{"delivery":{"receipt_id":"r","status":"delivered","delivered_bytes":867,"delivered_chars":41,` +
		`"expected_bytes":867,"expected_chars":41}}`)
	for i := 0; i < 50; i++ {
		got := parseDeliveryReceipt(body)
		if got.delivered != 867 || got.deliveredUnit != "bytes" || got.expected != 867 || got.expectedUnit != "bytes" {
			t.Fatalf("iteration %d parsed %d%s/%d%s, want the bytes spelling every time",
				i, got.delivered, got.deliveredUnit, got.expected, got.expectedUnit)
		}
	}
}

// The receipt is written by another process, and its error strings come
// from a terminal. A newline would forge a second log line, and a
// fan-out room can name dozens of members — one delivery must not be
// able to flood the log it exists to make readable.
func TestDeliveryReceiptLogField_HostileValuesAreBounded(t *testing.T) {
	members := []deliveryReceiptMember{{SessionID: "s0", Status: "failed",
		Error: "boom\ninbound: chan=C1 ts=1 forged line\r" + strings.Repeat("x", 400)}}
	for i := 1; i < 9; i++ {
		members = append(members, deliveryReceiptMember{
			SessionID: "s" + strconv.Itoa(i), Status: "failed", Error: "boom"})
	}
	r := deliveryReceipt{present: true, id: "rcpt_h", status: "partial", members: members}
	line := r.logField(receiptUnconfirmed)
	if strings.ContainsAny(line, "\n\r") {
		t.Errorf("log field carries a control character and can forge a log line: %q", line)
	}
	if n := strings.Count(line, "receipt_member_undelivered="); n != receiptLogMemberLimit {
		t.Errorf("named %d undelivered members, want the line capped at %d: %q", n, receiptLogMemberLimit, line)
	}
	if !strings.Contains(line, "receipt_members_undelivered_more=5") {
		t.Errorf("log field %q does not account for the members it elided", line)
	}
	if len(line) > 1024 {
		t.Errorf("log field grew to %d bytes on a hostile receipt: %q", len(line), line)
	}
}

// Two keys normalizing to one name carry two contradictory answers to
// one question. Resolving that by map order would make the same body
// parse differently on two calls, so the name reads as absent and the
// gate falls open.
func TestParseDeliveryReceipt_CollidingKeysFailOpen(t *testing.T) {
	for name, body := range map[string]string{
		"colliding status": `{"delivery":{"receipt_id":"r","status":"delivered","Status":"failed"}}`,
		"colliding block":  `{"delivery":{"status":"failed"},"Delivery":{"status":"delivered"}}`,
	} {
		for i := 0; i < 30; i++ {
			if v := parseDeliveryReceipt([]byte(body)).verdict(true); v != receiptUnsupported {
				t.Fatalf("%s: verdict = %v on iteration %d, want unsupported every time", name, v, i)
			}
		}
	}
	// A collision on a field nobody consulted must not disarm the rest
	// of the receipt.
	got := parseDeliveryReceipt([]byte(`{"delivery":{"status":"delivered","framing":"a","Framing":"b"}}`))
	if v := got.verdict(true); v != receiptVouched {
		t.Errorf("verdict = %v, want vouched (an unused colliding field is not the gate's business)", v)
	}
}

// A count is a whole, non-negative number of units. Anything else is not
// a count this adapter can reason about, and letting one reach the
// delivered < expected comparison would convict a delivery gc never
// accused.
func TestParseDeliveryReceipt_InvalidCountsAreNotCounts(t *testing.T) {
	for name, body := range map[string]string{
		"negative":    `{"delivery":{"status":"delivered","delivered_bytes":-1,"expected_bytes":867}}`,
		"fractional":  `{"delivery":{"status":"delivered","delivered_bytes":0.5,"expected_bytes":867}}`,
		"huge":        `{"delivery":{"status":"delivered","delivered_bytes":1e30,"expected_bytes":867}}`,
		"neg string":  `{"delivery":{"status":"delivered","delivered_bytes":"-4","expected_bytes":867}}`,
		"exponential": `{"delivery":{"status":"delivered","delivered_bytes":8.67e2,"expected_bytes":867}}`,
	} {
		got := parseDeliveryReceipt([]byte(body))
		if name == "exponential" {
			// 8.67e2 IS 867 — a whole number that happens to be spelled
			// with an exponent, and JSON has one number type.
			if !got.deliveredOK || got.delivered != 867 {
				t.Errorf("%s: parsed %d (ok=%t), want 867", name, got.delivered, got.deliveredOK)
			}
			continue
		}
		if got.deliveredOK {
			t.Errorf("%s: reported as a parsed count (%d)", name, got.delivered)
		}
		if v := got.verdict(true); v != receiptVouched {
			t.Errorf("%s: verdict = %v, want vouched — an unusable count is not evidence of truncation", name, v)
		}
	}
}

// THE MAYOR'S 05:39Z REPRODUCTION, 2026-08-28 (bead test vector).
// C0AP0KV9S9E ts=1787895576.823719: a 223-byte founder message reached
// the session as 134 bytes — 89 eaten off the HEAD, tail byte-exact,
// cut mid-word ("representation|s"). It read as a complete sentence
// starting mid-thought, which is the real severity: not visibly missing
// text, but plausible-looking corrupted text that gets acted on. A
// receipt that only attested ARRIVAL would have called this delivered.
func TestDeliveryReceipt_MayorReproduction_CompletenessNotArrival(t *testing.T) {
	const (
		sent     = 223
		received = 134
	)
	// What gc would say if the receipt attested arrival only.
	arrivalOnly := parseDeliveryReceipt([]byte(
		`{"delivery":{"receipt_id":"rcpt_0539","status":"delivered","members":[{"session_id":"gc__mayor","status":"delivered"}]}}`))
	if v := arrivalOnly.verdict(true); v != receiptVouched {
		t.Fatalf("arrival-only receipt verdict = %v, want vouched — this is the shape that must NOT be enough", v)
	}
	// What the confirmed contract says: gc's own counts convict it.
	complete := parseDeliveryReceipt([]byte(fmt.Sprintf(
		`{"delivery":{"receipt_id":"rcpt_0539","status":"delivered","delivered_bytes":%d,"expected_bytes":%d,`+
			`"members":[{"session_id":"gc__mayor","status":"delivered","delivered_bytes":%d,"expected_bytes":%d,`+
			`"digest":"d6d50cab6a19385b"}]}}`, received, sent, received, sent)))
	if v := complete.verdict(true); v != receiptUnconfirmed {
		t.Fatalf("verdict = %v, want unconfirmed — 89 bytes off the head of a founder message is not a delivery", v)
	}
	line := complete.logField(receiptUnconfirmed)
	for _, want := range []string{"receipt=rcpt_0539", "receipt_bytes=134/223", "receipt_member_undelivered=gc__mayor/delivered"} {
		if !strings.Contains(line, want) {
			t.Errorf("log field %q missing %q", line, want)
		}
	}
}

// The summary counts are SUMS across the notify fan-out, so a member
// that got half its payload can hide inside a total that adds up. Only
// the per-member pair catches it.
func TestDeliveryReceipt_PerMemberShortfallHidesInTheSum(t *testing.T) {
	// Two members, 400 expected each. One got 800 (its own payload plus
	// nothing) — contrived so the SUM matches exactly while a member
	// plainly fell short.
	got := parseDeliveryReceipt([]byte(
		`{"delivery":{"receipt_id":"r","status":"delivered","delivered_bytes":800,"expected_bytes":800,` +
			`"members":[{"session_id":"s1","status":"delivered","delivered_bytes":800,"expected_bytes":400},` +
			`{"session_id":"s2","status":"delivered","delivered_bytes":0,"expected_bytes":400}]}}`))
	if got.shortfall() {
		t.Fatal("the summary counts must agree — the point of this vector is that they do")
	}
	if v := got.verdict(true); v != receiptUnconfirmed {
		t.Errorf("verdict = %v, want unconfirmed (s2 received none of its 400 bytes)", v)
	}
}

// The ruling in force: pending must never spend a re-post. gc waits for
// the session to reach an idle boundary before pasting (NudgeIdleTimeout,
// 30s), so pending is the normal outcome for a BUSY session — exactly
// the population this bead is about. Re-posting there delivers both
// copies of the messages being fixed.
func TestDeliveryReceipt_PendingHoldsWithoutRepostingOrHandingOver(t *testing.T) {
	var posts int32
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"rcpt_p","status":"pending",` +
			`"members":[{"session_id":"gc__mayor","status":"pending","error":"waiting for idle boundary"}]}}`))
	}))
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> a message to a busy session"
	ts := "1787895576.823719"

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+strconv.Itoa(i+1), "C1", ts, "", text, true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	if n := atomic.LoadInt32(&posts); n != 1 {
		t.Errorf("gc saw %d POSTs for one ts, want exactly 1 — pending must neither re-post nor hand the claim to the twin", n)
	}
	if !cfg.deliveredIDs.seen("", "C1", ts) {
		t.Error("a held delivery did not conclude its claim; a later redelivery would post the message again")
	}
}

// The watermark rollback must REGRESS, never no-op, when a newer
// delivery moved the entry past the failed one. With two failing
// deliveries A then B, an exact-match rollback leaves the cache
// asserting that A's context arrived when neither landed — the one
// direction this cache must never fail in. Regressing under a newer
// entry can re-send context a successful newer delivery conveyed: one
// duplicated preamble, which is what an eviction already costs.
func TestThreadContextRollback_TwoFailuresCannotLeaveContextClaimed(t *testing.T) {
	c := newThreadContextCache()
	// P delivered earlier; A then B each advance and then fail.
	c.markDelivered("mayor", "C1", "T1", "100.000001") // P
	c.markDelivered("mayor", "C1", "T1", "100.000002") // A, prev = P
	c.markDelivered("mayor", "C1", "T1", "100.000003") // B, prev = A

	c.rollbackDelivered("mayor", "C1", "T1", "100.000002", "100.000001") // A fails first
	c.rollbackDelivered("mayor", "C1", "T1", "100.000003", "100.000002") // B fails second

	if got := c.lastDeliveredFor("mayor", "C1", "T1"); got != "100.000001" {
		t.Errorf("watermark = %q, want %q — a failed delivery must never be left claimed as delivered", got, "100.000001")
	}
}

func TestThreadContextRollback_NeverAdvancesAndStopsAtTheFloor(t *testing.T) {
	c := newThreadContextCache()
	c.markDelivered("mayor", "C1", "T1", "100.000002")

	// A rollback whose prev is NEWER than the current entry must not
	// move the watermark forward.
	c.rollbackDelivered("mayor", "C1", "T1", "100.000002", "100.000009")
	if got := c.lastDeliveredFor("mayor", "C1", "T1"); got != "100.000002" {
		t.Errorf("watermark = %q, want it unmoved — rollback may only regress", got)
	}
	// An entry already regressed past this attempt is left alone.
	c.rollbackDelivered("mayor", "C1", "T1", "100.000005", "100.000004")
	if got := c.lastDeliveredFor("mayor", "C1", "T1"); got != "100.000002" {
		t.Errorf("watermark = %q, want %q — a stale rollback must not resurrect a newer ts", got, "100.000002")
	}
	// With no predecessor the entry goes away entirely, so the next
	// visit rebuilds the full window.
	c.rollbackDelivered("mayor", "C1", "T1", "100.000002", "")
	if got := c.lastDeliveredFor("mayor", "C1", "T1"); got != "" {
		t.Errorf("watermark = %q, want empty", got)
	}
}

// --- end-to-end: the twin race ----------------------------------------------

// receiptInboundStub is flakyInboundStub's receipt-aware sibling: every
// POST is accepted (2xx, exactly like the live incident) but the first
// unvouchedPosts of them answer with a receipt that says the session
// got a fragment.
type receiptInboundStub struct {
	mu sync.Mutex
	// unvouchedPosts counts down the POSTs answered with a truncated
	// receipt; 0 means every POST vouches.
	unvouchedPosts int
	// omitReceipt answers with no delivery block at all — a gc that
	// predates gp-2rq.
	omitReceipt bool
	posts       []externalInboundMessage
}

func (s *receiptInboundStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		s.mu.Lock()
		s.posts = append(s.posts, env.Message)
		n := len(s.posts)
		unvouched := s.unvouchedPosts > 0
		if unvouched {
			s.unvouchedPosts--
		}
		omit := s.omitReceipt
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if omit {
			_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1"}`))
			return
		}
		id := "rcpt_" + string(rune('a'+n-1))
		expected := len([]rune(env.Message.Text))
		delivered := expected
		status := "delivered"
		if unvouched {
			// The live shape: only the boilerplate tail arrived.
			status = "partial"
			delivered = 41
		}
		_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"` + id +
			`","status":"` + status +
			`","delivered_chars":` + strconv.Itoa(delivered) +
			`,"expected_chars":` + strconv.Itoa(expected) + `}}`))
	}
}

func (s *receiptInboundStub) snapshot() []externalInboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]externalInboundMessage, len(s.posts))
	copy(out, s.posts)
	return out
}

func receiptClaimsConfig(gcURL string) config {
	cfg := claimsTestConfig(gcURL)
	cfg.deliveryReceiptGate = true
	return cfg
}

// THE BEAD'S REPRO. Two near-simultaneous deliveries of one ts (the
// `message` + `app_mention` twin pair) race the urgent path while gc
// accepts every POST but vouches for none of the first two. The winning
// twin must NOT conclude the claim on a 2xx: it re-posts in place, and
// when that still does not vouch it releases the claim so the parked
// twin re-posts. The recovery copy carries the COMPLETE payload.
func TestDeliveryReceipt_UnvouchedOwnerHandsOverToParkedTwin(t *testing.T) {
	// Owner's first post + its one in-place re-post both come back
	// unvouched; the twin's takeover post vouches.
	stub := &receiptInboundStub{unvouchedPosts: 1 + deliveryReceiptRepostAttempts}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> the founder message that must not be eaten"

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+string(rune('1'+i)), "C1", "1787890080.963799", "", text, true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	posts := stub.snapshot()
	want := 1 + deliveryReceiptRepostAttempts + 1 // owner + its re-post + twin takeover
	if len(posts) != want {
		t.Fatalf("gc saw %d posts, want %d (owner + %d in-place re-post + parked-twin takeover)",
			len(posts), want, deliveryReceiptRepostAttempts)
	}
	// Every attempt — the takeover above all — carries the whole
	// message unit, not a tail.
	for i, p := range posts {
		if !strings.Contains(p.Text, "the founder message that must not be eaten") {
			t.Errorf("post %d text = %q, want the complete payload", i, p.Text)
		}
	}
	// The claim must be committed by the vouched takeover, so a THIRD
	// copy of the same ts (a Slack redelivery) skips.
	if seen := cfg.deliveredIDs.seen("", "C1", "1787890080.963799"); !seen {
		t.Error("ts not recorded as delivered after the vouched takeover")
	}
}

// The receipt vouches on the first post: the parked twin skips exactly
// as it does today — the gate must not turn every twin pair into two
// deliveries.
func TestDeliveryReceipt_VouchedDeliveryStillSkipsTwin(t *testing.T) {
	stub := &receiptInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> vouched delivery"

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+string(rune('1'+i)), "C1", "100.000101", "", text, true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	if posts := stub.snapshot(); len(posts) != 1 {
		t.Fatalf("gc saw %d posts, want exactly 1 (vouched delivery, twin skips)", len(posts))
	}
}

// A gc that emits no receipt at all — today's live binary, and every gc
// until gp-2rq's rebuild lands — keeps the pre-gp-32q behavior exactly:
// one post, twin skips. This is what makes the pack safe to pin ahead
// of the core fix.
func TestDeliveryReceipt_UnsupportedGcKeepsLegacyBehavior(t *testing.T) {
	stub := &receiptInboundStub{omitReceipt: true}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> receipt-less gc"

	var wg sync.WaitGroup
	for i, eventType := range []string{"message", "app_mention"} {
		wg.Add(1)
		env := botMentionEnvelope(t, eventType, "Ev"+string(rune('1'+i)), "C1", "100.000102", "", text, true)
		go func(env slackEventEnvelope) {
			defer wg.Done()
			processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})
		}(env)
	}
	wg.Wait()

	if posts := stub.snapshot(); len(posts) != 1 {
		t.Fatalf("gc saw %d posts, want exactly 1 (no receipt = legacy commit-on-2xx)", len(posts))
	}
	if !cfg.deliveredIDs.seen("", "C1", "100.000102") {
		t.Error("ts not recorded as delivered against a receipt-less gc")
	}
}

// The kill switch: with the gate disarmed, a receipt that says the
// session got a fragment is ignored and the legacy path runs. This is
// the escape hatch if a gc build ever reports receipts wrongly.
func TestDeliveryReceipt_GateOffIgnoresFailingReceipt(t *testing.T) {
	stub := &receiptInboundStub{unvouchedPosts: 100}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.deliveryReceiptGate = false
	aliasReg := newTestHandleAliasRegistry(t)
	env := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000103", "",
		"<@"+testBotUserID+"> gate off", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})

	if posts := stub.snapshot(); len(posts) != 1 {
		t.Fatalf("gc saw %d posts, want exactly 1 (gate disarmed = no re-post)", len(posts))
	}
	if !cfg.deliveredIDs.seen("", "C1", "100.000103") {
		t.Error("ts not recorded as delivered with the gate disarmed")
	}
}

// A lone delivery (no twin) whose first receipt does not vouch recovers
// through the in-place re-post — the owner does not need a twin to
// exist to retry once.
func TestDeliveryReceipt_InPlaceRepostRecoversWithoutTwin(t *testing.T) {
	stub := &receiptInboundStub{unvouchedPosts: 1}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	env := botMentionEnvelope(t, "message", "Ev1", "C1", "100.000104", "",
		"<@"+testBotUserID+"> lone mention", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})

	posts := stub.snapshot()
	if len(posts) != 2 {
		t.Fatalf("gc saw %d posts, want 2 (first unvouched, in-place re-post vouched)", len(posts))
	}
	if !cfg.deliveredIDs.seen("", "C1", "100.000104") {
		t.Error("ts not recorded as delivered after the re-post vouched")
	}
}

// Coalesced chatter gc will not vouch for must not be recorded as
// delivered, must release its per-member claims so a same-ts urgent twin
// can take over, and must report the failure to the coalescer — an
// earlier revision returned nil here, which retired the batch and left an
// ordinary one-event room message with NO copy anywhere (codex r1 P2).
func TestDeliveryReceipt_UnvouchedBatchFailsIntoRecovery(t *testing.T) {
	stub := &receiptInboundStub{unvouchedPosts: 100}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.deliveryReceiptGate = true

	batch := []pendingChannelInbound{
		{inbound: externalInboundMessage{ProviderMessageID: "100.000200", Text: "chatter",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}},
	}
	err := deliverCoalescedBatch(cfg, "C1", batch)
	if !errors.Is(err, errDeliveryUnvouched) {
		t.Fatalf("deliverCoalescedBatch err = %v, want errDeliveryUnvouched (the batch must stay recoverable)", err)
	}
	if posts := stub.snapshot(); len(posts) != 1+deliveryReceiptRepostAttempts {
		t.Fatalf("gc saw %d posts, want %d (batch + its in-place re-post)", len(posts), 1+deliveryReceiptRepostAttempts)
	}
	if cfg.deliveredIDs.seen("", "C1", "100.000200") {
		t.Error("unvouched batch recorded as delivered — a later copy of this ts would be dropped as a duplicate")
	}
	// The claim must be released, not committed: begin() has to hand
	// ownership to the next caller rather than report "already
	// delivered".
	proceed, wait := cfg.channelClaims.begin(channelDeliveryClaimKey("C1", "100.000200"))
	if !proceed {
		t.Errorf("claim not released after an unvouched batch (proceed=%v wait=%v) — a same-ts twin would skip", proceed, wait != nil)
	}
}

// The bounded ladder: an unvouched batch is CHARGED, not restored
// uncharged. Restoring uncharged would re-notify the channel every
// window forever against a receipt that never vouches; charging retries
// it under maxCoalesceDeliveryAttempts and then writes it to the
// dead-letter file, reusing the gp-xnc machinery.
func TestDeliveryReceipt_UnvouchedBatchIsChargedAndDeadLettered(t *testing.T) {
	stub := &receiptInboundStub{unvouchedPosts: 1000}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.deliveryReceiptGate = true
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}
	var deadLettered []string
	cfg.coalescer.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		for _, p := range batch {
			deadLettered = append(deadLettered, p.inbound.ProviderMessageID)
		}
		return true
	}

	cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: "100.000210", Text: "never vouched",
		Conversation: conversationRef{ConversationID: "C1", Kind: "room"},
	}})
	for i := 0; i < maxCoalesceDeliveryAttempts; i++ {
		cfg.coalescer.flushAheadOf("C1", "")
	}

	if len(deadLettered) != 1 || deadLettered[0] != "100.000210" {
		t.Fatalf("dead-lettered = %v, want [100.000210] after %d charged attempts", deadLettered, maxCoalesceDeliveryAttempts)
	}
	cfg.coalescer.mu.Lock()
	pending := len(cfg.coalescer.pending["C1"])
	cfg.coalescer.mu.Unlock()
	if pending != 0 {
		t.Errorf("buffer still holds %d entries after the dead-letter — the ladder did not terminate", pending)
	}
}

// A batch carrying a no-wake reaction must return it to the side lane
// when the receipt says the session missed the delivery — otherwise the
// reaction leaves its buffer and is never delivered by anything.
// The isolation loop is a SECOND route to an unvouched delivery: a
// batch gc rejects with a 4xx is broken into singles, and gc can then
// accept a single without vouching for it. Classifying that as a
// transient fault would put the entry back UNCHARGED and re-notify the
// channel every window forever — the same unbounded loop the arm at the
// top of failed() closes for the batch route.
func TestDeliveryReceipt_UnvouchedSingleInIsolationIsCharged(t *testing.T) {
	var posts int32
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		atomic.AddInt32(&posts, 1)
		// A real batch carries both messages; gc rejects it outright.
		if strings.Contains(env.Message.Text, "alpha") && strings.Contains(env.Message.Text, "bravo") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"batch rejected"}`))
			return
		}
		// Each isolated single is ACCEPTED, and never vouched for.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"delivery":{"receipt_id":"rcpt_iso","status":"partial","delivered_bytes":7,"expected_bytes":420}}`))
	}))
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.deliveryReceiptGate = true
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}
	var deadLettered []string
	cfg.coalescer.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		for _, p := range batch {
			deadLettered = append(deadLettered, p.inbound.ProviderMessageID)
		}
		return true
	}

	for ts, body := range map[string]string{"100.000410": "alpha", "100.000411": "bravo"} {
		cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
			ProviderMessageID: ts, Text: body,
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"},
		}})
	}
	for i := 0; i < maxCoalesceDeliveryAttempts; i++ {
		cfg.coalescer.flushAheadOf("C1", "")
	}

	if len(deadLettered) != 2 {
		t.Fatalf("dead-lettered %v, want both singles after %d charged attempts — an unvouched single must not be put back uncharged",
			deadLettered, maxCoalesceDeliveryAttempts)
	}
	cfg.coalescer.mu.Lock()
	pending := len(cfg.coalescer.pending["C1"])
	cfg.coalescer.mu.Unlock()
	if pending != 0 {
		t.Errorf("buffer still holds %d entries — the isolation ladder did not terminate", pending)
	}
}

func TestDeliveryReceipt_UnvouchedBatchReturnsReactionsToSideLane(t *testing.T) {
	stub := &receiptInboundStub{unvouchedPosts: 100}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.deliveryReceiptGate = true
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}

	cfg.coalescer.enqueue("C1", pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: "100.000220", Text: "message",
		Conversation: conversationRef{ConversationID: "C1", Kind: "room"},
	}})
	cfg.coalescer.admitReaction("C1", pendingChannelInbound{
		reaction: true,
		inbound: externalInboundMessage{ProviderMessageID: "100.000221", Text: "reaction",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}},
	}, false)
	cfg.coalescer.flushAheadOf("C1", "")

	cfg.coalescer.mu.Lock()
	reactions := len(cfg.coalescer.reactions["C1"])
	cfg.coalescer.mu.Unlock()
	if reactions != 1 {
		t.Errorf("reaction side lane holds %d entries, want 1 (an unvouched batch must give its riders back)", reactions)
	}
}

// A vouched batch keeps today's behavior: claims committed, ts recorded.
func TestDeliveryReceipt_VouchedBatchCommitsClaims(t *testing.T) {
	stub := &receiptInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.deliveryReceiptGate = true

	batch := []pendingChannelInbound{
		{inbound: externalInboundMessage{ProviderMessageID: "100.000201", Text: "chatter",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}},
	}
	if err := deliverCoalescedBatch(cfg, "C1", batch); err != nil {
		t.Fatalf("vouched batch failed: %v", err)
	}
	if posts := stub.snapshot(); len(posts) != 1 {
		t.Fatalf("gc saw %d posts, want 1", len(posts))
	}
	if !cfg.deliveredIDs.seen("", "C1", "100.000201") {
		t.Error("vouched batch not recorded as delivered")
	}
	if proceed, wait := cfg.channelClaims.begin(channelDeliveryClaimKey("C1", "100.000201")); proceed || wait != nil {
		t.Errorf("claim not committed after a vouched batch (proceed=%v wait=%v)", proceed, wait != nil)
	}
}

// --- serialization invariant (codex r1 P3) -----------------------------------

// gatedReceiptStub holds the FIRST response open until released, so a
// test can prove what the second twin does while the owner's claim is
// still in flight. It also tracks peak concurrency: the whole point of
// the in-place re-post is that the owner keeps the claim across it, so
// two POSTs for one ts must never overlap.
type gatedReceiptStub struct {
	mu             sync.Mutex
	started        int
	finished       int
	maxConcurrent  int
	inFlight       int
	unvouchedPosts int
	release        chan struct{}
	firstStarted   chan struct{}
	firstOnce      sync.Once
}

func newGatedReceiptStub(unvouched int) *gatedReceiptStub {
	return &gatedReceiptStub{
		unvouchedPosts: unvouched,
		release:        make(chan struct{}),
		firstStarted:   make(chan struct{}),
	}
}

func (s *gatedReceiptStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)

		s.mu.Lock()
		s.started++
		mine := s.started
		s.inFlight++
		if s.inFlight > s.maxConcurrent {
			s.maxConcurrent = s.inFlight
		}
		unvouched := s.unvouchedPosts > 0
		if unvouched {
			s.unvouchedPosts--
		}
		s.mu.Unlock()

		if mine == 1 {
			s.firstOnce.Do(func() { close(s.firstStarted) })
			<-s.release
		}

		status, delivered := "delivered", len([]rune(env.Message.Text))
		if unvouched {
			status, delivered = "partial", 41
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"TargetSessionID":"sess-1","delivery":{"receipt_id":"rcpt","status":"` + status +
			`","delivered_chars":` + strconv.Itoa(delivered) +
			`,"expected_chars":` + strconv.Itoa(len([]rune(env.Message.Text))) + `}}`))

		s.mu.Lock()
		s.inFlight--
		s.finished++
		s.mu.Unlock()
	}
}

func (s *gatedReceiptStub) counts() (started, finished, peak int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started, s.finished, s.maxConcurrent
}

// The claim must stay HELD across the in-place re-post: while the owner
// is mid-attempt the twin has to be parked, not posting. Without this,
// a three-POST count could equally come from an implementation that
// releases the claim before re-posting and lets both twins run
// concurrently — which is the very race gp-ios closed.
func TestDeliveryReceipt_ClaimHeldAcrossInPlaceRepost(t *testing.T) {
	stub := newGatedReceiptStub(1 + deliveryReceiptRepostAttempts)
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> serialization check"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "message", "Ev1", "C1", "100.000300", "", text, true), func() {})
	}()

	// The owner is inside its first POST, holding the claim.
	select {
	case <-stub.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never reached gc")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "app_mention", "Ev2", "C1", "100.000300", "", text, true), func() {})
	}()

	// Give the twin room to misbehave. It must park on the claim rather
	// than POST alongside the owner.
	time.Sleep(250 * time.Millisecond)
	if started, _, _ := stub.counts(); started != 1 {
		t.Errorf("gc saw %d POSTs while the owner held its claim, want 1 (the twin must park)", started)
	}

	close(stub.release)
	wg.Wait()

	started, _, peak := stub.counts()
	want := 1 + deliveryReceiptRepostAttempts + 1
	if started != want {
		t.Errorf("gc saw %d POSTs, want %d (owner + %d in-place re-post + twin takeover)",
			started, want, deliveryReceiptRepostAttempts)
	}
	if peak != 1 {
		t.Errorf("peak concurrent POSTs for one ts = %d, want 1 — the claim was not held across the re-post", peak)
	}
	if !cfg.deliveredIDs.seen("", "C1", "100.000300") {
		t.Error("ts not recorded as delivered after the vouched takeover")
	}
}

// During the shutdown drain the owner must NOT spend a second gc
// round-trip: each POST can burn the full forward timeout, and two of
// them outrun shutdownEventDrainTimeout, so a re-post can still be in
// flight when shutdown seals the spool (codex r1 P2). The unvouched
// delivery goes straight to its failure path instead, whose drain arm
// spools the message for startup replay.
// An unvouched delivery during the shutdown drain CONCLUDES its claim
// once the copy is durably spooled, so the parked twin skips instead of
// taking over. A takeover POST can burn the full gcForwardClient
// timeout, outlive shutdownEventDrainTimeout and the spool seal, and
// then have no spool left to record its own failure — trading a
// recoverable message for a logged LOSS plus a possible partial
// delivery, on top of a second copy of a message the restart replays.
func TestDeliveryReceipt_DrainingTwinDefersToSpool(t *testing.T) {
	stub := newGatedReceiptStub(100) // nothing ever vouches
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	draining := &atomic.Bool{}
	cfg.draining = draining
	spoolPath := filepath.Join(t.TempDir(), "spool.jsonl")
	cfg.inboundSpool = newInboundSpool(spoolPath)

	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> the founder message that must not be eaten"
	ts := "100.000302"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "message", "Ev1", "C1", ts, "", text, true), func() {})
	}()

	select {
	case <-stub.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never reached gc")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "app_mention", "Ev2", "C1", ts, "", text, true), func() {})
	}()
	// Let the twin reach the claim and park behind the owner.
	time.Sleep(250 * time.Millisecond)
	if started, _, _ := stub.counts(); started != 1 {
		t.Fatalf("gc saw %d POSTs before the drain, want 1 (the twin must be parked)", started)
	}

	// SIGTERM lands while the owner is still inside its POST.
	draining.Store(true)
	close(stub.release)
	wg.Wait()

	if started, _, _ := stub.counts(); started != 1 {
		t.Errorf("gc saw %d POSTs, want 1 — the drain must stop both the in-place re-post and the parked-twin takeover", started)
	}
	if cfg.deliveredIDs.seen("", "C1", ts) {
		t.Error("an unvouched drain-time delivery was recorded as delivered")
	}
	// The deferral is only safe because the copy is durable: the owner's
	// drain arm spooled it, and the next startup replays it.
	spooled, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if !strings.Contains(string(spooled), ts) {
		t.Errorf("spool does not carry ts %s — the twin deferred to a redelivery path that does not exist:\n%s", ts, spooled)
	}
	if n := strings.Count(string(spooled), "the founder message that must not be eaten"); n != 1 {
		t.Errorf("spool carries the message %d time(s), want exactly 1", n)
	}
}

// The mirror of the test above, and the reason the deferral keys on the
// claim rather than on the drain flag: when the spool refuses the write
// (sealed, disabled, full), NOTHING durable exists, so the claim is
// RELEASED and the parked twin is the last recovery path there is.
func TestDeliveryReceipt_DrainingTwinTakesOverWhenNothingWasSpooled(t *testing.T) {
	stub := newGatedReceiptStub(1) // owner unvouched, takeover vouches
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	draining := &atomic.Bool{}
	cfg.draining = draining
	// Spooling disabled: the drain arm has nowhere durable to put the
	// copy, and says so with its LOSS log.
	cfg.inboundSpool = newInboundSpool("")

	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> the founder message that must not be eaten"
	ts := "100.000303"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "message", "Ev1", "C1", ts, "", text, true), func() {})
	}()
	select {
	case <-stub.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never reached gc")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "app_mention", "Ev2", "C1", ts, "", text, true), func() {})
	}()
	time.Sleep(250 * time.Millisecond)

	draining.Store(true)
	close(stub.release)
	wg.Wait()

	started, _, peak := stub.counts()
	if started != 2 {
		t.Errorf("gc saw %d POSTs, want 2 (owner + parked-twin takeover) — with no durable copy the twin must not defer", started)
	}
	if peak != 1 {
		t.Errorf("peak concurrent POSTs for one ts = %d, want 1 — the takeover must be serialized behind the owner", peak)
	}
	if !cfg.deliveredIDs.seen("", "C1", ts) {
		t.Error("ts not recorded as delivered after the vouched takeover")
	}
}

// A twin that skips its channel copy during the drain must not leave an
// hourglass behind. The busy mark's whole lifecycle is in-memory: it is
// cleared by the reply to a delivery this goroutine is no longer making,
// and the registry that could remove the reaction does not survive the
// restart — so the emoji would sit on the message forever while the
// spooled copy is replayed and answered normally after the restart.
func TestDeliveryReceipt_DrainDeferredTwinLeavesNoBusyMark(t *testing.T) {
	stub := newGatedReceiptStub(100) // nothing ever vouches
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)
	slackStub, rr := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slackStub)

	cfg := receiptClaimsConfig(gcSrv.URL)
	cfg.slackBotToken = "xoxb-fake"
	cfg.busyReaction = busyReactionDefault
	cfg.busyMarks = newBusyReactionRegistry()
	draining := &atomic.Bool{}
	cfg.draining = draining
	cfg.inboundSpool = newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))

	aliasReg := newTestHandleAliasRegistry(t)
	text := "<@" + testBotUserID + "> drain-time mention"
	ts := "100.000304"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "message", "Ev1", "C1", ts, "", text, true), func() {})
	}()
	select {
	case <-stub.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("owner never reached gc")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
			botMentionEnvelope(t, "app_mention", "Ev2", "C1", ts, "", text, true), func() {})
	}()
	time.Sleep(250 * time.Millisecond)

	draining.Store(true)
	close(stub.release)
	wg.Wait()

	// The owner never reaches its reactions.add — the failure branch
	// returns first — and cancels the mark it took. The twin is the only
	// goroutine that could still add one, and it must not: give the
	// async add room to appear before concluding it did not.
	time.Sleep(500 * time.Millisecond)
	rr.mu.Lock()
	defer rr.mu.Unlock()
	for _, rec := range rr.calls {
		if rec.op == "add" {
			t.Fatalf("a busy reaction was added on a drain-deferred delivery nothing can clear: chan=%s name=%s ts=%s",
				rec.channel, rec.name, rec.timestamp)
		}
	}
}

func TestDeliveryReceipt_NoRepostWhileDraining(t *testing.T) {
	stub := &receiptInboundStub{unvouchedPosts: 100}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := receiptClaimsConfig(gcSrv.URL)
	draining := &atomic.Bool{}
	draining.Store(true)
	cfg.draining = draining
	cfg.inboundSpool = newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))

	aliasReg := newTestHandleAliasRegistry(t)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		botMentionEnvelope(t, "message", "Ev1", "C1", "100.000301", "",
			"<@"+testBotUserID+"> draining", true), func() {})

	if posts := stub.snapshot(); len(posts) != 1 {
		t.Fatalf("gc saw %d posts while draining, want exactly 1 (no in-place re-post past the drain deadline)", len(posts))
	}
	if cfg.deliveredIDs.seen("", "C1", "100.000301") {
		t.Error("unvouched drain-time delivery recorded as delivered")
	}
}
