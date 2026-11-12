package main

import "github.com/sjarmak/gc-slack-adapter/internal/stopwatch"

// Slack's accepted timestamp measures words even when the independent
// ingestion proof cannot read history. Replays keep that original timestamp;
// the stopwatch deduplicates the post across retry and restart.
func recordAcceptedSlackWords(cfg config, channel, thread, message string) {
	if cfg.inboundJournal == nil {
		return
	}
	if at, ok := ingestionPublishTime(message); ok {
		cfg.inboundJournal.watch.Record(stopwatch.Event{Event: "words", Conversation: channel, Message: message, Thread: thread, At: at})
	}
}
