package main

import "github.com/sjarmak/gc-slack-adapter/internal/stopwatch"

// Company commands publish directly to Slack. Their existing authenticated
// ingress receipts are the confirmation path already used by reconciliation.
// Only an attributed company agent counts; arbitrary bot posts do not.
func (g *companyGateway) recordCompanyWords(msg CompanyMessage, author *CompanyAgent) {
	if g.cfg.inboundJournal == nil || author == nil || !isBotAuthored(msg) {
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
