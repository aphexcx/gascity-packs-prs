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
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var row struct {
			Head, Foot string
			Units      []struct {
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
			counts["members"]++
		}
		counts["blocks"]++
		counts["minimal_bytes"] += len(row.Head + strings.Join(texts, "\n\n") + row.Foot)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(counts, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("INJECTION_REPLAY_REPORT"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
