package app

import (
	"context"
	"time"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
)

func (a *application) rememberMediaRoots(roots ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.l.Clone()
	changed := false
	for _, root := range roots {
		changed = inbound.RememberMediaRoot(next, root) || changed
	}
	if !changed {
		return nil
	}
	if err := next.Save(a.cfg.Endpoints.StateFile); err != nil {
		return err
	}
	*a.l = *next
	return nil
}

// Local cleanup belongs to the service, independent of credential generations,
// registration, subscriptions and history reconciliation. Capture immutable
// process coordinates; settings reloads persist additional roots in the ledger.
func (a *application) pollMediaCleanup(ctx context.Context, root, statePath string) {
	store := &inbound.MediaStore{
		Snapshot: func() (*ledger.Ledger, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if err := a.checkStateLock(); err != nil {
				return nil, err
			}
			return a.l.Clone(), nil
		},
		Update: func(change func(*ledger.Ledger)) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			next := a.l.Clone()
			change(next)
			if err := next.Save(statePath); err != nil {
				return err
			}
			*a.l = *next
			return nil
		},
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		a.recoverMedia(ctx, root, store)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *application) recoverMedia(ctx context.Context, root string, store *inbound.MediaStore) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if a.loaded.Load() && a.checkStateLock() == nil {
		_ = store.Recover(ctx, root, a.log)
	}
}
