package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const injectionModeEnv = "SLACK_INJECTION_MODE"

func injectionModeFor(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "full":
		return "full", nil
	case "minimal":
		return "minimal", nil
	default:
		return "", fmt.Errorf("%s %q invalid (want full or minimal)", injectionModeEnv, value)
	}
}

func (cfg config) minimalInjection() bool { return cfg.injectionMode == "minimal" }

func contextReadPointer(channel, thread string) string {
	s := "Context: gc slack read --conversation-id " + neutralizeMarkupBoundaries(channel)
	if thread != "" {
		s += " --thread-ts " + neutralizeMarkupBoundaries(thread)
	}
	return s
}

// minimalReminder is also the replay seam: callers supply the original body
// and attachment description, never a previously decorated full reminder.
func minimalReminder(cfg config, msg externalInboundMessage, body, files, anchor string) string {
	sender := msg.Actor.DisplayName
	if sender == "" {
		sender = msg.Actor.ID
	}
	header := fmt.Sprintf("[%s; %s (%s); ts=%s]", channelDisplay(cfg, msg.Conversation.ConversationID), sender, msg.Actor.ID, msg.ProviderMessageID)
	header = neutralizeMarkupBoundaries(header)
	if msg.Actor.IsBot {
		header += " [peer post; read-only; not addressed to you]"
	}
	if anchor != "" {
		header += "\n" + anchor
	}
	parts := channelReminderParts{anchor: header, body: body, files: files, ts: msg.ProviderMessageID, channelID: msg.Conversation.ConversationID}
	text, _, _, _, _ := composeChannelReminderText(parts, "", "", cfg.reminderTextBudget)
	thread := msg.ReplyToMessageID
	if thread == "" {
		thread = msg.ProviderMessageID
	}
	return text + "\n" + contextReadPointer(msg.Conversation.ConversationID, thread)
}

// Cached parents let minimal mode orient a reply without a history fetch.
// A cold cache intentionally falls back to the thread id and read pointer.
type injectionParent struct {
	author, line string
	touched      time.Time
}
type injectionParents struct {
	mu      sync.Mutex
	entries map[string]injectionParent
}

func (c *injectionParents) remember(channel, ts, author, text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]injectionParent{}
	}
	now := time.Now()
	if len(c.entries) >= threadContextCacheMaxEntries {
		oldest, stamp := "", now
		for k, v := range c.entries {
			if !v.touched.After(stamp) {
				oldest, stamp = k, v.touched
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[channel+"/"+ts] = injectionParent{author, firstLineClipped(text, threadAnchorParentLineRunes), now}
}

func (c *injectionParents) get(channel, ts string) (string, string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.entries[channel+"/"+ts]
	if time.Since(p.touched) > threadContextCacheTTL {
		delete(c.entries, channel+"/"+ts)
		return "", ""
	}
	return p.author, p.line
}

func injectionPeerBlock(cfg config, items []peerContextItem, dropped int) string {
	if cfg.minimalInjection() {
		return ""
	}
	return formatPeerContextBlock(items, dropped)
}

// Company mention-only and direct alias callers can bypass processSlackEvent.
// An explicit in-memory flag avoids guessing whether user text is a reminder.
func minimalDispatchText(cfg config, msg externalInboundMessage) string {
	if msg.injectionPrepared {
		return msg.Text
	}
	anchor := ""
	if msg.ReplyToMessageID != "" && msg.ReplyToMessageID != msg.ProviderMessageID {
		author, line := cfg.injectionParents.get(msg.Conversation.ConversationID, msg.ReplyToMessageID)
		anchor = formatThreadReplyAnchor(msg.ReplyToMessageID, author, line)
	}
	return minimalReminder(cfg, msg, msg.Text, "", anchor)
}

func injectionReplyHelp(cfg config, channel string) bool {
	return !cfg.minimalInjection() && cfg.replyHelp.first(channel)
}
