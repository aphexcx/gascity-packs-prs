package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/neversilent"
)

func (a *application) newNeverSilent() (*neversilent.Engine, error) {
	return neversilent.New(neversilent.Options{
		Enabled: a.cfg.NeverSilent.Enabled, Path: a.cfg.Endpoints.StateFile + ".never-silent.json",
		HealthFile: a.cfg.NeverSilent.HealthFile, City: a.cfg.Secrets.CityName, Channel: "gchat",
		OperatorContext: a.cfg.NeverSilent.OperatorContext, OperatorAgent: a.cfg.NeverSilent.OperatorAgent, GC: os.Getenv("GC_BIN"),
		Log: func(s string) { a.log.Warn(s) },
		SendLocal: func(ctx context.Context, m neversilent.Message, line string) error {
			return a.raiseDelivery(ctx, inbound.Raise{ID: "never-silent:" + m.ID, Space: m.Conversation, Thread: m.Thread, Message: m.ID, Recorded: m.Created,
				Body: fmt.Sprintf("The adapter told the owner: %q\nSpace: %s\nThread: %s\nMessage: %s\nRead: gc gchat read --space %s\nStatus: gc gchat status\n", line, m.Conversation, m.Thread, m.ID, m.Conversation)})
		},
		Post: func(ctx context.Context, m neversilent.Message, line string) error {
			return a.s.PostNoticeWithClient(ctx, a.chatClient.Load(), m, line)
		},
	})
}

// The watch owns one engine for the service lifetime. Registration, inbound
// retries, subscription recovery and credential generations cannot stall it.
func (a *application) pollNeverSilent(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := a.neverSilent.Poll(ctx); err != nil {
				a.log.Warn("never-silent poll failed", "error", err)
			}
		}
	}
}
