package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration coverage for the gp-0qw reminder-atomicity pass at the
// processSlackEvent seam: the deletion notice for buffered copies of a
// deleted message (item 3), the thread-reply anchor (item 2), and the
// head-protected reminder budget shared with gp-9gc (item 1).

// budgetedTestConfig is coalescingTestConfig plus a reminder budget,
// wired before the deliver closure captures the cfg copy.
func budgetedTestConfig(gcURL string, window time.Duration, budget int) config {
	cfg := config{
		gcAPIBase:          gcURL,
		cityName:           "test-city",
		provider:           "slack",
		accountID:          "T1",
		handlePrefix:       "@",
		dispatchSem:        defaultTestDispatchSem,
		peerContext:        newPeerContextBuffer(),
		deliveredIDs:       newDeliveredIDs(),
		replyHelp:          newOncePerChannel(),
		bindingCheck:       newBindingCheckCache(),
		coalescer:          newInboundCoalescer(window, nil),
		reminderTextBudget: budget,
	}
	deliverCfg := cfg
	cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
		return deliverCoalescedBatch(deliverCfg, channel, batch)
	}
	return cfg
}

func rawEnvelope(t *testing.T, eventID string, ev slackMessageEvent) slackEventEnvelope {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return slackEventEnvelope{Type: "event_callback", EventID: eventID, Event: raw}
}

func TestMessageDeleted_BufferedCopyDeliversDeletionNotice(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 60*time.Millisecond)
	aliasReg := newTestHandleAliasRegistry(t)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev1", "C1", "100.000001", "delete me please"), func() {})
	// The sender deletes the message while it sits in the coalesce
	// buffer.
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		rawEnvelope(t, "Ev2", slackMessageEvent{
			Type: "message", Subtype: "message_deleted",
			Channel: "C1", TS: "100.000002", DeletedTS: "100.000001",
		}), func() {})

	waitFor(t, "deletion-notice delivery", func() bool { return len(stub.snapshotInbounds()) == 1 })
	got := stub.snapshotInbounds()[0]
	if !strings.Contains(got.Text, deletedBySenderNotice) {
		t.Fatalf("delivery must carry the deletion notice, got:\n%s", got.Text)
	}
	if strings.Contains(got.Text, "delete me please") {
		t.Fatalf("deleted message text must not deliver:\n%s", got.Text)
	}
	if got.ProviderMessageID != "100.000001" {
		t.Fatalf("notice keeps the deleted ts as its id, got %q", got.ProviderMessageID)
	}
}

func TestMessageDeleted_UnknownTSIsNoop(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 40*time.Millisecond)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		rawEnvelope(t, "Ev1", slackMessageEvent{
			Type: "message", Subtype: "message_deleted",
			Channel: "C1", TS: "100.000002", DeletedTS: "100.000001",
		}), func() {})
	time.Sleep(150 * time.Millisecond)
	if got := stub.snapshotInbounds(); len(got) != 0 {
		t.Fatalf("a deletion of an unbuffered ts must deliver nothing, got %d", len(got))
	}
	// Nil coalescer (directly-constructed configs) must be safe too.
	var c *inboundCoalescer
	if n := c.markDeleted("C1", "1.0"); n != 0 {
		t.Fatalf("nil coalescer markDeleted = %d, want 0", n)
	}
}

func TestCoalescedBatch_HelpWithheldOverBudgetThenReArms(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	help := replyHelpBlock(config{}, "C1")
	budget := len(help) + 100
	cfg := budgetedTestConfig(gcSrv.URL, 40*time.Millisecond, budget)
	aliasReg := newTestHandleAliasRegistry(t)

	// First delivery: body big enough that body+help overflows the
	// budget — the how-to must be withheld, never the body.
	bigBody := strings.Repeat("x", 200)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev1", "C1", "100.000001", bigBody), func() {})
	waitFor(t, "first flush", func() bool { return len(stub.snapshotInbounds()) == 1 })
	first := stub.snapshotInbounds()[0]
	if !strings.Contains(first.Text, bigBody) {
		t.Fatalf("body must survive over-budget delivery:\n%s", first.Text)
	}
	if strings.Contains(first.Text, "full reply how-to") {
		t.Fatalf("help block must be withheld when over budget:\n%s", first.Text)
	}

	// Second, smaller delivery: the re-armed how-to attaches now.
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev2", "C1", "100.000002", "short"), func() {})
	waitFor(t, "second flush", func() bool { return len(stub.snapshotInbounds()) == 2 })
	second := stub.snapshotInbounds()[1]
	if !strings.Contains(second.Text, "full reply how-to") {
		t.Fatalf("withheld help block must re-arm and ride the next fitting delivery:\n%s", second.Text)
	}
}

func TestUrgent_HelpWithheldAndBodyTrimmedHeadPreserved(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := budgetedTestConfig(gcSrv.URL, time.Hour, 700)
	head := strings.Repeat("H", 250)
	body := head + strings.Repeat("T", 5000)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		rawEnvelope(t, "Ev1", slackMessageEvent{
			Type: "app_mention", Channel: "C1", User: "U_ALICE", TS: "100.000001", Text: body,
		}), func() {})

	got := stub.snapshotInbounds()
	if len(got) != 1 {
		t.Fatalf("urgent path must deliver immediately, captured %d", len(got))
	}
	text := got[0].Text
	if !strings.HasPrefix(text, head) {
		t.Fatalf("protected body head must lead the delivery:\n%.300s", text)
	}
	if !strings.Contains(text, "[message trimmed to fit the delivery budget — full text at ts 100.000001 in channel C1]") {
		t.Fatalf("trim marker missing:\n%s", text)
	}
	if strings.Contains(text, "full reply how-to") {
		t.Fatalf("help block must be withheld on an over-budget delivery:\n%s", text)
	}
	if len(text) > 700 {
		t.Fatalf("delivery len=%d exceeds budget 700", len(text))
	}
}

func TestUrgentThreadReply_CarriesAnchorWithoutContextCache(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	// A DM thread reply takes the urgent path; with no
	// threadContextCache there is no fetch and no parent line — the
	// anchor still carries the thread ts (gp-0qw item 2).
	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		rawEnvelope(t, "Ev1", slackMessageEvent{
			Type: "message", Channel: "D0DM", ChannelType: "im", User: "U_ALICE",
			TS: "100.000002", ThreadTS: "100.000001", Text: "threaded answer",
		}), func() {})

	got := stub.snapshotInbounds()
	if len(got) != 1 {
		t.Fatalf("captured %d inbounds, want 1", len(got))
	}
	// The once-per-channel how-to may follow; the anchor + body prefix
	// is the contract under test.
	want := "[thread reply — reply with --thread-ts 100.000001]\nthreaded answer"
	if !strings.HasPrefix(got[0].Text, want) {
		t.Fatalf("thread reply text = %q, want prefix %q", got[0].Text, want)
	}
	if got[0].ReplyToMessageID != "100.000001" {
		t.Fatalf("ReplyToMessageID = %q, want the thread ts", got[0].ReplyToMessageID)
	}
}

func TestSingleBufferedThreadReply_CarriesAnchor(t *testing.T) {
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 40*time.Millisecond)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		rawEnvelope(t, "Ev1", slackMessageEvent{
			Type: "message", Channel: "C1", User: "U_ALICE",
			TS: "100.000002", ThreadTS: "100.000001", Text: "buffered thread reply",
		}), func() {})
	waitFor(t, "buffered flush", func() bool { return len(stub.snapshotInbounds()) == 1 })
	text := stub.snapshotInbounds()[0].Text
	if !strings.HasPrefix(text, "[thread reply — reply with --thread-ts 100.000001]\n") {
		t.Fatalf("single buffered thread reply must lead with the anchor:\n%s", text)
	}
	if !strings.Contains(text, "buffered thread reply") {
		t.Fatalf("body missing:\n%s", text)
	}
}

func TestMessageDeleted_TombstoneBeatsEnqueue(t *testing.T) {
	// codex round-1 finding 3: the deletion event can be processed before
	// the message's own enqueue (event goroutines run concurrently).
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 60*time.Millisecond)
	aliasReg := newTestHandleAliasRegistry(t)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		rawEnvelope(t, "Ev2", slackMessageEvent{
			Type: "message", Subtype: "message_deleted",
			Channel: "C1", TS: "100.000002", DeletedTS: "100.000001",
		}), func() {})
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil,
		humanEnvelope(t, "Ev1", "C1", "100.000001", "delete me please"), func() {})

	waitFor(t, "deletion-notice delivery", func() bool { return len(stub.snapshotInbounds()) == 1 })
	got := stub.snapshotInbounds()[0]
	if !strings.Contains(got.Text, deletedBySenderNotice) || strings.Contains(got.Text, "delete me please") {
		t.Fatalf("late-enqueued copy must deliver as the notice:\n%s", got.Text)
	}
}

func TestMessageDeleted_TombstoneAppliesOnRestore(t *testing.T) {
	// A copy detached into a failed delivery is restored AFTER the
	// deletion landed: the retry must carry the notice.
	c := newInboundCoalescer(time.Hour, nil)
	c.markDeleted("C1", "100.000001")
	c.restore("C1", []pendingChannelInbound{{
		inbound: externalInboundMessage{ProviderMessageID: "100.000001", Text: "original"},
		body:    "original",
	}})
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.pending["C1"]
	if len(got) != 1 || got[0].inbound.Text != deletedBySenderNotice || got[0].body != "" {
		t.Fatalf("restored copy must be the deletion notice, got %+v", got)
	}
}

func TestSingleBufferedMessage_BudgetMatchesImmediatePath(t *testing.T) {
	// codex round-1 finding 1: a single buffered message must be bounded
	// exactly like the immediate path.
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := budgetedTestConfig(gcSrv.URL, 40*time.Millisecond, 700)
	head := strings.Repeat("H", 250)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
		humanEnvelope(t, "Ev1", "C1", "100.000001", head+strings.Repeat("T", 5000)), func() {})
	waitFor(t, "buffered flush", func() bool { return len(stub.snapshotInbounds()) == 1 })
	text := stub.snapshotInbounds()[0].Text
	if !strings.HasPrefix(text, head) {
		t.Fatalf("protected head must lead:\n%.300s", text)
	}
	if !strings.Contains(text, "[message trimmed to fit the delivery budget — full text at ts 100.000001 in channel C1]") {
		t.Fatalf("trim marker missing:\n%s", text)
	}
	if strings.Contains(text, "full reply how-to") || len(text) > 700 {
		t.Fatalf("help withheld and budget honored expected (len=%d):\n%s", len(text), text)
	}
}

func TestSingleBufferedThreadReply_CarriesStoredAnchorWithParent(t *testing.T) {
	// codex round-1 finding 4: the parent line captured at intake rides
	// the buffered copy through to delivery.
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, 40*time.Millisecond)
	anchor := formatThreadReplyAnchor("100.000001", "Afik", "ship it?")
	cfg.coalescer.enqueue("C1", pendingChannelInbound{
		inbound: externalInboundMessage{
			ProviderMessageID: "100.000002", ReplyToMessageID: "100.000001",
			Text: "yes, approved", DedupKey: "slack-100.000002",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"},
		},
		threadAnchor: anchor,
		body:         "yes, approved",
	})
	waitFor(t, "buffered flush", func() bool { return len(stub.snapshotInbounds()) == 1 })
	text := stub.snapshotInbounds()[0].Text
	if !strings.HasPrefix(text, anchor+"\nyes, approved") {
		t.Fatalf("stored anchor with parent line must lead the delivery:\n%s", text)
	}
}

func TestSpool_RoundTripsReminderParts(t *testing.T) {
	path := t.TempDir() + "/spool.jsonl"
	anchor := formatThreadReplyAnchor("1.0", "Afik", "root line")
	if !newInboundSpool(path).spillBatch("C1", []pendingChannelInbound{{
		inbound:      externalInboundMessage{ProviderMessageID: "1.1", Text: "pre\n---\n\nbody"},
		threadAnchor: anchor, preamble: "pre\n---\n\n", body: "body",
	}}) {
		t.Fatalf("spill failed")
	}
	c := newInboundCoalescer(time.Hour, nil)
	if n := newInboundSpool(path).replayInto(c); n != 1 {
		t.Fatalf("replayed %d, want 1", n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.pending["C1"]
	if len(got) != 1 || got[0].threadAnchor != anchor || got[0].preamble != "pre\n---\n\n" || got[0].body != "body" {
		t.Fatalf("parts did not survive the spool round trip: %+v", got)
	}
}

func TestAliasDispatch_ThreadReplyThreadsUnderRoot(t *testing.T) {
	// codex round-1 finding 5: the alias reply how-to must name the
	// thread ROOT for --thread-ts, never the child reply's own ts.
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := config{gcAPIBase: gcSrv.URL, cityName: "test-city"}
	inbound := externalInboundMessage{
		ProviderMessageID: "100.000002",
		ReplyToMessageID:  "100.000001",
		Conversation:      conversationRef{ConversationID: "C1"},
		Actor:             externalActor{ID: "U_ALICE"},
		Text:              "threaded ask",
	}
	if _, ok := dispatchToAliasedSession(cfg, "sess-mayor", inbound, "mayor"); !ok {
		t.Fatalf("dispatch failed")
	}
	msgs := stub.snapshotSessionMessages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d session messages, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0], "--thread-ts 100.000001") || strings.Contains(msgs[0], "--thread-ts 100.000002") {
		t.Fatalf("reply how-to must thread under the root:\n%s", msgs[0])
	}
	if !strings.Contains(msgs[0], "(Slack ts 100.000002, a reply in thread 100.000001)") {
		t.Fatalf("thread note missing:\n%s", msgs[0])
	}
}

func TestMessageDeleted_IsolationDeliversNotice(t *testing.T) {
	// codex round-2 finding 2: a deletion landing while a batch is
	// detached must reach the isolation singles, not just the pending
	// buffer.
	c := newInboundCoalescer(time.Hour, nil)
	var mu sync.Mutex
	var singles []externalInboundMessage
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range batch {
			singles = append(singles, p.inbound)
		}
		return nil
	}
	batch := []pendingChannelInbound{
		{inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: "original one"}, body: "original one"},
		{inbound: externalInboundMessage{ProviderMessageID: "2.0", Text: "original two"}, body: "original two"},
	}
	c.markDeleted("C1", "1.0")
	c.failed("C1", batch, &inboundPostError{Status: 422})
	mu.Lock()
	defer mu.Unlock()
	if len(singles) != 2 {
		t.Fatalf("isolation delivered %d singles, want 2", len(singles))
	}
	for _, m := range singles {
		if m.ProviderMessageID == "1.0" && m.Text != deletedBySenderNotice {
			t.Fatalf("deleted entry re-delivered with original text: %q", m.Text)
		}
	}
}

func TestMessageDeleted_PostCloseSpillCarriesNotice(t *testing.T) {
	// codex round-2 finding 3: a straggler enqueue after the admission
	// barrier spills durably — tombstones are memory-only, so the
	// spilled line must already be the notice.
	c := newInboundCoalescer(time.Hour, nil)
	var mu sync.Mutex
	var spilled []pendingChannelInbound
	c.spill = func(channel string, batch []pendingChannelInbound) bool {
		mu.Lock()
		defer mu.Unlock()
		spilled = append(spilled, batch...)
		return true
	}
	c.markDeleted("C1", "1.0")
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.enqueue("C1", pendingChannelInbound{
		inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: "original"},
		body:    "original",
	})
	mu.Lock()
	defer mu.Unlock()
	if len(spilled) != 1 || spilled[0].inbound.Text != deletedBySenderNotice {
		t.Fatalf("post-close spill must carry the deletion notice, got %+v", spilled)
	}
}

func TestSingleBufferedReaction_GetsNoReplyAnchor(t *testing.T) {
	// codex round-2 finding 5: a reaction notification is not an ask —
	// the legacy-parts fallback must not synthesize a reply anchor.
	stub := &gcRouterStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{{
		reaction: true,
		inbound: externalInboundMessage{
			ProviderMessageID: "100.000002", ReplyToMessageID: "100.000001",
			Text: "[reaction] thumbsup on 100.000001 — no reply expected",
		},
	}})
	if err != nil {
		t.Fatalf("deliver failed: %v", err)
	}
	got := stub.snapshotInbounds()
	if len(got) != 1 {
		t.Fatalf("captured %d, want 1", len(got))
	}
	if strings.Contains(got[0].Text, "[thread reply") {
		t.Fatalf("reaction delivery must not carry a reply anchor:\n%s", got[0].Text)
	}
}

func TestDeletionTombstones_TTLSweepSpansChannels(t *testing.T) {
	// codex round-2 finding 6: expiry must reclaim tombstones in
	// channels with no later deletion.
	c := newInboundCoalescer(time.Hour, nil)
	c.mu.Lock()
	c.deleted["OLD"] = map[string]time.Time{"1.0": time.Now().Add(-2 * deletionTombstoneTTL)}
	c.mu.Unlock()
	c.markDeleted("NEW", "2.0")
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.deleted["OLD"]; ok {
		t.Fatalf("expired tombstones in other channels must be reclaimed: %+v", c.deleted)
	}
	if _, ok := c.deleted["NEW"]["2.0"]; !ok {
		t.Fatalf("fresh tombstone missing: %+v", c.deleted)
	}
}

func TestMessageDeleted_DuringIsolationLoop(t *testing.T) {
	// codex round-3 finding 2: a deletion landing between isolation
	// singles must reach the later single.
	c := newInboundCoalescer(time.Hour, nil)
	var mu sync.Mutex
	var singles []externalInboundMessage
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		singles = append(singles, batch[0].inbound)
		first := len(singles) == 1
		mu.Unlock()
		if first {
			// The sender deletes message two while message one posts.
			c.markDeleted("C1", "2.0")
		}
		return nil
	}
	c.failed("C1", []pendingChannelInbound{
		{inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: "one"}, body: "one"},
		{inbound: externalInboundMessage{ProviderMessageID: "2.0", Text: "two"}, body: "two"},
	}, &inboundPostError{Status: 422})
	mu.Lock()
	defer mu.Unlock()
	if len(singles) != 2 || singles[1].Text != deletedBySenderNotice {
		t.Fatalf("second single must carry the notice, got %+v", singles)
	}
}

func TestMessageDeleted_DeadLetterWritesNotice(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	var got []pendingChannelInbound
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		got = append(got, batch...)
		return true
	}
	c.markDeleted("C1", "1.0")
	c.charge("C1", pendingChannelInbound{
		inbound:  externalInboundMessage{ProviderMessageID: "1.0", Text: "poison"},
		body:     "poison",
		attempts: maxCoalesceDeliveryAttempts,
	}, &inboundPostError{Status: 422})
	if len(got) != 1 || got[0].inbound.Text != deletedBySenderNotice {
		t.Fatalf("dead-letter must record the notice, got %+v", got)
	}
}

func TestMessageDeleted_PostCloseRestoreSpillsNotice(t *testing.T) {
	// codex round-3 finding 3: a post-barrier restore spills durably and
	// must already carry the notice.
	c := newInboundCoalescer(time.Hour, nil)
	var spilled []pendingChannelInbound
	c.spill = func(channel string, batch []pendingChannelInbound) bool {
		spilled = append(spilled, batch...)
		return true
	}
	c.markDeleted("C1", "1.0")
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.restore("C1", []pendingChannelInbound{{
		inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: "original"},
		body:    "original",
	}})
	if len(spilled) != 1 || spilled[0].inbound.Text != deletedBySenderNotice {
		t.Fatalf("post-close restore spill must carry the notice, got %+v", spilled)
	}
}

func TestSpool_StoresPartsWithoutDuplicatingText(t *testing.T) {
	// codex round-3 finding 4: the folded Text is rebuilt from the parts
	// on replay instead of being persisted twice.
	path := t.TempDir() + "/spool.jsonl"
	body := strings.Repeat("b", 2000)
	preamble := "Thread context (1 earlier message):\n@alice: hi\n\n---\n\n"
	files := "[file: photo.jpg]"
	folded := preamble + body + "\n\n" + files
	if !newInboundSpool(path).spillBatch("C1", []pendingChannelInbound{{
		inbound:  externalInboundMessage{ProviderMessageID: "1.1", Text: folded},
		preamble: preamble, body: body, files: files,
	}}) {
		t.Fatalf("spill failed")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if strings.Count(string(raw), body) != 1 {
		t.Fatalf("body persisted %d times, want exactly once", strings.Count(string(raw), body))
	}
	c := newInboundCoalescer(time.Hour, nil)
	if n := newInboundSpool(path).replayInto(c); n != 1 {
		t.Fatalf("replayed %d, want 1", n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := c.pending["C1"][0].inbound.Text; got != folded {
		t.Fatalf("replayed Text must equal the folded legacy text:\n got %q\nwant %q", got, folded)
	}
}

func TestSpool_DeletionRecordAppliesOnReplayRegardlessOfOrder(t *testing.T) {
	// codex round-4 finding 1: a deletion processed AFTER a message was
	// spooled (or in the check-to-write window) must still replay as
	// the notice — the record is durable and order-independent.
	path := t.TempDir() + "/spool.jsonl"
	s := newInboundSpool(path)
	if !s.spillBatch("C1", []pendingChannelInbound{{
		inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: "original"},
		body:    "original",
	}}) {
		t.Fatalf("spill failed")
	}
	if !s.recordDeletion("C1", "1.0") {
		t.Fatalf("recordDeletion failed")
	}
	c := newInboundCoalescer(time.Hour, nil)
	if n := newInboundSpool(path).replayInto(c); n != 1 {
		t.Fatalf("replayed %d message entries, want 1", n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.pending["C1"]
	if len(got) != 1 || got[0].inbound.Text != deletedBySenderNotice || got[0].body != "" {
		t.Fatalf("replayed entry must be the deletion notice, got %+v", got)
	}
	if !c.isDeletedLocked("C1", "1.0", time.Now()) {
		t.Fatalf("replay must seed the in-memory tombstone")
	}
}

func TestMarkDeleted_PersistsThroughHook(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	var recorded []string
	c.persistDeletion = func(channel, ts string) { recorded = append(recorded, channel+"|"+ts) }
	c.markDeleted("C1", "1.0")
	if len(recorded) != 1 || recorded[0] != "C1|1.0" {
		t.Fatalf("persist hook not invoked, got %v", recorded)
	}
	// Sealed spool refuses the record without panicking.
	s := newInboundSpool(t.TempDir() + "/spool.jsonl")
	s.seal()
	if s.recordDeletion("C1", "1.0") {
		t.Fatalf("sealed spool must refuse the deletion record")
	}
}

func TestSpool_MergeFailureStillAppliesNewerDeletions(t *testing.T) {
	// codex round-5 finding 1: when a crash-mid-replay leftover cannot
	// absorb the newer spool, the newer spool's deletion records must
	// still apply to the entries replaying now.
	dir := t.TempDir()
	path := dir + "/spool.jsonl"
	s := newInboundSpool(path)
	if !s.spillBatch("C1", []pendingChannelInbound{{
		inbound: externalInboundMessage{ProviderMessageID: "1.0", Text: "original"},
		body:    "original",
	}}) {
		t.Fatalf("spill failed")
	}
	// Stage it as a leftover .replaying file that refuses appends.
	if err := os.Rename(path, s.replayingPath()); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := os.Chmod(s.replayingPath(), 0o400); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.replayingPath(), 0o600) })
	// The newer spool carries only the deletion.
	if !newInboundSpool(path).recordDeletion("C1", "1.0") {
		t.Fatalf("recordDeletion failed")
	}
	c := newInboundCoalescer(time.Hour, nil)
	if n := newInboundSpool(path).replayInto(c); n != 1 {
		t.Fatalf("replayed %d, want 1", n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := c.pending["C1"]; len(got) != 1 || got[0].inbound.Text != deletedBySenderNotice {
		t.Fatalf("replayed entry must be the deletion notice despite the failed merge, got %+v", got)
	}
}
