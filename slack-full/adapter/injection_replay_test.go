package main

import (
	"bufio"
	"encoding/json"
	"os"
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
	counts := map[string]int{"blocks": 0, "members": 0, "minimal_bytes": 0}
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
				IsBot                    bool `json:"is_bot"`
			}
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal("invalid replay row")
		}
		var texts []string
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
		bytes := len(row.Head + strings.Join(texts, "\n\n") + row.Foot)
		counts["minimal_bytes"] += bytes
		if daily[row.Day] == nil {
			daily[row.Day] = map[string]int{}
		}
		daily[row.Day]["minimal_bytes"] += bytes
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
