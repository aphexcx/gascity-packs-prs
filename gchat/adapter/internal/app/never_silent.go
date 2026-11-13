package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/token"
)

// Keep each client paired with the credential it actually uses, including
// across settings reloads. A client object alone is not permission to post.
type noticeProvider struct {
	client *chat.Client
	source *token.Source
}

func (a *application) ownerNoticeClient() *chat.Client {
	p := a.noticeProvider.Load()
	if p == nil || a.credentialHold.Load() || p.source.State() != token.Ready {
		return nil
	}
	return p.client
}

func (a *application) newNeverSilent() (*neversilent.Engine, error) {
	return neversilent.New(neversilent.Options{
		Enabled: a.cfg.NeverSilent.Enabled, Path: a.cfg.Endpoints.StateFile + ".never-silent.json",
		HealthFile: a.cfg.NeverSilent.HealthFile, City: a.cfg.Secrets.CityName, Channel: "gchat",
		OperatorContext: a.cfg.NeverSilent.OperatorContext, OperatorAgent: a.cfg.NeverSilent.OperatorAgent, GC: os.Getenv("GC_BIN"),
		Log:        func(s string) { a.log.Warn(s) },
		Guard:      a.checkStateLock,
		OwnerReady: func() bool { return a.ownerNoticeClient() != nil },
		SendLocal: func(ctx context.Context, m neversilent.Message, line string) error {
			return a.raiseDelivery(ctx, inbound.Raise{ID: "never-silent:" + m.ID, Space: m.Conversation, Thread: m.Thread, Message: m.ID, Recorded: m.Created,
				Body: fmt.Sprintf("The adapter told the owner: %q\nSpace: %s\nThread: %s\nMessage: %s\nRead: gc gchat read --space %s\nStatus: gc gchat status\n", line, m.Conversation, m.Thread, m.ID, m.Conversation)})
		},
		Post: func(ctx context.Context, m neversilent.Message, line string) error {
			return a.s.PostNoticeWithClient(ctx, a.ownerNoticeClient(), m, line)
		},
	})
}

// The detached watch inherits the generation's local preconditions. Ownership
// loss is permanent; local reconciliation must finish before Poll decides what
// is due. Poll's OwnerReady predicate gates only the owner reservation, so a
// credential hold cannot delay operator mail.
func (a *application) neverSilentGate() error {
	if err := a.checkStateLock(); err != nil {
		return err
	}
	if !a.neverSilent.Enabled() {
		return nil
	}
	a.mu.Lock()
	snapshot := a.l.Clone()
	a.mu.Unlock()
	return inbound.ReconcileNeverSilent(snapshot, a.neverSilent)
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
			err := a.neverSilentGate()
			if err == nil {
				err = a.neverSilent.Poll(ctx)
			}
			if errors.Is(err, ErrStateLockLost) {
				return
			}
			if err != nil {
				a.log.Warn("never-silent poll failed", "error", err)
			}
		}
	}
}
