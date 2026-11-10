package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInboundRecoveryShutdownWaitsForReceiptPersistence(t *testing.T) {
	for _, mode := range []string{"send-pending", "send-delivered", "poll-delivered"} {
		t.Run(mode, func(t *testing.T) {
			status := "delivered"
			if mode == "send-pending" {
				status = "pending"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				fmt.Fprintf(w, `{"delivery":{"receipt_id":"new-receipt","status":%q}}`, status)
			}))
			defer srv.Close()
			cfg := recoveryTestConfig(t, srv.URL)
			r := cfg.inboundRecovery
			original := recoveryReceipt("original", "s1", false)
			if mode == "poll-delivered" {
				original.status = "pending"
			}
			if err := r.hold(testPending("C1", "100.1", "held"), original, true); err != nil {
				t.Fatal(err)
			}
			now := time.Now().Add(time.Minute)
			r.now = func() time.Time { return now }
			var draining atomic.Bool
			r.stopping = draining.Load
			returned, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			if mode == "poll-delivered" {
				r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
					close(returned)
					<-release
					return receiptPollConcluded, recoveryReceipt("original", "s1", true), nil
				}
			} else {
				send := r.send
				r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
					receipt, err := send(e)
					close(returned)
					<-release
					return receipt, err
				}
			}
			ticked := make(chan struct{})
			go func() { r.tick(false); close(ticked) }()
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not reach receipt boundary")
			}
			draining.Store(true)
			drained := make(chan struct{})
			go func() { cfg.coalescer.flushAll(); close(drained) }()
			select {
			case <-drained:
				t.Fatal("shutdown returned before recovery persisted its receipt")
			case <-time.After(50 * time.Millisecond):
			}
			unblock()
			select {
			case <-ticked:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery did not settle")
			}
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not finish after receipt persistence")
			}
			restarted, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil {
				t.Fatal(err)
			}
			e := restarted.state.Entries["C1|100.1"]
			if mode == "send-pending" {
				if !e.PendingReceipt || e.PendingReceiptID != "new-receipt" {
					t.Fatalf("restart lost pending receipt: %+v", e)
				}
			} else if e.DeliveredAt.IsZero() {
				t.Fatal("restart lost delivered receipt")
			}
		})
	}
}

func TestInboundRecoveryRejectedReplacementClearsPendingReceipt(t *testing.T) {
	for _, dead := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-dead=%t", dead), func(t *testing.T) {
			dir := t.TempDir()
			r, err := newInboundRecovery(filepath.Join(dir, "state.json"), filepath.Join(dir, "dead"))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			r.now = func() time.Time { return now }
			if err := r.hold(testPending("C1", "100.1", "held"), deliveryReceipt{present: true, id: "old", status: "pending"}, false); err != nil {
				t.Fatal(err)
			}
			if dead {
				e := r.state.Entries["C1|100.1"]
				e.DeadLetteredAt = now
				r.put("C1|100.1", e)
			}
			polls, posts := 0, 0
			r.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				polls++
				return receiptPollConcluded, recoveryReceipt("old", "s1", false), nil
			}
			r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				posts++
				return deliveryReceipt{}, &inboundPostError{Status: http.StatusUnprocessableEntity, StatusText: "422", Body: "rejected"}
			}
			now = now.Add(time.Minute)
			r.tick(false)
			for i := 0; i < 3; i++ {
				now = now.Add(time.Hour)
				r.tick(false)
			}
			e := r.state.Entries["C1|100.1"]
			if polls != 1 || posts != 1 || e.PendingReceipt || e.PendingReceiptID != "" || e.DeadLetteredAt.IsZero() {
				t.Fatalf("rejected replacement kept polling/reposting: polls=%d posts=%d entry=%+v", polls, posts, e)
			}
			restarted, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil {
				t.Fatal(err)
			}
			if e := restarted.state.Entries["C1|100.1"]; e.PendingReceipt || e.PendingReceiptID != "" {
				t.Fatal("stale pending receipt persisted across restart")
			}
		})
	}
}

func TestInboundRecoveryNewDeadLettersImmediatelyCountAsMissed(t *testing.T) {
	for _, path := range []string{"liveness", "receipt-fallback"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			r, err := newInboundRecovery(filepath.Join(dir, "state.json"), filepath.Join(dir, "dead"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := config{inboundRecovery: r, inboundDeadLetterDir: r.deadDir}
			l := newInboundLiveness(cfg, newFakeHistory())
			if path == "liveness" {
				l.deadLetterRetiredHolds([]livenessRetiredHold{{channel: "C1", ts: "100.1", known: true,
					held: livenessHeldReplay{Msg: slackHistoryMessage{Text: "missed"}}}})
			} else if !deadLetterHeldLoss(cfg, "C1", []pendingChannelInbound{testPending("C1", "100.1", "missed")}, errors.New("failed delivery")) {
				t.Fatal("could not retire the receipt")
			}
			for i := 0; i < 2; i++ {
				l.probe(context.Background(), probeModeWatchdog, "test")
				if l.lastMissed.Load() != 1 {
					t.Fatalf("probe %d lost a newly retired record: missed=%d", i, l.lastMissed.Load())
				}
			}
		})
	}
}

func TestInboundRecoveryIncompleteLivenessHistoryStaysMissed(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead")
	l := newInboundLiveness(config{inboundDeadLetterDir: dead}, newFakeHistory())
	clipped, _ := clipHeldMessage(slackHistoryMessage{Text: strings.Repeat("x", livenessHeldTextMax+100)})
	l.deadLetterRetiredHolds([]livenessRetiredHold{
		{channel: "C1", ts: "100.1", known: true, held: livenessHeldReplay{Msg: clipped}},
		{channel: "C1", ts: "100.2", known: true, held: livenessHeldReplay{Msg: slackHistoryMessage{Text: "file caption"}, FilesDropped: 1}},
		{channel: "C1", ts: "100.3", known: true, held: livenessHeldReplay{TextDropped: true}},
		{channel: "C1", threadTS: "100.4", cursor: "100.3", reason: "inbound liveness: unread thread replies"},
	})
	// Old records have only the liveness reason to identify an incomplete
	// payload. A real parent shares the thread alarm's provider message ID.
	_, err := writeInboundDeadLetterRecords(dead, "C1", []inboundDeadLetterRecord{
		{DeadLetteredAt: time.Now(), Channel: "C1", Reason: "inbound liveness: held replay aged out", Inbound: testPending("C1", "100.5", "legacy fragment").inbound},
		{DeadLetteredAt: time.Now(), Channel: "C1", Reason: "failed delivery", Inbound: testPending("C1", "100.4", "complete parent").inbound},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := newInboundRecovery(filepath.Join(dir, "state.json"), dead)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	posts := 0
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		posts++
		if !strings.Contains(e.Inbound.Text, "complete parent") {
			t.Errorf("attempted incomplete replay: %s", e.Inbound.ProviderMessageID)
		}
		return recoveryReceipt("done", "s1", true), nil
	}
	r.notice = func(string, string, string, string) (string, error) {
		t.Error("manual recovery record advertised automatic retries")
		return "notice", nil
	}
	r.tick(true)
	r.manual["C1"] = true
	r.observe(recoveryReceipt("trigger", "s1", true))
	r.tick(false)
	if posts != 1 || r.counts()["C1"].Dead != 5 || r.known("C1", "100.1") {
		t.Fatalf("incomplete payload was settled or blocked full backfill: posts=%d counts=%v", posts, r.counts())
	}
	restarted, err := newInboundRecovery(r.path, dead)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	restarted.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Error("restart attempted incomplete/already delivered replay")
		return deliveryReceipt{}, nil
	}
	restarted.tick(true)
	probe := newInboundLiveness(config{inboundRecovery: restarted}, newFakeHistory())
	probe.probe(context.Background(), probeModeWatchdog, "test")
	if probe.lastMissed.Load() != 5 {
		t.Fatalf("manual recovery alarms disappeared: missed=%d", probe.lastMissed.Load())
	}
}

func TestInboundRecoveryPeerHoldSurvivesRestart(t *testing.T) {
	for _, mode := range []string{"immediate", "coalesced"} {
		for _, status := range []string{"pending", "held", "failed"} {
			t.Run(mode+"/"+status, func(t *testing.T) {
				var posts []externalInboundMessage
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if strings.Contains(req.URL.Path, "/receipts/") {
						fmt.Fprint(w, `{"receipt_id":"first","state":"concluded","delivery":{"receipt_id":"first","status":"failed"}}`)
						return
					}
					var body struct {
						Message externalInboundMessage `json:"message"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					posts = append(posts, body.Message)
					if len(posts) == 1 {
						fmt.Fprintf(w, `{"delivery":{"receipt_id":"first","status":%q}}`, status)
					} else {
						fmt.Fprint(w, `{"delivery":{"receipt_id":"retry","status":"delivered"}}`)
					}
				}))
				defer srv.Close()
				cfg := recoveryTestConfig(t, srv.URL)
				cfg.peerContext = newPeerContextBuffer()
				// The coalesced callback must see this peer queue as well.
				cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
					return deliverCoalescedBatch(cfg, channel, batch)
				}
				peer := testPending("C1", "100.0", "unique-peer-payload").inbound
				cfg.peerContext.add(peerContextItem{Channel: "C1", TS: "100.0", Text: peer.Text, inbound: peer})
				if mode == "immediate" {
					env := botMentionEnvelope(t, "message", "immediate", "C1", "100.1", "", "<@"+testBotUserID+"> human payload", true)
					processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
				} else {
					cfg.coalescer.enqueue("C1", testPending("C1", "100.1", "human payload"))
					cfg.coalescer.flushAheadOf("C1", "")
				}
				if len(posts) != 1 || cfg.peerContext.count("C1") != 0 || cfg.inboundRecovery.counts()["C1"].Held != 2 {
					t.Fatalf("rider lacks durable ownership: posts=%d buffered=%d recovery=%v", len(posts), cfg.peerContext.count("C1"), cfg.inboundRecovery.counts())
				}
				cfg.peerContext = newPeerContextBuffer()
				if err := installInboundRecovery(&cfg); err != nil {
					t.Fatal(err)
				}
				cfg.inboundRecovery.now = func() time.Time { return time.Now().Add(time.Minute) }
				cfg.inboundRecovery.tick(false)
				if len(posts) != 3 || posts[1].ProviderMessageID != "100.0" || posts[2].ProviderMessageID != "100.1" {
					t.Fatalf("restart lost or reordered riders: %+v", posts)
				}
				if !strings.Contains(posts[1].Text, peer.Text) || strings.Contains(posts[2].Text, peer.Text) {
					t.Fatal("peer must have exactly one replay owner")
				}
				if got := cfg.inboundRecovery.counts()["C1"]; got.Held != 0 || got.Dead != 0 {
					t.Fatalf("recovery did not settle: %+v", got)
				}
			})
		}
	}
}

func TestInboundRecoveryLadderDoesNotRepostInPlace(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		posts++
		fmt.Fprint(w, `{"delivery":{"receipt_id":"unvouched","status":"failed"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	now := time.Now()
	cfg.inboundRecovery.now = func() time.Time { return now }
	cfg.coalescer.enqueue("C1", testPending("C1", "100.1", "held"))
	cfg.coalescer.flushAheadOf("C1", "")
	if posts != 1 {
		t.Fatalf("initial posts=%d", posts)
	}
	now = now.Add(20 * time.Second)
	cfg.inboundRecovery.tick(false)
	if posts != 2 {
		t.Fatalf("one ladder deadline made %d extra sends", posts-1)
	}
}

func TestInboundRecoveryPeerRenderingSurvivesRestart(t *testing.T) {
	var posted externalInboundMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Message externalInboundMessage `json:"message"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posted = body.Message
		fmt.Fprint(w, `{"delivery":{"receipt_id":"retry","status":"delivered"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	p := testPending("C1", "100.1", "")
	p.files = "[file: plan.txt at /tmp/plan.txt]"
	p.threadAnchor = "[thread 99.0]"
	if err := cfg.inboundRecovery.hold(p, recoveryReceipt("first", "s1", false), false); err != nil {
		t.Fatal(err)
	}
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.inboundRecovery.now = func() time.Time { return time.Now().Add(time.Minute) }
	cfg.inboundRecovery.tick(false)
	if !strings.Contains(posted.Text, "plan.txt") || !strings.Contains(posted.Text, "/tmp/plan.txt") {
		t.Fatalf("lost file-only reminder: %q", posted.Text)
	}
}

func TestInboundRecoverySynchronousLandingStopsRetry(t *testing.T) {
	r, _ := newInboundRecovery("", t.TempDir())
	now := time.Now()
	r.now = func() time.Time { return now }
	if err := r.hold(testPending("C1", "100.1", "body"), recoveryReceipt("first", "s1", false), false); err != nil {
		t.Fatal(err)
	}
	posts := 0
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		posts++
		receipt := parseDeliveryReceipt([]byte(`{"delivery":{"receipt_id":"whole","status":"partial","members":[{"session_id":"s1","status":"delivered"},{"session_id":"s2","status":"pending","delivered_bytes":10,"expected_bytes":10}]}}`))
		return receipt, &unvouchedDeliveryError{receipt: receipt}
	}
	now = now.Add(time.Minute)
	r.tick(false)
	now = now.Add(time.Hour)
	r.tick(false)
	if posts != 1 || r.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
		t.Fatalf("posts=%d state=%+v", posts, r.state.Entries)
	}
}

func TestInboundRecoveryOriginalLandingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	r, err := newInboundRecovery(filepath.Join(dir, "state.json"), filepath.Join(dir, "dead"))
	if err != nil {
		t.Fatal(err)
	}
	receipt := parseDeliveryReceipt([]byte(`{"delivery":{"receipt_id":"whole","status":"pending","members":[{"session_id":"s1","status":"pending","delivered_bytes":10,"expected_bytes":10}]}}`))
	if err := r.hold(testPending("C1", "100.1", "already pasted"), receipt, false); err != nil {
		t.Fatal(err)
	}
	r2, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	r2.now = func() time.Time { return time.Now().Add(time.Hour) }
	r2.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Fatal("reposted a known complete paste")
		return deliveryReceipt{}, nil
	}
	r2.tick(false)
	if r2.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
		t.Fatal("known landing was retired instead of completed")
	}
}

func TestInboundRecoveryImmediatePeerHasOneReplayOwner(t *testing.T) {
	var posts []externalInboundMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/receipts/") {
			fmt.Fprint(w, `{"receipt_id":"first","state":"concluded","delivery":{"receipt_id":"first","status":"failed"}}`)
			return
		}
		var body struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		posts = append(posts, body.Message)
		if len(posts) == 1 {
			fmt.Fprint(w, `{"delivery":{"receipt_id":"first","status":"pending"}}`)
		} else {
			fmt.Fprint(w, `{"delivery":{"receipt_id":"retry","status":"delivered"}}`)
		}
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	cfg.peerContext = newPeerContextBuffer()
	peer := testPending("C1", "100.0", "unique-peer-payload").inbound
	cfg.peerContext.add(peerContextItem{Channel: "C1", TS: "100.0", Text: peer.Text, inbound: peer})
	env := botMentionEnvelope(t, "message", "immediate", "C1", "100.1", "", "<@"+testBotUserID+"> human payload", true)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
	cfg.inboundRecovery.now = func() time.Time { return time.Now().Add(time.Minute) }
	cfg.inboundRecovery.tick(false)
	if len(posts) != 3 {
		t.Fatalf("posts=%d", len(posts))
	}
	peerCopies := 0
	for _, post := range posts[1:] {
		if strings.Contains(post.Text, peer.Text) {
			peerCopies++
		}
	}
	if peerCopies != 1 {
		t.Fatalf("peer replayed %d times: %+v", peerCopies, posts)
	}
}

func TestInboundRecoveryLegacyUncertaintyCannotAutoReplay(t *testing.T) {
	for _, alias := range []bool{false, true} {
		for _, journal := range []bool{false, true} {
			t.Run(fmt.Sprintf("alias=%t/journal=%t", alias, journal), func(t *testing.T) {
				dir := t.TempDir()
				dead := filepath.Join(dir, "dead")
				msg := testPending("C1", "100.1", "legacy").inbound
				rec := inboundDeadLetterRecord{Inbound: msg, Channel: "C1", DeadLetteredAt: time.Now(), Reason: errReceiptFollowUpStuck.Error()}
				if alias {
					rec.AliasSessionID = "s1"
					rec.AliasBody = "alias body"
				}
				if _, err := writeInboundDeadLetterRecords(dead, "C1", []inboundDeadLetterRecord{rec}); err != nil {
					t.Fatal(err)
				}
				r, _ := newInboundRecovery("", dead)
				if err := r.importDeadLetters(); err != nil {
					t.Fatal(err)
				}
				if journal {
					j := &inboundJournal{dir: filepath.Join(dir, "journal")}
					for _, id := range []string{"original", "current"} {
						jr := inboundJournalRecord{Channel: "C1", MessageTS: "100.1", ReceiptID: id, Status: "pending", Phase: "accepted", TargetSessions: []string{"s1"}}
						if alias {
							jr.ProofKind = "session"
						}
						if err := j.append(jr); err != nil {
							t.Fatal(err)
						}
					}
					// Another member of the original batch can conclude its
					// old receipt after this message's recovery was accepted.
					late := inboundJournalRecord{Channel: "C1", MessageTS: "100.1", ReceiptID: "original", Status: "failed", Phase: "concluded", TargetSessions: []string{"s1"}}
					if alias {
						late.ProofKind = "session"
					}
					if err := j.append(late); err != nil {
						t.Fatal(err)
					}
					if err := r.enrichFromJournal(j.dir); err != nil {
						t.Fatal(err)
					}
				}
				polled := ""
				r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
					polled = id
					return receiptPollPending, deliveryReceipt{}, nil
				}
				r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
					t.Fatal("reposted uncertain legacy delivery")
					return deliveryReceipt{}, nil
				}
				r.tick(true)
				want := ""
				if journal {
					want = "current"
				}
				if polled != want {
					t.Fatalf("polled=%q want=%q", polled, want)
				}
			})
		}
	}
}

func TestInboundRecoveryCompactsCompletedPayloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	r, _ := newInboundRecovery(path, filepath.Join(dir, "dead"))
	p := testPending("C1", "100.1", strings.Repeat("payload", 1<<17))
	p.files = "file description"
	receipt := recoveryReceipt("first", "s1", false)
	receipt.submission = captureRecoverySubmission(p.inbound)
	if err := r.hold(p, receipt, false); err != nil {
		t.Fatal(err)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return recoveryReceipt("done", "s1", true), nil }
	r.now = func() time.Time { return time.Now().Add(time.Minute) }
	r.tick(false)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4096 {
		t.Fatalf("completed payload retained: %d bytes", info.Size())
	}
	r2, err := newInboundRecovery(path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.known("C1", "100.1") || r2.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
		t.Fatal("compaction lost durable dedup")
	}
}

func TestInboundRecoveryStreamsLargeHistoryAndState(t *testing.T) {
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead")
	if err := os.MkdirAll(dead, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dead, "C1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rec := inboundDeadLetterRecord{Channel: "C1", Inbound: testPending("C1", "100.1", strings.Repeat("a", 1<<20)).inbound, DeadLetteredAt: time.Now(), ReplayedAt: time.Now()}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	for i := 0; i < 65; i++ {
		if _, err := file.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ := newInboundRecovery("", dead)
	if err := r.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if len(r.state.Entries) != 1 || r.state.Entries["C1|100.1"].Inbound.Text != "" {
		t.Fatal("history not folded and compacted")
	}
	path := filepath.Join(dir, "state.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"entries":{},"notices":{}`)); err != nil {
		t.Fatal(err)
	}
	padding := bytes.Repeat([]byte(" "), 1<<20)
	for i := 0; i < 65; i++ {
		if _, err := f.Write(padding); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Write([]byte("}")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newInboundRecovery(path, dead); err != nil {
		t.Fatal(err)
	}
}

func TestInboundRecoveryTornHistoryDoesNotDisableStartup(t *testing.T) {
	dir := t.TempDir()
	r, _ := newInboundRecovery(filepath.Join(dir, "state.json"), filepath.Join(dir, "dead"))
	p := testPending("C1", "100.1", "held")
	if err := r.hold(p, recoveryReceipt("first", "s1", false), false); err != nil {
		t.Fatal(err)
	}
	e := r.state.Entries["C1|100.1"]
	e.Retiring = true
	e.DeadLetteredAt = time.Now()
	r.put("C1|100.1", e)
	if err := os.MkdirAll(r.deadDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.deadDir, "C1.jsonl")
	if err := os.WriteFile(path, []byte(`{"torn":`), 0600); err != nil {
		t.Fatal(err)
	}
	r2, err := newInboundRecovery(r.path, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	r2.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return deliveryReceipt{}, errors.New("offline") }
	r2.retire("C1|100.1", r2.state.Entries["C1|100.1"])
	r3, err := newInboundRecovery("", r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r3.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	if r3.counts()["C1"].Dead != 1 {
		t.Fatalf("retirement lost behind torn tail: %+v", r3.state)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("{\"torn\":\n")) {
		t.Fatal("damaged bytes not retained separately")
	}
}

func TestInboundRecoveryNoRouteRemainsOutstanding(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	if err := r.hold(testPending("C1", "100.1", "missed"), recoveryReceipt("original", "s1", false), false); err != nil {
		t.Fatal(err)
	}
	e := r.state.Entries["C1|100.1"]
	e.DeadLetteredAt = now
	e.NextAttemptAt = now
	r.put("C1|100.1", e)
	if err := r.writeDead(e, time.Time{}); err != nil {
		t.Fatal(err)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		return deliveryReceipt{present: true, id: "unroutable", status: "no_route"}, nil
	}
	r.tick(true)
	if r.counts()["C1"].Dead != 1 || !r.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
		t.Fatal("unroutable replay falsely completed")
	}
	now = now.Add(2 * time.Minute)
	r.observe(recoveryReceipt("fresh", "s1", true))
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		return recoveryReceipt("delivered", "s1", true), nil
	}
	r.tick(false)
	if r.counts()["C1"].Dead != 0 || r.state.Entries["C1|100.1"].DeliveredAt.IsZero() {
		t.Fatal("restored route did not recover the message")
	}
}

func TestInboundRecoveryWriteFailureKeepsPendingOwnership(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		posts++
		fmt.Fprint(w, `{"delivery":{"receipt_id":"pending-original","status":"pending"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	r := cfg.inboundRecovery
	good := r.path
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	r.path = filepath.Join(blocker, "state.json")
	cfg.coalescer.enqueue("C1", testPending("C1", "100.1", "held"))
	cfg.coalescer.flushAheadOf("C1", "")
	cfg.coalescer.flushAheadOf("C1", "")
	if posts != 1 || !r.dirty || !r.state.Entries["C1|100.1"].PendingReceipt {
		t.Fatalf("pending ownership lost: posts=%d dirty=%t", posts, r.dirty)
	}
	polled := ""
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		polled = id
		return receiptPollPending, deliveryReceipt{}, nil
	}
	r.now = func() time.Time { return time.Now().Add(time.Minute) }
	r.tick(false)
	if polled != "pending-original" || posts != 1 {
		t.Fatalf("polled=%s posts=%d", polled, posts)
	}
	r.path = good
	r.tick(false)
	restarted, err := newInboundRecovery(good, r.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.state.Entries["C1|100.1"].PendingReceipt || restarted.state.Entries["C1|100.1"].PendingReceiptID != "pending-original" {
		t.Fatal("persistence retry lost receipt")
	}
}

func TestInboundRecoveryTrustedPathsSurviveRestartAndHistory(t *testing.T) {
	// Synthetic redaction fixture: never a literal credential in source.
	filePath := filepath.Join(t.TempDir(), "xo"+"xb-"+strings.Repeat("1", 12)+"-"+strings.Repeat("2", 12)+"-"+strings.Repeat("a", 32)+".txt")
	if redactInboundText(filePath) == filePath {
		t.Fatal("fixture must need a trusted path exemption")
	}
	if err := os.WriteFile(filePath, []byte("attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	var posted externalInboundMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Message externalInboundMessage `json:"message"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posted = body.Message
		fmt.Fprint(w, `{"delivery":{"receipt_id":"recovered","status":"delivered"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	p := testPending("C1", "100.1", "file payload")
	p.inbound.Attachments = []externalAttachment{{URL: "file://" + filePath}}
	p = withholdAttachments(p, permanent422())
	receipt := recoveryReceipt("failed", "s1", false)
	receipt.submission = captureRecoverySubmission(p.inbound)
	if err := cfg.inboundRecovery.hold(p, receipt, false); err != nil {
		t.Fatal(err)
	}
	e := cfg.inboundRecovery.state.Entries["C1|100.1"]
	e.DeadLetteredAt = time.Now()
	if err := cfg.inboundRecovery.writeDead(e, time.Time{}); err != nil {
		t.Fatal(err)
	}
	history, err := newInboundRecovery("", cfg.inboundRecovery.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := history.importDeadLetters(); err != nil {
		t.Fatal(err)
	}
	loaded := history.state.Entries["C1|100.1"]
	if len(loaded.TrustedFilePaths) != 1 || !strings.Contains(redactInboundMessage(loaded.Submission.message()).Text, filePath) {
		t.Fatal("history lost trusted path metadata")
	}
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.inboundRecovery.tick(true)
	if !strings.Contains(posted.Text, filePath) || len(posted.Attachments) != 0 {
		t.Fatal("restart corrupted the withheld attachment path or reattached it")
	}
}

func TestInboundRecoveryDeadPendingPollsAndResumesFollowers(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	for _, ts := range []string{"100.1", "100.2"} {
		if err := r.hold(testPending("C1", ts, "dead"), recoveryReceipt("old-"+ts, "s1", false), false); err != nil {
			t.Fatal(err)
		}
		e := r.state.Entries["C1|"+ts]
		e.DeadLetteredAt = now
		e.NextAttemptAt = time.Time{}
		r.put("C1|"+ts, e)
	}
	var sent []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		sent = append(sent, e.Inbound.ProviderMessageID)
		if len(sent) == 1 {
			return deliveryReceipt{present: true, id: "pending-replay", status: "pending"}, nil
		}
		return recoveryReceipt("done", "s1", true), nil
	}
	polls := 0
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		polls++
		if id != "pending-replay" {
			t.Fatalf("polled=%s", id)
		}
		if polls == 1 {
			return receiptPollPending, deliveryReceipt{}, nil
		}
		return receiptPollConcluded, recoveryReceipt(id, "s1", true), nil
	}
	r.tick(true)
	now = now.Add(time.Minute)
	r.tick(false)
	if polls != 1 || len(sent) != 1 {
		t.Fatalf("pending replay stopped polling or follower passed it: polls=%d sent=%v", polls, sent)
	}
	now = now.Add(2 * time.Minute)
	r.tick(false)
	if polls != 2 || len(sent) != 2 || sent[1] != "100.2" || r.counts()["C1"].Dead != 0 {
		t.Fatalf("completion did not resume selected followers: polls=%d sent=%v counts=%v", polls, sent, r.counts())
	}
}

func TestInboundRecoveryDeletionDuringInitialSendSurvivesRestart(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var posts []externalInboundMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Message externalInboundMessage `json:"message"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		posts = append(posts, body.Message)
		first := len(posts) == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
			fmt.Fprint(w, `{"delivery":{"receipt_id":"first","status":"held"}}`)
			return
		}
		fmt.Fprint(w, `{"delivery":{"receipt_id":"retry","status":"delivered"}}`)
	}))
	defer srv.Close()
	cfg := recoveryTestConfig(t, srv.URL)
	done := make(chan struct{})
	go func() {
		env := botMentionEnvelope(t, "message", "delete-race", "C1", "100.1", "", "<@"+testBotUserID+"> original-to-delete", true)
		processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
		close(done)
	}()
	<-started
	cfg.coalescer.markDeleted("C1", "100.1")
	close(release)
	<-done
	loaded, err := newInboundRecovery(cfg.inboundRecovery.path, cfg.inboundRecovery.deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.state.Entries["C1|100.1"].Inbound.Text != deletedBySenderNotice {
		t.Fatal("deletion was absent from durable hold admission")
	}
	cfg.coalescer.mu.Lock()
	cfg.coalescer.deleted = nil
	cfg.coalescer.mu.Unlock()
	if err := installInboundRecovery(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.inboundRecovery.now = func() time.Time { return time.Now().Add(time.Minute) }
	cfg.inboundRecovery.tick(false)
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 2 || posts[1].Text != deletedBySenderNotice || len(posts[1].Attachments) != 0 {
		t.Fatalf("deleted content replayed after restart: posts=%d", len(posts))
	}
}

func TestInboundRecoveryIncompleteJournalPreservesUncertainty(t *testing.T) {
	for _, mode := range []string{"malformed", "over-budget", "missing"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			r, err := newInboundRecovery(filepath.Join(dir, "state.json"), filepath.Join(dir, "dead"))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.hold(testPending("C1", "100.1", "legacy"), recoveryReceipt("original", "s1", false), false); err != nil {
				t.Fatal(err)
			}
			e := r.state.Entries["C1|100.1"]
			e.LegacyUncertain = true
			e.PendingReceipt = true
			e.PendingReceiptID = "previous-scan"
			e.DeadLetteredAt = time.Now()
			r.put("C1|100.1", e)
			journal := filepath.Join(dir, "journal")
			if mode != "missing" {
				if err := os.MkdirAll(journal, 0700); err != nil {
					t.Fatal(err)
				}
				rec := inboundJournalRecord{Channel: "C1", MessageTS: "100.1", ReceiptID: "obsolete", Status: "failed", Phase: "accepted"}
				data, err := json.Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(journal, "a.jsonl"), append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				last, err := os.Create(filepath.Join(journal, "z.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "over-budget" {
					err = last.Truncate(64 << 20)
				} else {
					_, err = last.Write([]byte("{"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := last.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err = r.enrichFromJournal(journal)
			if mode != "missing" && err == nil {
				t.Fatal("incomplete scan was accepted")
			}
			if got := r.state.Entries["C1|100.1"]; !got.PendingReceipt || got.PendingReceiptID != "" {
				t.Fatalf("partial selection became authoritative: pending=%t id=%s", got.PendingReceipt, got.PendingReceiptID)
			}
			restarted, err := newInboundRecovery(r.path, r.deadDir)
			if err != nil {
				t.Fatal(err)
			}
			restarted.poll = func(context.Context, string) (receiptPollState, deliveryReceipt, error) {
				t.Fatal("polled an obsolete receipt from an incomplete scan")
				return receiptPollUnknown, deliveryReceipt{}, nil
			}
			restarted.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
				t.Fatal("reposted uncertain legacy payload")
				return deliveryReceipt{}, nil
			}
			restarted.tick(true)
		})
	}
}

func TestInboundRecoveryFIFOAllowsPollingAndDeadlines(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	for i, id := range []string{"", "good", "lost", "unvouched"} {
		receipt := deliveryReceipt{present: true, id: id, status: "pending"}
		if id == "unvouched" {
			receipt.status = "failed"
		}
		if err := r.hold(testPending("C1", fmt.Sprintf("100.%d", i+1), "held"), receipt, false); err != nil {
			t.Fatal(err)
		}
	}
	var polled []string
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		polled = append(polled, id)
		return receiptPollConcluded, recoveryReceipt(id, "s1", id == "good"), nil
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) {
		t.Fatal("new send passed the older unknown receipt")
		return deliveryReceipt{}, nil
	}
	now = now.Add(time.Minute)
	r.tick(false)
	if len(polled) != 2 || r.state.Entries["C1|100.2"].DeliveredAt.IsZero() || r.state.Entries["C1|100.3"].PendingReceipt {
		t.Fatalf("FIFO blocked independent receipt settlement: polls=%v", polled)
	}
	now = now.Add(30 * time.Minute)
	r.tick(false)
	if r.counts()["C1"].Dead != 3 {
		t.Fatalf("blocked holds skipped their deadlines: %v", r.counts())
	}
	old := r.state.Entries["C1|100.1"]
	old.DeliveredAt = now
	r.put("C1|100.1", old)
	r.manual["C1"] = true
	var sent []string
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		sent = append(sent, e.Inbound.ProviderMessageID)
		return recoveryReceipt("done", "s1", true), nil
	}
	r.tick(false)
	if len(sent) != 2 || sent[0] != "100.3" || sent[1] != "100.4" {
		t.Fatalf("resolved losses did not resume in order: %v", sent)
	}
}

func TestInboundRecoveryPauseDoesNotCancelPendingReceipts(t *testing.T) {
	now := time.Now()
	r, _ := newInboundRecovery("", t.TempDir())
	r.now = func() time.Time { return now }
	for i := 1; i <= 2; i++ {
		receipt := recoveryReceipt("old", "s1", false)
		if i == 2 {
			receipt.status = "pending"
			receipt.id = "already-submitted"
		}
		ts := fmt.Sprintf("100.%d", i)
		if err := r.hold(testPending("C1", ts, "dead"), receipt, false); err != nil {
			t.Fatal(err)
		}
		e := r.state.Entries["C1|"+ts]
		e.DeadLetteredAt = now
		e.NextAttemptAt = now
		r.put("C1|"+ts, e)
	}
	r.send = func(inboundRecoveryEntry) (deliveryReceipt, error) { return deliveryReceipt{}, errors.New("offline") }
	polled := false
	r.poll = func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		polled = true
		return receiptPollConcluded, recoveryReceipt(id, "s1", true), nil
	}
	r.tick(true)
	if !polled || r.state.Entries["C1|100.2"].DeliveredAt.IsZero() {
		t.Fatal("an older replay failure cancelled a later receipt poll")
	}
}
