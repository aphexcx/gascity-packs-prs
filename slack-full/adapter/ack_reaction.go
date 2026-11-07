package main

import (
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// sendAckReaction is the one configured acknowledgement path. The caller
// supplies the posting token: legacy/switchboard for rooms, owner for agent DMs.
// It is best effort; company receipts retain their durable delivery status.
func sendAckReaction(cfg config, client *http.Client, token string, msg slackMessageEvent) {
	if cfg.ackReaction == "" || token == "" || msg.User == "" || msg.BotID != "" ||
		(msg.Subtype != "" && msg.Subtype != "file_share") ||
		(strings.TrimSpace(msg.Text) == "" && len(msg.Files) == 0) {
		return
	}
	key := ackReceiptKey{msg.Channel, msg.TS}
	if !cfg.ackReceipts.claim(key, time.Now()) {
		return
	}
	defer func() { cfg.ackReceipts.finish(key, time.Now()) }()
	name := cfg.ackReaction
	switch name {
	case "auto":
		name = pickAckEmoji(msg.Text, len(msg.Files) > 0)
	case "model":
		if cfg.ackModelKeyFile == "" {
			name = pickAckEmoji(msg.Text, len(msg.Files) > 0)
		} else {
			cfg.slackBotToken = token // channel metadata has the same token custody
			name = pickAckEmojiModel(cfg, msg.Text, msg.Files, resolveChannelName(cfg, msg.Channel))
		}
	}
	resp, err := postReactionMethod(client, token, "reactions.add", slackReactionsAddReq{
		Channel: msg.Channel, Name: name, Timestamp: msg.TS,
	})
	if err != nil {
		log.Printf("ack reaction %s failed: chan=%s ts=%s: %v", name, msg.Channel, msg.TS, err)
	} else if !resp.OK && resp.Error != "already_reacted" {
		log.Printf("ack reaction %s failed: chan=%s ts=%s: slack error=%s", name, msg.Channel, msg.TS, resp.Error)
	}
}

// Instant acknowledgement reaction from the hou-dynasty local patch of
// 2026-10-30 and 2026-11-03, upstreamed by gp-n32w.
//
// The busy reaction (busy_reaction.go) only marks targeted inbounds, and
// ambient room chatter waits out the coalesce window before it reaches
// the agent, so a person writing in a bound room can wait minutes with
// no sign the message was seen. ACK_REACTION closes that gap: every
// human message gets a reaction the moment the event arrives, before
// routing. ACK_REACTION=<emoji> always uses that emoji; ACK_REACTION=auto
// picks one from the message text with the keyword table below (the
// Dynasty's reactions-first convention); unset or empty disables it.

type ackRule struct {
	emoji string
	re    *regexp.Regexp
}

// ackRules are checked in order; the first match wins. Social signals
// (thanks, good news, a bare yes) come before topics, and a question
// only wins when no topic matched.
var ackRules = []ackRule{
	{"pray", regexp.MustCompile(`\b(thanks|thank you|thx|ty|appreciate it)\b`)},
	{"tada", regexp.MustCompile(`\b(congrats|congratulations|great news|good news|we won|it worked|closed the deal)\b`)},
	{"white_check_mark", regexp.MustCompile(`^\s*(yes|yep|yeah|ok|okay|sure|go|go ahead|do it|approved|sounds good|perfect|agreed|confirmed)[.!]*\s*$`)},
	{"chart_with_upwards_trend", regexp.MustCompile(`\b(stocks?|trades?|trading|market|shares|options?|puts?|calls?|earnings|nasdaq|s&p|spy|qqq|hedge|polymarket|robinhood|portfolio|ticker|after hours|pre-?market)\b|\b(down|up) \d+(\.\d+)?%|\$[a-z]{1,5}\b`)},
	{"house", regexp.MustCompile(`\b(house|home|property|properties|listing|owner|realtor|har|mls|tax record|appraisal|closing|lot|offer on|mortgage|tenant|lease)\b|\b\d{2,6} [a-z]+ (st|street|ave|avenue|rd|road|dr|drive|way|ln|lane|blvd|ct|court|pl|place|cir)\b`)},
	{"moneybag", regexp.MustCompile(`\b(invoice|payment|paid|pay|wire|bank|deposit|price|cost|budget|refund|credits?|zelle)\b|\$\d`)},
	{"calendar", regexp.MustCompile(`\b(schedule|meeting|calendar|appointment|tomorrow|tonight|next week|reschedule|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b`)},
	{"wrench", regexp.MustCompile(`\b(fix|broken|bug|error|crash|outage|restart|install|setup|set up|maintenance|repair|not working)\b`)},
	{"page_facing_up", regexp.MustCompile(`\b(document|doc|pdf|contract|form|report|spreadsheet|sheet|draft|letter)\b`)},
	{"thinking_face", regexp.MustCompile(`\?|\b(why|how come|should we|what if|can you)\b`)},
}

// pickAckEmoji chooses the acknowledgement emoji for a message.
func pickAckEmoji(text string, hasFiles bool) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return "eyes" // a bare upload: seen, topic unknown
	}
	for _, r := range ackRules {
		if r.re.MatchString(t) {
			return r.emoji
		}
	}
	return "eyes"
}
