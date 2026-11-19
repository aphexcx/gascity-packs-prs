package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
)

type stopwatchSlackFile struct {
	ID     string `json:"id"`
	Shares map[string]map[string][]struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"shares"`
}

func recordFileShares(cfg config, channel string, files []stopwatchSlackFile) bool {
	if cfg.inboundJournal == nil || cfg.inboundJournal.watch == nil {
		return true
	}
	found := false
	for _, file := range files {
		for _, channels := range file.Shares {
			for _, share := range channels[channel] {
				if at, ok := ingestionPublishTime(share.TS); ok {
					cfg.inboundJournal.watch.Record(stopwatch.Event{Event: "words", Conversation: channel, Message: share.TS, Thread: share.ThreadTS, At: at})
					found = true
				}
			}
		}
	}
	return found
}

var missingShareWarning sync.Once

// Slack may omit shares from completeUploadExternal. Read the posted file once
// for the channel's own timestamp, off the publish response path. An upload's
// creation time and our local clock are not substitutes for its post time.
func recordUploadedFile(cfg config, channel, id string, files []stopwatchSlackFile) {
	if recordFileShares(cfg, channel, files) {
		return
	}
	// With the acknowledgement off the adapter makes no Slack call the base
	// does not make: an upload whose answer has no shares gets no words line.
	if !cfg.arrivalChoice.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, slackAPIBase+"/files.info?file="+url.QueryEscape(id), nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+cfg.slackBotToken)
		var response *http.Response
		response, err = slackAPIClient.Do(req)
		if err == nil {
			defer response.Body.Close()
			var body struct {
				OK   bool               `json:"ok"`
				File stopwatchSlackFile `json:"file"`
			}
			if response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&body) == nil && body.OK && recordFileShares(cfg, channel, []stopwatchSlackFile{body.File}) {
				return
			}
		}
	}
	missingShareWarning.Do(func() { log.Print("stopwatch: Slack file post timestamp unavailable") })
}
