package main

import (
	"encoding/json"
	"fmt"
	"log"
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
	if anchor != "" {
		header += "\n" + anchor
	}
	parts := channelReminderParts{anchor: header, body: body, files: files, ts: msg.ProviderMessageID, channelID: msg.Conversation.ConversationID}
	thread := msg.ReplyToMessageID
	if thread == "" && !(msg.Actor.IsBot && strings.HasPrefix(msg.Actor.DisplayName, reactionActorPrefix)) {
		thread = msg.ProviderMessageID
	}
	pointer := "\n" + contextReadPointer(msg.Conversation.ConversationID, thread)
	budget := cfg.reminderTextBudget
	if budget > 0 {
		// Reserve the mandatory read pointer before trimming the body. Keep
		// a positive budget even when the pointer alone exceeds the limit:
		// zero disables trimming, while protected payload may overflow.
		budget = max(1, budget-len(pointer))
	}
	text, _, _, _, _ := composeChannelReminderText(parts, "", "", budget)
	return text + pointer
}

// Cached parents let minimal mode orient a reply without a history fetch.
// A cold cache intentionally falls back to the thread id and read pointer.
type injectionParent struct {
	author, line string
	touched      time.Time
	invalidated  bool
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
	key := channel + "/" + ts
	if p := c.entries[key]; p.invalidated && time.Since(p.touched) <= threadContextCacheTTL {
		return
	}
	c.putLocked(key, injectionParent{author: author, line: firstLineClipped(text, threadAnchorParentLineRunes), touched: time.Now()})
}

func (c *injectionParents) putLocked(key string, p injectionParent) {
	if c.entries == nil {
		c.entries = map[string]injectionParent{}
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= threadContextCacheMaxEntries {
		oldest, stamp := "", time.Now()
		for k, v := range c.entries {
			if !v.touched.After(stamp) {
				oldest, stamp = k, v.touched
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = p
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

// Edits and deletions invalidate the old excerpt; a later reply falls back
// to its thread id, without fetching history. A bounded tombstone prevents
// an older root handler from restoring the excerpt after concurrent mutation.
func (c *injectionParents) invalidateMutation(msg slackMessageEvent, raw json.RawMessage) {
	if c == nil {
		return
	}
	ts := msg.DeletedTS
	if msg.Subtype == "message_changed" {
		var changed struct {
			Message struct {
				TS string `json:"ts"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &changed) != nil {
			return
		}
		ts = changed.Message.TS
	} else if msg.Subtype != "message_deleted" {
		return
	}
	if msg.Channel == "" || ts == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(msg.Channel+"/"+ts, injectionParent{touched: time.Now(), invalidated: true})
}

// peerRecovery includes whole failed posts in FIFO order in both modes.
// Deferred entries are retained before returning; the caller restores the
// included prefix if transport fails. A count is always visible when pending
// messages cannot fit. The first queued post is protected like the new message:
// it rides whole even over budget, so a fixed limit cannot strand the FIFO.
func peerRecovery(cfg config, channel, text string, items []peerContextItem) (string, []peerContextItem) {
	notice := func(n int) string {
		if n == 0 {
			return ""
		}
		return fmt.Sprintf("\n[%d omitted — awaiting delivery]", n)
	}
	used := 0
	result := text + notice(len(items))
	for i := range items {
		candidate := formatPeerContextBlock(items[:i+1], 0) + "\n\n" + text + notice(len(items)-i-1)
		if cfg.reminderTextBudget > 0 && len(candidate) > cfg.reminderTextBudget {
			if used > 0 {
				break
			}
			log.Printf("peer-bot: oversized recovery head chan=%s item_bytes=%d budget=%d — carrying whole",
				channel, len(formatPeerContextBlock(items[:1], 0)), cfg.reminderTextBudget)
		}
		result, used = candidate, i+1
	}
	cfg.peerContext.restore(channel, items[used:], 0)
	return result, items[:used]
}

// Company mention-only and direct alias callers can bypass processSlackEvent.
// An explicit in-memory flag avoids guessing whether user text is a reminder.
func minimalDispatchText(cfg config, msg externalInboundMessage) string {
	if msg.injectionPrepared {
		return msg.Text
	}
	return minimalDispatchParts(cfg, msg, "")
}

func minimalDispatchParts(cfg config, msg externalInboundMessage, files string) string {
	anchor := ""
	if msg.ReplyToMessageID != "" && msg.ReplyToMessageID != msg.ProviderMessageID {
		author, line := cfg.injectionParents.get(msg.Conversation.ConversationID, msg.ReplyToMessageID)
		anchor = formatThreadReplyAnchor(msg.ReplyToMessageID, author, line)
	}
	return minimalReminder(cfg, msg, msg.Text, files, anchor)
}

func injectionReplyHelp(cfg config, channel string) bool {
	return !cfg.minimalInjection() && cfg.replyHelp.first(channel)
}
