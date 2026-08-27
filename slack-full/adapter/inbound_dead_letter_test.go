package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// --- gp-xnc: poison-batch handling for the inbound coalescer ---------------
//
// Incident 2026-08-26 09:48Z: one inbound whose attachment gc rejected
// (422, deterministic) was restored and retried every window forever,
// and because restore re-queues ahead of newer messages the poison
// rode in every later batch — the whole channel was head-of-line
// blocked and the log carried one 422 line per 8s. These tests pin the
// contract that replaces that: a PERMANENT failure (HTTP 4xx other than
// 408/429) counts against a small per-message attempt cap and the
// message is dead-lettered once it is reached; a TRANSIENT failure
// (network error, 5xx, 429) keeps the pre-existing retry-forever
// durability; later innocent messages are never dropped with the
// poison.

func TestPermanentDeliveryFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"422 validation", &inboundPostError{Status: 422}, true},
		{"400 bad request", &inboundPostError{Status: 400}, true},
		{"413 payload too large", &inboundPostError{Status: 413}, true},
		{"415 unsupported media type", &inboundPostError{Status: 415}, true},
		{"401 unauthorized is operational, not payload", &inboundPostError{Status: 401}, false},
		{"403 forbidden is operational, not payload", &inboundPostError{Status: 403}, false},
		{"404 not found (wrong city / route mid-rollout) keeps retrying", &inboundPostError{Status: 404}, false},
		{"405 method not allowed keeps retrying", &inboundPostError{Status: 405}, false},
		{"408 request timeout is transient", &inboundPostError{Status: 408}, false},
		{"429 rate limited is transient", &inboundPostError{Status: 429}, false},
		{"500 is transient", &inboundPostError{Status: 500}, false},
		{"503 is transient", &inboundPostError{Status: 503}, false},
		{"network error is transient", errors.New("dial tcp 127.0.0.1:8080: connect: connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanentDeliveryFailure(tc.err); got != tc.want {
				t.Fatalf("permanentDeliveryFailure(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// postInbound's error must carry the HTTP status so the coalescer can
// classify it, while its text stays byte-identical to the historical
// "<status line>: <body>" form operators grep for.
func TestPostInboundReturnsTypedStatusError(t *testing.T) {
	const body = `{"code":"validation-failed","errors":[{"message":"expected required property mime_type to be present"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cfg := config{gcAPIBase: srv.URL, cityName: "citadel"}
	err := postInbound(cfg, externalInboundMessage{ProviderMessageID: "1.0"})
	if err == nil {
		t.Fatal("expected an error for a 422 response")
	}
	var pe *inboundPostError
	if !errors.As(err, &pe) {
		t.Fatalf("error %T is not *inboundPostError: %v", err, err)
	}
	if pe.Status != 422 {
		t.Fatalf("status = %d, want 422", pe.Status)
	}
	if !strings.Contains(pe.Body, "validation-failed") {
		t.Fatalf("body not carried: %q", pe.Body)
	}
	if want := "422 Unprocessable Entity: " + body; err.Error() != want {
		t.Fatalf("error text = %q, want %q", err.Error(), want)
	}
}

type deadLetterCall struct {
	channel string
	batch   []pendingChannelInbound
	cause   error
}

func permanent422() error {
	return &inboundPostError{Status: 422, StatusText: "422 Unprocessable Entity", Body: `{"code":"validation-failed"}`}
}

func TestCoalescerPermanentFailureDeadLettersAfterMaxAttempts(t *testing.T) {
	attempts := make(chan []pendingChannelInbound, 16)
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		attempts <- batch
		return permanent422()
	}
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "voice memo"))

	var call deadLetterCall
	select {
	case call = <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("poison message was never dead-lettered")
	}
	if call.channel != "C1" || len(call.batch) != 1 || call.batch[0].inbound.ProviderMessageID != "1.0" {
		t.Fatalf("unexpected dead-letter call: %+v", call)
	}
	if call.batch[0].attempts != maxCoalesceDeliveryAttempts {
		t.Fatalf("dead-lettered after %d attempts, want %d", call.batch[0].attempts, maxCoalesceDeliveryAttempts)
	}
	var pe *inboundPostError
	if !errors.As(call.cause, &pe) || pe.Status != 422 {
		t.Fatalf("cause must be the final delivery error, got %v", call.cause)
	}
	if n := len(attempts); n != maxCoalesceDeliveryAttempts {
		t.Fatalf("delivery attempted %d times, want exactly %d", n, maxCoalesceDeliveryAttempts)
	}
	for i := 0; i < maxCoalesceDeliveryAttempts; i++ {
		if b := <-attempts; b[0].attempts != i {
			t.Fatalf("attempt %d delivered with attempts=%d, want %d", i+1, b[0].attempts, i)
		}
	}
	// No retry storm: nothing further fires and the message is no
	// longer pending.
	select {
	case b := <-attempts:
		t.Fatalf("delivery retried after dead-lettering: %+v", b)
	case <-time.After(120 * time.Millisecond):
	}
	if c.pendingContains("C1", "1.0") {
		t.Fatal("dead-lettered message must not remain pending")
	}
}

func TestCoalescerDeadLetterSparesLaterMessages(t *testing.T) {
	var mu sync.Mutex
	attempts := make(chan []pendingChannelInbound, 32)
	delivered := make(chan []pendingChannelInbound, 8)
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		attempts <- batch
		for _, p := range batch {
			if p.inbound.ProviderMessageID == "1.0" {
				return permanent422()
			}
		}
		delivered <- batch
		return nil
	}
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "poison"))
	// Let the poison fail once, then queue an innocent message behind it.
	select {
	case <-attempts:
	case <-time.After(2 * time.Second):
		t.Fatal("first attempt did not fire")
	}
	c.enqueue("C1", testPending("C1", "2.0", "innocent"))

	select {
	case call := <-dead:
		if len(call.batch) != 1 || call.batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("only the poison must be dead-lettered, got %+v", call.batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("poison was never dead-lettered")
	}
	select {
	case batch := <-delivered:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "2.0" {
			t.Fatalf("innocent message must deliver alone after the poison is removed, got %+v", batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("innocent message behind the poison was never delivered")
	}
	select {
	case call := <-dead:
		t.Fatalf("unexpected second dead-letter: %+v", call.batch)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCoalescerTransientFailureRetriesBeyondPermanentCap(t *testing.T) {
	var mu sync.Mutex
	fails := maxCoalesceDeliveryAttempts + 1 // strictly more than the permanent cap
	calls := 0
	delivered := make(chan []pendingChannelInbound, 4)
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(15*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fails > 0 {
			fails--
			return errors.New("dial tcp 127.0.0.1:8080: connect: connection refused")
		}
		delivered <- batch
		return nil
	}
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "x"))
	select {
	case batch := <-delivered:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("unexpected batch %+v", batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transient failures must keep retrying until delivery succeeds")
	}
	select {
	case call := <-dead:
		t.Fatalf("transient failures must never dead-letter: %+v", call.batch)
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != maxCoalesceDeliveryAttempts+2 {
		t.Fatalf("deliver called %d times, want %d", calls, maxCoalesceDeliveryAttempts+2)
	}
}

// A nil hook (bare test configs) must still stop the storm.
func TestCoalescerPermanentFailureWithoutHookStopsRetrying(t *testing.T) {
	attempts := make(chan []pendingChannelInbound, 16)
	c := newInboundCoalescer(15*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		attempts <- batch
		return permanent422()
	}
	c.enqueue("C1", testPending("C1", "1.0", "x"))
	for i := 0; i < maxCoalesceDeliveryAttempts; i++ {
		select {
		case <-attempts:
		case <-time.After(2 * time.Second):
			t.Fatalf("attempt %d did not fire", i+1)
		}
	}
	select {
	case <-attempts:
		t.Fatal("retried past the cap with no dead-letter hook")
	case <-time.After(100 * time.Millisecond):
	}
	if c.pendingContains("C1", "1.0") {
		t.Fatal("message must be dropped from pending once the cap is hit")
	}
}

func TestWriteInboundDeadLetterAppendsJSONL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbound-dead-letter")
	batch := []pendingChannelInbound{
		{inbound: testPending("C1", "1.0", "first").inbound, attempts: 3},
		{inbound: testPending("C1", "2.0", "second").inbound, attempts: 3},
	}
	path, err := writeInboundDeadLetter(dir, "C1", batch, permanent422())
	if err != nil {
		t.Fatalf("writeInboundDeadLetter: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("dead-letter file %s must live directly under %s", path, dir)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL records, got %d:\n%s", len(lines), raw)
	}
	for i, line := range lines {
		var rec inboundDeadLetterRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not JSON: %v\n%s", i, err, line)
		}
		if rec.Channel != "C1" || rec.Attempts != 3 || rec.Inbound.ProviderMessageID != batch[i].inbound.ProviderMessageID {
			t.Fatalf("line %d fields wrong: %+v", i, rec)
		}
		if !strings.Contains(rec.Reason, "422") {
			t.Fatalf("line %d reason must carry the final error, got %q", i, rec.Reason)
		}
		if rec.DeadLetteredAt.IsZero() {
			t.Fatalf("line %d missing dead_lettered_at", i)
		}
	}
	// Second write appends rather than truncating.
	if _, err := writeInboundDeadLetter(dir, "C1", batch[:1], permanent422()); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	if n := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); n != 3 {
		t.Fatalf("want 3 records after append, got %d", n)
	}
	// Store discipline matches the inbound file store: 0o700 dir, 0o600 file.
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v (err %v), want 0o700", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v (err %v), want 0o600", info.Mode().Perm(), err)
	}
}

// The channel id is Slack-controlled: it must be sanitized into the
// file name so it cannot escape the dead-letter directory.
func TestWriteInboundDeadLetterSanitizesChannel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dl")
	path, err := writeInboundDeadLetter(dir, "../../escape", []pendingChannelInbound{testPending("x", "1.0", "x")}, permanent422())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("hostile channel escaped the store: %s", path)
	}
}

// A rejected MULTI-message batch is isolated — each member re-delivered
// alone under the same flush — so only the member gc actually rejects
// accrues attempts. In the incident the text message buffered 5s after
// the voice memo shared its batch; it must deliver, not die with it.
func TestCoalescerRejectedBatchIsolatesPoisonAndDeliversInnocents(t *testing.T) {
	var mu sync.Mutex
	delivered := make(chan []pendingChannelInbound, 8)
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range batch {
			if p.inbound.ProviderMessageID == "1.0" {
				return permanent422()
			}
		}
		delivered <- batch
		return nil
	}
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	// Both land inside one window: the first flush is a batch of two.
	c.enqueue("C1", testPending("C1", "1.0", "voice memo (poison)"))
	c.enqueue("C1", testPending("C1", "2.0", "text that shared the window"))

	select {
	case batch := <-delivered:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "2.0" {
			t.Fatalf("innocent batch-mate must deliver alone, got %+v", batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("innocent batch-mate of the poison was never delivered")
	}
	select {
	case call := <-dead:
		if len(call.batch) != 1 || call.batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("only the poison must be dead-lettered, got %+v", call.batch)
		}
		if call.batch[0].attempts != maxCoalesceDeliveryAttempts {
			t.Fatalf("poison attempts = %d, want %d", call.batch[0].attempts, maxCoalesceDeliveryAttempts)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("poison was never dead-lettered")
	}
	if c.pendingContains("C1", "1.0") || c.pendingContains("C1", "2.0") {
		t.Fatal("nothing should remain pending")
	}
}

func TestInboundDeadLetterDirConfig(t *testing.T) {
	env := baseSlackEnv()
	env["GC_CITY_PATH"] = "/city"
	lookup := func(key string) (string, bool) { v, ok := env[key]; return v, ok }
	cfg, err := loadConfigFromLookup(lookup)
	if err != nil {
		t.Fatalf("loadConfigFromLookup: %v", err)
	}
	if want := "/city/.gc/slack/inbound_dead_letter"; cfg.inboundDeadLetterDir != want {
		t.Fatalf("default inboundDeadLetterDir = %q, want %q", cfg.inboundDeadLetterDir, want)
	}
	env["SLACK_INBOUND_DEAD_LETTER_DIR"] = "/elsewhere/dl"
	if cfg, err = loadConfigFromLookup(lookup); err != nil {
		t.Fatalf("loadConfigFromLookup: %v", err)
	}
	if cfg.inboundDeadLetterDir != "/elsewhere/dl" {
		t.Fatalf("SLACK_INBOUND_DEAD_LETTER_DIR override ignored: %q", cfg.inboundDeadLetterDir)
	}
}

// --- codex round 1 (gp-xnc) -------------------------------------------------

// recordingDeliver returns a deliver hook that appends every batch's
// member ids (as one string per call) to an ordered log, and delegates
// the verdict to decide. Members are rendered "ts" or "ts(r)" for
// reactions.
func recordingDeliver(decide func(batch []pendingChannelInbound) error) (func(string, []pendingChannelInbound) error, func() []string) {
	var mu sync.Mutex
	var calls []string
	render := func(batch []pendingChannelInbound) string {
		parts := make([]string, 0, len(batch))
		for _, p := range batch {
			id := p.inbound.ProviderMessageID
			if p.reaction {
				id += "(r)"
			}
			parts = append(parts, id)
		}
		return strings.Join(parts, ",")
	}
	return func(channel string, batch []pendingChannelInbound) error {
			mu.Lock()
			calls = append(calls, render(batch))
			mu.Unlock()
			return decide(batch)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), calls...)
		}
}

func waitForCalls(t *testing.T, calls func() []string, want []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := calls()
		if len(got) >= len(want) {
			if strings.Join(got[:len(want)], " ") != strings.Join(want, " ") {
				t.Fatalf("delivery sequence\n got %v\nwant %v", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery sequence never reached %v; got %v", want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Finding 1: isolation must stop at the first TRANSIENT single — gc
// that hangs after rejecting a batch must not cost one client timeout
// per member under the channel flush mutex. The untested members and
// the reactions are restored together, uncharged, and the whole batch
// retries on the next window.
func TestCoalescerIsolationStopsAtFirstTransientError(t *testing.T) {
	var mu sync.Mutex
	transientOnce := true
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch) > 1 {
			return permanent422()
		}
		mu.Lock()
		defer mu.Unlock()
		if batch[0].inbound.ProviderMessageID == "1.0" && transientOnce {
			transientOnce = false
			return errors.New("dial tcp: connection refused")
		}
		return nil
	})
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "a"))
	c.enqueue("C1", testPending("C1", "2.0", "b"))
	c.enqueue("C1", testPending("C1", "3.0", "c"))
	// Pass 1: batch rejected, first single hits a transient error → stop.
	// Pass 2: batch rejected again (still >1), then every single delivers.
	waitForCalls(t, calls, []string{"1.0,2.0,3.0", "1.0", "1.0,2.0,3.0", "1.0", "2.0", "3.0"})
	if got := calls(); len(got) != 6 {
		t.Fatalf("unexpected extra deliveries: %v", got)
	}
	select {
	case call := <-dead:
		t.Fatalf("nothing may be dead-lettered on transient evidence: %+v", call.batch)
	default:
	}
	for _, ts := range []string{"1.0", "2.0", "3.0"} {
		if c.pendingContains("C1", ts) {
			t.Fatalf("%s still pending after delivery", ts)
		}
	}
}

// Finding 2: reactions are charged ONLY when their own group was
// submitted and rejected. Here the reaction is the poison: the message
// single delivers, then the reaction group is posted behind it (never
// solo) and rejected — one charge, back to the side-buffer.
func TestCoalescerReactionsChargedOnlyWhenTheirGroupIsRejected(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		for _, p := range batch {
			if p.reaction && p.inbound.ProviderMessageID == "2.0" {
				return permanent422()
			}
		}
		return nil
	})
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.admitReaction("C1", testPending("C1", "2.0", "reaction"), false)
	c.enqueue("C1", testPending("C1", "1.0", "msg"))
	waitForCalls(t, calls, []string{"1.0,2.0(r)", "1.0", "2.0(r)"})
	time.Sleep(60 * time.Millisecond) // a reaction alone arms no timer: nothing else may fire
	if got := calls(); len(got) != 3 {
		t.Fatalf("unexpected extra deliveries: %v", got)
	}
	c.mu.Lock()
	rs := append([]pendingChannelInbound(nil), c.reactions["C1"]...)
	c.mu.Unlock()
	if len(rs) != 1 || rs[0].inbound.ProviderMessageID != "2.0" || rs[0].attempts != 1 {
		t.Fatalf("reaction must be back in the side-buffer with one charge, got %+v", rs)
	}
	select {
	case call := <-dead:
		t.Fatalf("premature dead-letter: %+v", call.batch)
	default:
	}
}

// Finding 2 (other half): a rejection caused by the BATCH (e.g. its
// combined size), where every single and the reaction group then
// succeed, charges nothing.
func TestCoalescerBatchLevelRejectionChargesNothing(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch) > 1 {
			return permanent422()
		}
		return nil
	})
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.admitReaction("C1", testPending("C1", "2.0", "reaction"), false)
	c.enqueue("C1", testPending("C1", "1.0", "msg"))
	waitForCalls(t, calls, []string{"1.0,2.0(r)", "1.0", "2.0(r)"})
	time.Sleep(60 * time.Millisecond)
	if got := calls(); len(got) != 3 {
		t.Fatalf("unexpected extra deliveries: %v", got)
	}
	c.mu.Lock()
	leftover := len(c.reactions["C1"]) + len(c.pending["C1"])
	c.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("everything delivered, yet %d entries remain buffered", leftover)
	}
	select {
	case call := <-dead:
		t.Fatalf("nothing was rejected alone, yet dead-lettered: %+v", call.batch)
	default:
	}
}

// Finding 3: a dead-letter write that fails must not forget the entry.
// It stays in the buffer at the cap and the next rejection retries the
// write; only a confirmed write retires it.
func TestCoalescerDeadLetterWriteFailureKeepsEntry(t *testing.T) {
	var mu sync.Mutex
	var hookAttempts []int
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error { return permanent422() })
	c := newInboundCoalescer(15*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		mu.Lock()
		defer mu.Unlock()
		hookAttempts = append(hookAttempts, batch[0].attempts)
		return len(hookAttempts) > 2 // two writes "fail" (disk full), the third succeeds
	}
	c.enqueue("C1", testPending("C1", "1.0", "poison"))
	waitForCalls(t, calls, []string{"1.0", "1.0", "1.0", "1.0", "1.0"})
	time.Sleep(60 * time.Millisecond)
	if got := calls(); len(got) != 5 {
		t.Fatalf("want exactly 5 deliveries (3 to the cap, 2 more after the failed writes), got %v", got)
	}
	mu.Lock()
	got := append([]int(nil), hookAttempts...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("dead-letter hook called %d times, want 3", len(got))
	}
	// codex r2 finding 3: the counter saturates at the cap — a record
	// written after storage recovers still says "3 attempts".
	for i, a := range got {
		if a != maxCoalesceDeliveryAttempts {
			t.Fatalf("hook call %d saw attempts=%d, want %d (saturated)", i+1, a, maxCoalesceDeliveryAttempts)
		}
	}
	if c.pendingContains("C1", "1.0") {
		t.Fatal("entry must retire once the write is confirmed")
	}
}

// Finding 6: the stored reason is bounded INCLUDING the marker and never
// splits a multi-byte rune.
func TestWriteInboundDeadLetterTruncatesReasonOnRuneBoundary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dl")
	long := strings.Repeat("é", maxDeadLetterReasonBytes) // 2 bytes each: far over the bound
	path, err := writeInboundDeadLetter(dir, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}, errors.New(long))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var rec inboundDeadLetterRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Reason) > maxDeadLetterReasonBytes {
		t.Fatalf("reason is %d bytes, bound is %d", len(rec.Reason), maxDeadLetterReasonBytes)
	}
	if !utf8.ValidString(rec.Reason) {
		t.Fatal("truncation split a rune")
	}
	if !strings.HasSuffix(rec.Reason, "(truncated)") {
		t.Fatalf("truncation marker missing: %q", rec.Reason[len(rec.Reason)-20:])
	}
}

// Finding 5: a pre-planted symlink at <dir>/<channel>.jsonl must not
// redirect the append elsewhere.
func TestWriteInboundDeadLetterRefusesSymlinkTarget(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "victim.jsonl")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, "C1.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeInboundDeadLetter(dir, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}, permanent422()); err == nil {
		t.Fatal("append through a symlinked dead-letter file must be refused")
	}
	if raw, _ := os.ReadFile(elsewhere); len(raw) != 0 {
		t.Fatalf("symlink target was written: %q", raw)
	}
}

// codex r2 finding 1: a pre-planted FIFO at <dir>/<channel>.jsonl must
// not block the open (and with it the channel flush mutex) waiting for
// a reader that never comes — it is refused promptly like any other
// non-regular file.
func TestWriteInboundDeadLetterRefusesFIFOWithoutBlocking(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "C1.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := writeInboundDeadLetter(dir, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}, permanent422())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("append into a FIFO must be refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("open blocked on the FIFO — the channel flush mutex would hang")
	}
}

// codex r3 nit: a FIFO WITH a reader opens successfully (no ENXIO) and
// must then be rejected by the regular-file check on the open
// descriptor, promptly.
func TestWriteInboundDeadLetterRefusesFIFOWithReader(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "C1.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	rfd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(rfd)
	done := make(chan error, 1)
	go func() {
		_, err := writeInboundDeadLetter(dir, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}, permanent422())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO with a reader must be refused as non-regular, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer blocked on a FIFO that has a reader")
	}
	// Nothing may have been written into the FIFO.
	buf := make([]byte, 16)
	if n, err := syscall.Read(rfd, buf); err == nil && n > 0 {
		t.Fatalf("bytes were written into the FIFO: %q", buf[:n])
	}
}

// A dead-letter directory whose parent chain is missing is created and
// every created level is synced (codex r3 finding 1 path).
func TestWriteInboundDeadLetterCreatesMissingChain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "dl")
	path, err := writeInboundDeadLetter(dir, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}, permanent422())
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("record file missing after chain creation: %v", err)
	}
	for _, d := range []string{dir, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("chain level %s missing: %v", d, err)
		}
	}
}
