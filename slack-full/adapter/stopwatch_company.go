package main

import (
	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
)

// Company commands publish directly to Slack. Their existing authenticated
// ingress receipts are the confirmation path already used by reconciliation.
// Only an attributed company agent counts; arbitrary bot posts do not.
func (g *companyGateway) recordCompanyWords(msg CompanyMessage, author *CompanyAgent) {
	if g.cfg.inboundJournal == nil || author == nil || !isBotAuthored(msg) || isNeverSilentOwnerLine(msg.Text) {
		return
	}
	if msg.UserID != author.BotUserID && msg.ResolvedBotUserID != author.BotUserID {
		return
	}
	if msg.AppID != "" && msg.AppID != author.AppID {
		return
	}
	if msg.BotProfileAppID != "" && msg.BotProfileAppID != author.AppID {
		return
	}
	if at, ok := ingestionPublishTime(msg.TS); ok {
		g.cfg.inboundJournal.watch.Record(stopwatch.Event{Event: "words", Conversation: msg.ChannelID, Message: msg.TS, Thread: msg.ThreadTS, At: at})
	}
}

// A never-silent owner line on a company route is posted with the target
// agent's own bot token (never_silent.go), so its echo arrives here as that
// agent's bot post. It is the adapter's notice, never the session's words,
// whichever city's adapter posted it (PR 104 round 3, R5).
func isNeverSilentOwnerLine(text string) bool {
	switch text {
	case neversilent.DownLine, neversilent.NoSeatLine, neversilent.CapLine:
		return true
	}
	return false
}
