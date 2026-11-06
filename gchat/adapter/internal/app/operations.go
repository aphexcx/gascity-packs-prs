package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
)

// Seed once per credential identity. Observed Chat identity wins subsequent
// client rebuilds, so an OAuth/Chat mismatch cannot create a rebuild loop.
func (a *application) seedIdentity(id string) error {
	if id == "" {
		a.legacyWarning.Do(func() { a.log.Warn("legacy token has no identity; run gc gchat auth", "operation", "identity") })
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id == a.l.IdentitySeed && a.l.IdentityUserID != "" {
		return nil
	}
	previous := a.l.IdentityUserID
	previousSeed := a.l.IdentitySeed
	a.l.IdentityUserID, a.l.IdentitySeed = id, id
	if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
		a.l.IdentityUserID, a.l.IdentitySeed = previous, previousSeed
		return err
	}
	// Shutdown may checkpoint before the generation installs its new client.
	// Once a new consent is durable, the previous client cannot supply identity.
	a.chatClient.Store(nil)
	if previous != "" && previous != id {
		if previousSeed == "" {
			a.log.Warn("legacy ledger identity re-seeded from credential; observed identity will be verified again", "operation", "identity")
		} else {
			a.log.Warn("credential identity changed; rebuilding client", "operation", "identity")
		}
	}
	return nil
}

func (a *application) persistIdentity(c *chat.Client) error {
	id, conflicted := c.ObservedIdentityUserID()
	var result error
	if conflicted {
		result = errIdentityChanged
	}
	if id == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.l.IdentityUserID == id {
		return result
	}
	previous := a.l.IdentityUserID
	a.l.IdentityUserID = id
	if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
		a.l.IdentityUserID = previous
		return err
	}
	return result
}

func (a *application) quarantine(ctx context.Context, ev *puller.Event, path string, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, q := range a.l.Quarantine {
		if q.PubSubID == ev.PubSubID {
			return nil
		}
	}
	previous := a.l.Quarantine
	a.l.Quarantine = append(append([]ledger.QuarantinedEvent(nil), previous...), ledger.QuarantinedEvent{PubSubID: ev.PubSubID, Reason: cause.Error(), Path: path, At: time.Now()})
	if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
		a.l.Quarantine = previous
		return err
	}
	return nil
}

func (a *application) retryPending(ctx context.Context, registered *atomic.Bool) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !registered.Load() {
				continue
			}
			if a.gcDown.Load() {
				if err := a.ensureRegistered(ctx, registered); err != nil {
					return
				}
				a.gcDown.Store(false)
			}
			if a.s.Health().State != "ready" {
				continue
			}
			if err := a.s.RetryPending(ctx); err != nil && ctx.Err() == nil {
				count := 1
				if joined, ok := err.(interface{ Unwrap() []error }); ok {
					count = len(joined.Unwrap())
				}
				a.log.Warn("outbound recovery deferred", "operation", "retry_pending", "failed_keys", count)
			}
		}
	}
}

type notification struct{ kind, message, agent, dedup string }

func (a *application) notice(kind string, details ...string) {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	key := kind
	if len(details) > 0 {
		key += fmt.Sprintf(":%x", sha256.Sum256([]byte(details[0])))
	}
	if a.notified[key] {
		return
	}
	message := "Subscription reconciliation failed; inspect gc gchat status and the README Google-side setup checklist (enabled APIs and topic Publisher IAM)."
	if kind == "credentials" {
		message = "Google Chat credentials are missing or revoked; run gc gchat auth. Events remain at Google."
	} else if len(details) > 0 {
		message = details[0] + ". " + message
	}
	select {
	case a.notices <- notification{kind: kind, message: message, agent: a.cfg.Identity().Agent, dedup: "gchat:" + a.cfg.Secrets.CityName + ":" + a.cfg.Identity().Name + ":" + key}:
		a.notified[key] = true
	default:
	}
}
func (a *application) notifyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case notice := <-a.notices:
			nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			var err error
			if a.opts.Notify != nil {
				err = a.opts.Notify(nctx, "gchat: "+notice.kind, notice.message)
			} else {
				bin := os.Getenv("GC_BIN")
				if bin == "" {
					bin = "gc"
				}
				err = exec.CommandContext(nctx, bin, "mail", "send", notice.agent, "--dedup", notice.dedup, "-s", "gchat: "+notice.kind, "-m", notice.message).Run()
			}
			cancel()
			if err != nil {
				a.log.Warn("operator notification failed", "operation", "notify")
			}
		}
	}
}
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
