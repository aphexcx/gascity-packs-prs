package main

import (
	"encoding/json"
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

func injectionPeerBlock(cfg config, items []peerContextItem, dropped int) string {
	if cfg.minimalInjection() {
		return ""
	}
	return formatPeerContextBlock(items, dropped)
}

// minimalPeerRecovery omits ambient context, but includes failed immediate
// forwards in FIFO order while the complete reminder fits its usual budget.
// Return only the included items so a failed delivery can restore them; any
// deferred items are restored here, including when the current body is large.
func minimalPeerRecovery(cfg config, channel, text string, items []peerContextItem) (string, []peerContextItem) {
	var failed []peerContextItem
	for _, item := range items {
		if item.FailedForward {
			failed = append(failed, item)
		}
	}
	result := text
	for i := range failed {
		candidate := formatPeerContextBlock(failed[:i+1], 0) + "\n\n" + text
		if cfg.reminderTextBudget > 0 && len(candidate) > cfg.reminderTextBudget {
			if i == 0 {
				// A post larger than the configured budget must not block
				// the queue forever. Deliver a bounded excerpt when there
				// is room for its provenance plus a useful text prefix.
				// Preserve the original item for any transport retry.
				item := failed[0]
				item.Text = ""
				const marker = " …[truncated; read channel]"
				room := cfg.reminderTextBudget - len(formatPeerContextBlock([]peerContextItem{item}, 0)+"\n\n"+text) - len(marker)
				// Leave the whole excerpt below the formatter's own cap so
				// it cannot clip again and append a second marker.
				room = min(room, maxPeerContextItemChars-len(marker))
				if room > 0 && room >= min(64, len(failed[0].Text)) {
					item.Text = clipRuneSafe(failed[0].Text, room) + marker
					candidate = formatPeerContextBlock([]peerContextItem{item}, 0) + "\n\n" + text
					if len(candidate) <= cfg.reminderTextBudget {
						cfg.peerContext.restore(channel, failed[1:], 0)
						return candidate, failed[:1]
					}
				}
			}
			cfg.peerContext.restore(channel, failed[i:], 0)
			return result, failed[:i]
		}
		result = candidate
	}
	return result, failed
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
