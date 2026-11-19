package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const compactFixtureText = "Please review the invented test message."

func compactFixtureMessage() externalInboundMessage {
	return externalInboundMessage{
		Conversation:      conversationRef{ConversationID: "C_EXAMPLE", Kind: "room"},
		Actor:             externalActor{ID: "U_EXAMPLE", DisplayName: "Example"},
		ProviderMessageID: "9000000000.000002",
		Text:              compactFixtureText,
	}
}

func TestMentionOnlyCompactDeliveryContract(t *testing.T) {
	for _, reason := range []string{mentionOnlyReasonBotMention, mentionOnlyReasonHandle, mentionOnlyReasonOwnThread} {
		for _, threaded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/threaded=%t", reason, threaded), func(t *testing.T) {
				msg := compactFixtureMessage()
				root := msg.ProviderMessageID
				if threaded {
					root = "9000000000.000001"
					msg.ReplyToMessageID = root
				}
				body := formatMentionOnlyReminder(config{}, msg, reason, "example")
				if overhead := len(body) - len(msg.Text); overhead > 400 {
					t.Errorf("non-message bytes = %d, want <=400", overhead)
				}
				for _, want := range []string{"Slack mention-only room delivery:", "Example (U_EXAMPLE)", "C_EXAMPLE", "ts=" + msg.ProviderMessageID, "thread=" + root, msg.Text} {
					if !strings.Contains(body, want) {
						t.Errorf("missing %q", want)
					}
				}
				var replyLines []string
				for _, line := range strings.Split(body, "\n") {
					if strings.HasPrefix(line, "Reply: ") {
						replyLines = append(replyLines, line)
					}
				}
				wantReply := "Reply: gc slack publish-to-channel --conversation-id C_EXAMPLE --thread-ts " + root + " --body-file <file>"
				if len(replyLines) != 1 || replyLines[0] != wantReply || len(replyLines[0]) > 160 {
					t.Errorf("want one <=160-character reply command, got %q", replyLines)
				}
				for _, repeated := range []string{"You are bound", "Read the rest", "gc slack read", "gc slack react", "registered identity", "once gc has confirmed", "you were @mentioned", "a reply landed"} {
					if strings.Contains(body, repeated) {
						t.Errorf("standing instruction or redundant reason remains: %q", repeated)
					}
				}
			})
		}
	}
}

func TestMentionOnlyCompactPreservesNewContextAndAttachments(t *testing.T) {
	msg := compactFixtureMessage()
	msg.Text = "Thread context (1 earlier message):\nExample: a synthetic parent\n---\n\n" + msg.Text
	msg.Attachments = []externalAttachment{{URL: "file:///tmp/example.txt", MIMEType: "text/plain"}}
	body := formatMentionOnlyReminder(config{}, msg, mentionOnlyReasonOwnThread, "example")
	if strings.Count(body, msg.Text) != 1 || !strings.Contains(body, "file:///tmp/example.txt") {
		t.Fatal("new thread context, message or attachment lost/duplicated")
	}
}

// Capture production renderer output on synthetic inputs before and after the
// change. Shared/DM Text is captured at the fake gc boundary; gc's own outer
// envelope is outside this repository and deliberately excluded from the table.
func TestMentionOnlyDeliveryMeasurements(t *testing.T) {
	if len(compactFixtureText) != 40 {
		t.Fatal("measurement fixture must be 40 ASCII bytes")
	}
	msg := compactFixtureMessage()
	rows := map[string]string{
		"mention": formatMentionOnlyReminder(config{}, msg, mentionOnlyReasonBotMention, "example"),
	}
	msg.ReplyToMessageID = "9000000000.000001"
	rows["own_thread"] = formatMentionOnlyReminder(config{}, msg, mentionOnlyReasonOwnThread, "example")
	for _, kind := range []string{"shared", "dm"} {
		capture := &inboundCapture{}
		gc := httptest.NewServer(capture.handler())
		t.Cleanup(gc.Close)
		cfg := coalescingTestConfig(gc.URL, 0)
		channel := "C_EXAMPLE"
		if kind == "dm" {
			channel = "D_EXAMPLE"
		}
		aliases := newTestHandleAliasRegistry(t)
		for i, temperature := range []string{"first", "subsequent"} {
			env := humanEnvelope(t, fmt.Sprintf("E_%s_%d", kind, i), channel, fmt.Sprintf("9000000000.%06d", i+2), compactFixtureText)
			processSlackEvent(cfg, aliases, nil, nil, nil, nil, env, func() {})
			messages := capture.snapshot()
			if len(messages) != i+1 {
				t.Fatalf("%s: got %d deliveries, want %d", kind, len(messages), i+1)
			}
			rows[kind+"_"+temperature] = messages[i].Text
		}
	}
	for name, body := range rows {
		if strings.Count(body, compactFixtureText) != 1 {
			t.Fatalf("%s: fixture lost or duplicated", name)
		}
		t.Logf("%s: message=40 other=%d total=%d", name, len(body)-40, len(body))
	}
	if path := os.Getenv("SLACK_DELIVERY_MEASURE_OUTPUT"); path != "" {
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
