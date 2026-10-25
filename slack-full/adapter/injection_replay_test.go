package main

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in offline measurement: raw inputs and rendered outputs never enter the
// test log. Only the aggregate report is written to the requested scratch path.
func TestMinimalReplayMeasurement(t *testing.T) {
	path := os.Getenv("INJECTION_REPLAY_INPUT")
	if path == "" {
		t.Skip("offline replay input not supplied")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	counts := map[string]int{"blocks": 0, "members": 0, "minimal_bytes": 0, "full_bytes": 0, "context_blocks": 0, "context_dropped_whole": 0, "context_partial": 0}
	daily := map[string]map[string]int{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var row struct {
			Head, Foot  string
			Day         string
			PeerArrival bool `json:"peer_arrival"`
			Units       []struct {
				Channel, Sender          string
				SenderID                 string `json:"sender_id"`
				TS, Thread, Body, Anchor string
				Preamble                 string
				IsBot                    bool `json:"is_bot"`
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal("invalid replay row")
		}
		var texts, fullTexts []string
		var fullBatch []pendingChannelInbound
		hasContext, droppedWhole, partial := false, false, false
		cfg := config{injectionMode: "minimal", reminderTextBudget: defaultReminderTextBudget}
		if len(row.Units) > 1 {
			cfg.reminderTextBudget = 0
		}
		for _, u := range row.Units {
			m := externalInboundMessage{Conversation: conversationRef{ConversationID: u.Channel}, Actor: externalActor{ID: u.SenderID, DisplayName: u.Sender, IsBot: u.IsBot}, ProviderMessageID: u.TS, ReplyToMessageID: u.Thread}
			anchor := u.Anchor
			if anchor == "" && u.Thread != "" && u.Thread != u.TS {
				anchor = formatThreadReplyAnchor(u.Thread, "", "")
			}
			texts = append(texts, minimalReminder(cfg, m, u.Body, "", anchor))
			if len(row.Units) > 1 {
				// Production batches carry thread coordinates in each member
				// prefix, without the single-message reply anchor.
				anchor = ""
			}
			full, _, _, usedPreamble, _ := composeChannelReminderText(channelReminderParts{anchor: anchor, preamble: u.Preamble, body: u.Body}, "", "", cfg.reminderTextBudget)
			fullTexts = append(fullTexts, full)
			m.Text = full
			fullBatch = append(fullBatch, pendingChannelInbound{inbound: m})
			if u.Preamble != "" {
				hasContext = true
				// Inspect only the composed context, never matching user body text.
				context := strings.TrimSuffix(full, u.Body)
				context = strings.TrimPrefix(context, anchor+"\n")
				whole, fitted := replayContextOutcome(context, usedPreamble)
				droppedWhole = droppedWhole || whole
				partial = partial || fitted
			}
			if row.PeerArrival {
				full, _, _, _, _ := composeChannelReminderText(channelReminderParts{anchor: anchor, body: u.Body}, "", "", defaultReminderTextBudget)
				reply := u.Thread
				if reply == "" {
					reply = u.TS
				}
				foot := strings.Replace(row.Foot, "\nreply-to "+reply+"\n", "\nReply: gc slack reply-current --conversation-id "+u.Channel+" --reply-to "+reply+" --body-file <file>\n", 1)
				counts["peer_full_bytes"] += len(row.Head + full + foot)
			}
			counts["members"]++
		}
		counts["blocks"]++
		if hasContext {
			counts["context_blocks"]++
		}
		if droppedWhole {
			counts["context_dropped_whole"]++
		}
		if partial {
			counts["context_partial"]++
		}
		last := row.Units[len(row.Units)-1]
		reply := last.Thread
		if reply == "" {
			reply = last.TS
		}
		fullFoot := strings.Replace(row.Foot, "\nreply-to "+reply+"\n", "\nReply: gc slack reply-current --conversation-id "+last.Channel+" --reply-to "+reply+" --body-file <file>\n", 1)
		fullText := strings.Join(fullTexts, "\n\n")
		if len(fullBatch) > 1 {
			fullText = formatCoalescedBlock(config{}, last.Channel, fullBatch)
		}
		fullBytes := len(row.Head + fullText + fullFoot)
		counts["full_bytes"] += fullBytes
		bytes := len(row.Head + strings.Join(texts, "\n\n") + row.Foot)
		counts["minimal_bytes"] += bytes
		if daily[row.Day] == nil {
			daily[row.Day] = map[string]int{}
		}
		daily[row.Day]["minimal_bytes"] += bytes
		daily[row.Day]["full_bytes"] += fullBytes
		if row.PeerArrival {
			counts["peer_minimal_bytes"] += bytes
			daily[row.Day]["peer_minimal_bytes"] += bytes
			daily[row.Day]["peer_arrivals"]++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"totals": counts, "daily": daily}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("INJECTION_REPLAY_REPORT"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func replayContextOutcome(context string, unchanged bool) (whole, partial bool) {
	if unchanged {
		return false, false
	}
	if strings.Contains(context, "… [truncated]") {
		return false, true
	}
	if strings.Contains(context, "omitted — thread context over budget]") {
		if strings.HasPrefix(strings.TrimSpace(context), "[") && !strings.Contains(context, "\n@") {
			return true, false
		}
		return false, true
	}
	return false, false
}

func TestReplayContextOutcome(t *testing.T) {
	parts := channelReminderParts{preamble: "Thread context (1 earlier message):\n@robot: " + strings.Repeat("界", 100) + "\n\n---\n\n", body: "new post"}
	full, _, _, used, _ := composeChannelReminderText(parts, "", "", 100)
	context := strings.TrimSuffix(full, parts.body)
	if whole, partial := replayContextOutcome(context, used); whole || !partial {
		t.Fatal("clipped lone quote not counted as partial")
	}
	if whole, partial := replayContextOutcome(contextOmissionNotice(2), false); !whole || partial {
		t.Fatal("baseline whole-context omission not counted")
	}
	if whole, partial := replayContextOutcome("@human: … [truncated]\n", true); whole || partial {
		t.Fatal("unchanged quoted text mistaken for fitter output")
	}
}

func TestReplayThreadedBatchParity(t *testing.T) {
	const head = "synthetic envelope\n"
	const foot = "\nreply-to 100.1\n"
	var batch []pendingChannelInbound
	var units []map[string]any
	for _, ts := range []string{"100.2", "100.3"} {
		p := pendingChannelInbound{threadAnchor: "[reply in thread 100.1]", preamble: "@prior: synthetic context\n\n", body: "synthetic body"}
		p.inbound = externalInboundMessage{Conversation: conversationRef{ConversationID: "C_TEST"}, ProviderMessageID: ts, ReplyToMessageID: "100.1", Actor: externalActor{ID: "U_TEST", DisplayName: "Example"}, Text: p.foldedText()}
		batch = append(batch, p)
		units = append(units, map[string]any{"channel": "C_TEST", "sender": "Example", "sender_id": "U_TEST", "ts": ts, "thread": "100.1", "body": p.body, "preamble": p.preamble, "anchor": p.threadAnchor})
	}
	input, err := json.Marshal(map[string]any{"head": head, "foot": foot, "units": units})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	inputPath, reportPath := filepath.Join(dir, "input.jsonl"), filepath.Join(dir, "report.json")
	if err := os.WriteFile(inputPath, append(input, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INJECTION_REPLAY_INPUT", inputPath)
	t.Setenv("INJECTION_REPLAY_REPORT", reportPath)
	TestMinimalReplayMeasurement(t)
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report struct{ Totals map[string]int }
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	capture := &inboundCapture{}
	server := httptest.NewServer(capture.handler())
	defer server.Close()
	cfg := coalescingTestConfig(server.URL, 0)
	cfg.replyHelp.first("C_TEST")
	if err := deliverCoalescedBatch(cfg, "C_TEST", batch); err != nil {
		t.Fatal(err)
	}
	got := capture.snapshot()
	if len(got) != 1 {
		t.Fatalf("deliveries=%d", len(got))
	}
	fullFoot := "\nReply: gc slack reply-current --conversation-id C_TEST --reply-to 100.1 --body-file <file>\n"
	wantBytes := len(head + got[0].Text + fullFoot)
	if strings.Contains(got[0].Text, "[reply in thread") || report.Totals["full_bytes"] != wantBytes {
		t.Fatalf("batch projection=%d, delivered=%d", report.Totals["full_bytes"], wantBytes)
	}
}
