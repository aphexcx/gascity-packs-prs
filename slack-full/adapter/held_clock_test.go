package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// UNPROVEN P3/P4: this fixture interprets held as zero pastes and allows
// a later POST to reach the same session. It models no live gc process.
func clockReceipt(status, session string) deliveryReceipt {
	return deliveryReceipt{
		postStatus: 200, present: true, status: status, id: "receipt", targetSessionID: session,
		members: []deliveryReceiptMember{{SessionID: session, Status: status}},
	}
}

func TestHeldClockAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, summary, member     string
		code                      int
		summaryCount, memberCount int
		gateOff, absent           bool
		err                       error
		want                      bool
	}{
		{name: "held", summary: "held", member: "held", code: 200, want: true},
		{name: "normalized", summary: " Held ", member: "FAILED", code: 299, want: true},
		{name: "member without status", summary: "held", code: 201, want: true},
		{name: "adapter withheld vouch", summary: "held", member: "held", code: 202, err: errDeliveryUnvouched, want: true},
		{name: "redirect", summary: "held", member: "held", code: 300},
		{name: "unanswered", summary: "held", member: "held"},
		{name: "failed", summary: "failed", member: "held", code: 200},
		{name: "partial", summary: "partial", member: "held", code: 200},
		{name: "pending", summary: "pending", member: "held", code: 200},
		{name: "no route", summary: "no_route", code: 200},
		{name: "short", summary: "delivered", summaryCount: 1, code: 200},
		{name: "unknown summary", summary: "new", code: 200},
		{name: "partial member", summary: "held", member: "partial", code: 200},
		{name: "pending member", summary: "held", member: "pending", code: 200},
		{name: "unknown member", summary: "held", member: "new", code: 200},
		{name: "delivered member", summary: "held", member: "delivered", code: 200},
		{name: "summary bytes", summary: "held", member: "held", summaryCount: 1, code: 200},
		{name: "member bytes", summary: "held", member: "failed", memberCount: 1, code: 200},
		{name: "gate off", summary: "held", member: "held", code: 200, gateOff: true},
		{name: "no receipt", summary: "held", member: "held", code: 200, absent: true},
		{name: "other error", summary: "held", member: "held", code: 200, err: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := clockReceipt(tc.summary, "s1")
			r.postStatus, r.present = tc.code, !tc.absent
			r.delivered, r.deliveredOK, r.expected, r.expectedOK = tc.summaryCount, true, 2, true
			r.members = append(r.members, deliveryReceiptMember{Status: tc.member, Delivered: tc.memberCount, DeliveredOK: true})
			if got := r.heldAnswer(!tc.gateOff, tc.err); got != tc.want {
				t.Fatalf("held answer = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestHeldClockPOSTStatus(t *testing.T) {
	original := gcForwardClient
	t.Cleanup(func() { gcForwardClient = original })
	cfg := config{gcAPIBase: "http://fixture.invalid", deliveryReceiptGate: true}
	msg := testPending("C1", "100.1", "sample").inbound
	for _, path := range []string{"channel", "alias"} {
		t.Run(path, func(t *testing.T) {
			var first deliveryReceipt
			for _, code := range []int{200, 300} {
				gcForwardClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					// UNPROVEN P3: identical held bodies; only the HTTP status differs.
					body := `{"delivery":{"status":"held","members":[{"session_id":"s1","status":"held","delivered_bytes":0}]}}`
					return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
				})}
				var got deliveryReceipt
				if path == "channel" {
					var err error
					got, err = postInboundWithReceipt(cfg, msg)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var accepted bool
					got, accepted = sendAliasDispatch(cfg, "s1", "alias", "sample", msg)
					if !accepted {
						t.Fatal("alias refused response")
					}
					got.submission.Started, got.submission.Inbound.injectionStartedAt = time.Time{}, time.Time{}
				}
				if got.postStatus != code || !got.present || got.status != "held" {
					t.Errorf("HTTP %d: receipt status %q, code %d", code, got.status, got.postStatus)
				}
				got.postStatus = 0
				if code == 200 {
					first = got
				} else if !reflect.DeepEqual(first, got) {
					t.Error("other receipt fields changed")
				}
			}
		})
	}
}

// All elapsed times are seconds on a fake clock. The fixture keeps an
// independent inventory of admitted messages, POST instants and pastes.
type heldClockCase struct {
	t       *testing.T
	r       *inboundRecovery
	now     *time.Time
	start   time.Time
	entries []string
	posts   map[string][]int
	pastes  map[string]int
	answer  func(inboundRecoveryEntry) (deliveryReceipt, error)
}

func clockCase(t *testing.T, durable bool) *heldClockCase {
	t.Helper()
	r, now := round2Recovery(t)
	if !durable {
		r.path = ""
	}
	c := &heldClockCase{t: t, r: r, now: now, start: *now, posts: map[string][]int{}, pastes: map[string]int{}}
	c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) { return clockReceipt("held", "s1"), nil }
	r.send = c.send
	t.Cleanup(c.checkNoLoss)
	return c
}

func (c *heldClockCase) send(e inboundRecoveryEntry) (deliveryReceipt, error) {
	c.t.Helper()
	key := recoveryKey(e.Inbound)
	if c.pastes[key] != 0 {
		c.t.Errorf("POST after paste for %s", key)
	}
	c.posts[key] = append(c.posts[key], int(c.now.Sub(c.start)/time.Second))
	receipt, err := c.answer(e)
	if err == nil && receipt.verdict(true) == receiptVouched {
		c.pastes[key]++
	}
	return receipt, err
}

func (c *heldClockCase) hold(ts, session string) string {
	c.t.Helper()
	p := testPending("C1", ts, "sample")
	if err := c.r.hold(p, clockReceipt("held", session), false); err != nil {
		c.t.Fatal(err)
	}
	key := recoveryKey(p.inbound)
	c.entries = append(c.entries, key)
	return key
}

func (c *heldClockCase) tick(second int) {
	*c.now = c.start.Add(time.Duration(second) * time.Second)
	c.r.tick(false)
}

func (c *heldClockCase) through(second int) {
	for s := int(c.now.Sub(c.start)/time.Second) + 1; s <= second; s++ {
		c.tick(s)
	}
}

func (c *heldClockCase) restart() {
	c.t.Helper()
	r, err := newInboundRecovery(c.r.path, c.r.deadDir)
	if err != nil {
		c.t.Fatal(err)
	}
	r.now, r.send = c.r.now, c.send
	c.r = r
}

func (c *heldClockCase) history() []inboundDeadLetterRecord {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.r.deadDir, "C1.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		c.t.Fatal(err)
	}
	var rows []inboundDeadLetterRecord
	for _, line := range bytes.FieldsFunc(data, func(r rune) bool { return r == '\n' }) {
		var row inboundDeadLetterRecord
		if err := json.Unmarshal(line, &row); err != nil {
			c.t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func (c *heldClockCase) checkNoLoss() {
	c.t.Helper()
	// Let the normal worker write any outstanding retirement at the horizon.
	*c.now = c.now.Add(25 * time.Hour)
	c.r.tick(false)
	rows := c.history()
	for _, key := range c.entries {
		e, ok := c.r.state.Entries[key]
		if !ok {
			c.t.Errorf("admitted entry %s disappeared", key)
			continue
		}
		recorded, replayed := false, false
		for _, row := range rows {
			if recoveryKey(row.Inbound) == key {
				recorded = true
				replayed = replayed || !row.ReplayedAt.IsZero()
			}
		}
		switch {
		case c.pastes[key] == 1 && !e.DeliveredAt.IsZero() && e.inactive() && (e.DeadLetteredAt.IsZero() || replayed):
		case c.pastes[key] == 0 && !e.inactive() && !e.DeadLetteredAt.IsZero() && recorded:
		case c.pastes[key] == 0 && (!e.AcknowledgedAt.IsZero() || e.ManualRecovery || e.ReplayBlockedReason != "") && recorded:
		default:
			c.t.Errorf("%s: pastes=%d inactive=%t recorded=%t replayed=%t", key, c.pastes[key], e.inactive(), recorded, replayed)
		}
	}
}

func TestHeldClockGap(t *testing.T) {
	for _, ready := range []int{30, 90, 210, 510, 1110, 1900, 10800, 82800, 108000} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			c := clockCase(t, false)
			key := c.hold("100.1", "s1")
			c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				if c.now.Sub(c.start) >= time.Duration(ready)*time.Second {
					return clockReceipt("delivered", "s1"), nil
				}
				return clockReceipt("held", "s1"), nil
			}
			c.through(ready + 60)
			if ready == 108000 {
				e := c.r.state.Entries[key]
				if c.pastes[key] != 0 || e.DeadLetteredAt.IsZero() || e.inactive() {
					t.Fatal("expired entry was offered or lost")
				}
				for _, s := range c.posts[key] {
					if s > 1800+86400 {
						t.Fatalf("offer after replay age at %d", s)
					}
				}
			} else if c.pastes[key] != 1 {
				t.Fatalf("no paste within 60s of %ds", ready)
			}
		})
	}
}

func TestHeldClockCadence(t *testing.T) {
	c := clockCase(t, false)
	key := c.hold("100.1", "s1")
	// UNPROVEN P5/P7: this counts offers; it does not measure their cost to gc.
	c.through(10800)
	if n := len(c.posts[key]); n < 175 || n > 190 {
		t.Fatalf("three hours: %d offers", n)
	}
}

func TestHeldClockOneTurn(t *testing.T) {
	t.Run("held then delivered", func(t *testing.T) {
		c := clockCase(t, false)
		key := c.hold("100.1", "s1")
		c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
			if len(c.posts[key]) >= 4 {
				return clockReceipt("delivered", "s1"), nil
			}
			return clockReceipt("held", "s1"), nil
		}
		c.through(1800)
		c.r.observe(clockReceipt("delivered", "s1"))
		c.through(2000)
		if len(c.posts[key]) != 4 || c.pastes[key] != 1 {
			t.Error("expected one paste on the fourth offer")
		}
	})
	for _, conclusion := range []string{"delivered", "held"} {
		t.Run("pending concluded "+conclusion, func(t *testing.T) {
			c := clockCase(t, false)
			key := c.hold("100.1", "s1")
			pending := false
			c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				if pending {
					t.Error("POST while receipt pending")
				}
				if len(c.posts[key]) == 1 {
					pending = true
					return clockReceipt("pending", "s1"), nil
				}
				return clockReceipt("delivered", "s1"), nil
			}
			polls := 0
			c.r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				polls++
				if c.now.Sub(c.start) < 620*time.Second {
					return receiptPollPending, deliveryReceipt{}, nil
				}
				pending = false
				if conclusion == "delivered" {
					c.pastes[key]++
				}
				return receiptPollConcluded, clockReceipt(conclusion, "s1"), nil
			}
			c.through(619)
			if len(c.posts[key]) != 1 || polls == 0 {
				t.Fatal("expected only polls for ten minutes")
			}
			c.through(1000)
			want := 1
			if conclusion == "held" {
				want = 2
			}
			if len(c.posts[key]) != want || c.pastes[key] != 1 {
				t.Errorf("posts=%v pastes=%d", c.posts[key], c.pastes[key])
			}
		})
	}
	for _, outcome := range []string{"no answer", "no_route", "failed", "partial member", "short delivered"} {
		t.Run(outcome, func(t *testing.T) {
			c := clockCase(t, false)
			key := c.hold("100.1", "s1")
			c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				if len(c.posts[key]) == 1 {
					return clockReceipt("held", "s1"), nil
				}
				switch outcome {
				case "no answer":
					return deliveryReceipt{}, io.ErrUnexpectedEOF
				case "partial member":
					r := clockReceipt("held", "s1")
					r.members = append(r.members, deliveryReceiptMember{Status: "partial"})
					return r, nil
				case "short delivered":
					r := clockReceipt("delivered", "s1")
					r.delivered, r.deliveredOK, r.expected, r.expectedOK = 1, true, 2, true
					return r, nil
				default:
					return clockReceipt(outcome, "s1"), nil
				}
			}
			c.through(3600)
			want := []int{20, 80, 200, 500, 1100, 1800}
			if !reflect.DeepEqual(c.posts[key], want) {
				t.Fatalf("offers=%v want %v", c.posts[key], want)
			}
		})
	}
	t.Run("restart after held", func(t *testing.T) {
		c := clockCase(t, true)
		key := c.hold("100.1", "s1")
		c.tick(20)
		c.tick(80)
		c.restart()
		if !c.r.state.Entries[key].LastHeldAnswerAt.Equal(*c.now) {
			t.Error("latest held answer did not survive restart")
		}
		c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) { return clockReceipt("delivered", "s1"), nil }
		c.tick(139)
		c.tick(140)
		c.tick(141)
		if !reflect.DeepEqual(c.posts[key], []int{20, 80, 140}) || c.pastes[key] != 1 {
			t.Errorf("after restart %v", c.posts[key])
		}
	})
	t.Run("restart before POST", func(t *testing.T) {
		c := clockCase(t, true)
		key := c.hold("100.1", "s1")
		var snapshot []byte
		c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
			var err error
			snapshot, err = os.ReadFile(c.r.path)
			if err != nil {
				t.Fatal(err)
			}
			return clockReceipt("held", "s1"), nil
		}
		c.tick(20)
		c.tick(80)
		if len(snapshot) == 0 || bytes.Contains(snapshot, []byte("last_held_answer_at")) {
			t.Fatal("pre-POST snapshot carries a mark or is missing")
		}
		if err := os.WriteFile(c.r.path, snapshot, 0600); err != nil {
			t.Fatal(err)
		}
		c.restart()
		c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) { return clockReceipt("failed", "s1"), nil }
		// The saved ladder deadline was already due. Its next rung is 120s later.
		c.tick(81)
		c.through(200)
		if !reflect.DeepEqual(c.posts[key], []int{20, 80, 81}) {
			t.Fatalf("unexpected post-crash offers %v", c.posts[key])
		}
	})
	t.Run("gate off", func(t *testing.T) {
		c := clockCase(t, false)
		c.r.gated = false
		key := c.hold("100.1", "s1")
		if !c.r.state.Entries[key].LastHeldAnswerAt.IsZero() {
			t.Error("gate-off admission marked")
		}
		c.through(3600)
		if len(c.posts[key]) != 0 {
			t.Fatalf("gate-off offers %v", c.posts[key])
		}
	})
	t.Run("acknowledgement during offer", func(t *testing.T) {
		c := clockCase(t, false)
		key := c.hold("100.1", "s1")
		c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
			if len(c.posts[key]) == 2 {
				if n, err := c.r.acknowledge("C1", "100.1"); n != 1 || err != nil {
					t.Fatalf("acknowledge %d, %v", n, err)
				}
			}
			return clockReceipt("held", "s1"), nil
		}
		c.through(3600)
		if !reflect.DeepEqual(c.posts[key], []int{20, 80}) {
			t.Fatalf("offers %v", c.posts[key])
		}
	})
	t.Run("vouch and clock", func(t *testing.T) {
		c := clockCase(t, false)
		key := c.hold("100.1", "s1")
		c.tick(20)
		c.tick(80)
		c.r.observe(clockReceipt("delivered", "s1"))
		c.tick(140)
		c.tick(140)
		if !reflect.DeepEqual(c.posts[key], []int{20, 80, 140}) {
			t.Fatalf("offers %v", c.posts[key])
		}
	})
}

func TestHeldClockState(t *testing.T) {
	t.Run("save around POST", func(t *testing.T) {
		c := clockCase(t, true)
		key := c.hold("100.1", "s1")
		saved := func() time.Time {
			state, err := readRecoveryState(c.r.path)
			if err != nil {
				t.Fatal(err)
			}
			return state.Entries[key].LastHeldAnswerAt
		}
		if !saved().Equal(*c.now) {
			t.Error("initial held answer missing from state")
		}
		old, err := round2BaseReadRecoveryState(c.r.path)
		if err != nil || old.Entries[key].Inbound.ProviderMessageID != "100.1" {
			t.Fatal("old reader lost marked entry", err)
		}
		c.answer = func(inboundRecoveryEntry) (deliveryReceipt, error) {
			if !saved().IsZero() {
				t.Error("mark still saved during POST")
			}
			return clockReceipt("failed", "s1"), nil
		}
		c.tick(20)
		if !saved().IsZero() {
			t.Error("failed answer retained mark")
		}
	})
	t.Run("save failed", func(t *testing.T) {
		c := clockCase(t, true)
		key := c.hold("100.1", "s1")
		c.tick(20)
		c.tick(80)
		e := c.r.state.Entries[key]
		// Seed a durable mark even on the baseline so the failure stands alone.
		e.LastHeldAnswerAt = *c.now
		c.r.put(key, e)
		good := c.r.path
		c.r.path = filepath.Join(good, "not-a-directory")
		c.tick(140)
		c.r.path = good
		if len(c.posts[key]) != 2 || !c.r.state.Entries[key].LastHeldAnswerAt.Equal(e.LastHeldAnswerAt) {
			t.Fatal("save failure consumed the mark or made a POST")
		}
	})
	t.Run("old state and history keys", func(t *testing.T) {
		c := clockCase(t, true)
		key := c.hold("100.1", "s1")
		e := c.r.state.Entries[key]
		e.LastHeldAnswerAt = time.Time{}
		e.NextAttemptAt = c.start.Add(5 * time.Minute)
		c.r.put(key, e)
		raw, err := os.ReadFile(c.r.path)
		if err != nil || bytes.Contains(raw, []byte("last_held_answer_at")) {
			t.Fatal("empty field was serialized", err)
		}
		c.restart()
		c.through(299)
		if len(c.posts[key]) != 0 {
			t.Fatal("legacy entry got a clocked offer")
		}
		c.tick(300)
		if len(c.posts[key]) != 1 {
			t.Fatal("legacy rung missing")
		}
		c.tick(1800)
		raw, err = os.ReadFile(filepath.Join(c.r.deadDir, "C1.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err = json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		want := strings.Fields("acknowledged_at attempts channel dead_lettered_at inbound original_receipt_id reason receipt_state_known replayed_at target_sessions")
		got := make([]string, 0, len(keys))
		for k := range keys {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("history keys %v", got)
		}
	})
}

func TestHeldClockOrder(t *testing.T) {
	c := clockCase(t, false)
	old := c.hold("100.1", "s1")
	other := c.hold("100.3", "s2")
	var landed []string
	c.answer = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		key := recoveryKey(e.Inbound)
		if key == other || c.now.Sub(c.start) > 30*time.Minute {
			if key == old && !strings.HasPrefix(e.Inbound.Text, "[delivered late: dead-lettered at ") {
				t.Error("missing late marker")
			}
			landed = append(landed, key)
			return clockReceipt("delivered", e.TargetSessions[0]), nil
		}
		return clockReceipt("held", "s1"), nil
	}
	c.through(30)
	if c.pastes[other] != 1 || c.pastes[old] != 0 {
		t.Error("independent session did not make progress")
	}
	young := c.hold("100.2", "s1")
	c.through(1900)
	if !reflect.DeepEqual(landed, []string{other, old, young}) {
		t.Errorf("landing order %v", landed)
	}
	for _, s := range c.posts[young] {
		if s < 1860 {
			t.Errorf("younger offered early at %d", s)
			break
		}
	}
}

func TestHeldClockRow(t *testing.T) {
	c := clockCase(t, false)
	key := c.hold("100.1", "s1")
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	c.through(1800)
	rows := c.history()
	if len(rows) != 1 || rows[0].Reason != "unvouched delivery held for 30m0s" || c.r.counts()["C1"].Dead != 1 || !strings.Contains(logs.String(), "inbound HOLD: LOSS") {
		t.Fatal("missing bound record, reason, count or log")
	}
	n := len(c.posts[key])
	c.through(1920)
	if got := c.posts[key][n:]; !reflect.DeepEqual(got, []int{1860, 1920}) {
		t.Errorf("replay offers %v", got)
	}
	if strings.Contains(logs.String(), "inbound replay: trigger=") {
		t.Error("clock reported a replay trigger")
	}
	c.answer = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		if !strings.HasPrefix(e.Inbound.Text, "[delivered late: dead-lettered at ") {
			t.Error("missing late marker")
		}
		return clockReceipt("delivered", "s1"), nil
	}
	c.through(1980)
	// A vouch also allows the baseline to finish, independently of the clock.
	c.r.observe(clockReceipt("delivered", "s1"))
	c.tick(1981)
	rows = c.history()
	if len(rows) != 2 || rows[1].ReplayedAt.IsZero() {
		t.Error("missing replay completion record")
	}
}
