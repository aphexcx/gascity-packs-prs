package app

import (
	"context"
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
	if id == a.credentialIdentity && a.l.IdentityUserID != "" {
		return nil
	}
	previous := a.l.IdentityUserID
	if previous != id {
		a.l.IdentityUserID = id
		if err := a.l.Save(a.cfg.Endpoints.StateFile); err != nil {
			a.l.IdentityUserID = previous
			return err
		}
		if previous != "" {
			a.log.Warn("credential identity changed; rebuilding client", "operation", "identity")
		}
	}
	a.credentialIdentity = id
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
				if err := a.s.Register(ctx); err != nil {
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

func (a *application) notice(kind string) {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	if a.notified[kind] {
		return
	}
	a.notified[kind] = true
	select {
	case a.notices <- kind:
	default:
	}
}
func (a *application) notifyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case kind := <-a.notices:
			message := "Subscription reconciliation failed; inspect gc gchat status."
			if kind == "credentials" {
				message = "Google Chat credentials are missing or revoked; run gc gchat auth. Events remain at Google."
			}
			nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := a.opts.Notify(nctx, "gchat: "+kind, message)
			cancel()
			if err != nil {
				a.log.Warn("operator notification failed", "operation", "notify")
			}
		}
	}
}
func notifyMayor(ctx context.Context, subject, body string) error {
	return exec.CommandContext(ctx, "gc", "mail", "send", "mayor", "-s", subject, "-m", body).Run()
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
