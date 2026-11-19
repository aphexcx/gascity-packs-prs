package inbound

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/media"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

// MediaStore shares copy-on-save persistence with the caller. Both operations
// use its ledger mutex, released before filesystem or network I/O. Normalizers
// call it while holding mediaNormalizationGate, as do cleanup and edit changes.
type MediaStore struct {
	Snapshot func() (*ledger.Ledger, error)
	Update   func(func(*ledger.Ledger)) error
}

// RememberMediaRoot records an already-canonical configured inbound directory.
// The caller persists the ledger before enabling the new configuration. These
// are store roots, never attachment names or paths inside a message directory.
func RememberMediaRoot(l *ledger.Ledger, root string) bool {
	if root == "" || slices.Contains(l.MediaRoots, root) {
		return false
	}
	l.MediaRoots = append(l.MediaRoots, root)
	return true
}

func mediaState(l *ledger.Ledger, message string) *ledger.MediaState {
	if l.Media == nil {
		l.Media = make(map[string]*ledger.MediaState)
	}
	if l.Media[message] == nil {
		l.Media[message] = &ledger.MediaState{}
	}
	return l.Media[message]
}

func mediaRevision(m *api.Message) time.Time {
	revision := m.LastUpdateTime
	if revision == "" {
		revision = m.CreateTime
	}
	at, _ := time.Parse(time.RFC3339Nano, revision)
	return at
}

func mediaAllowed(l *ledger.Ledger, m *api.Message, attachment string) string {
	if state := l.Messages[m.Name]; state != nil && state.Deleted {
		return "message deleted"
	}
	if state := l.Media[m.Name]; state != nil {
		if state.DeletePending {
			return "message deleted"
		}
		at := mediaRevision(m)
		if !state.Revision.IsZero() && !at.After(state.Revision) && !slices.Contains(state.Retained, attachment) {
			return "attachment removed"
		}
	}
	return ""
}

func (s *MediaStore) allowed(m *api.Message, attachment string) string {
	if s == nil {
		return ""
	}
	l, err := s.Snapshot()
	if err != nil {
		return "media state unavailable"
	}
	return mediaAllowed(l, m, attachment)
}

// Reserve before writing: even a stop during a history download leaves a link
// for a later edit. A failed download may leave a harmless link to no file.
func (s *MediaStore) reserve(m *api.Message, attachment, dest string) (string, bool) {
	if s == nil {
		return "", false
	}
	reason := ""
	pending := false
	err := s.Update(func(l *ledger.Ledger) {
		reason = mediaAllowed(l, m, attachment)
		if reason != "" {
			return
		}
		state := mediaState(l, m.Name)
		if state.Files == nil {
			state.Files = make(map[string]string)
		}
		root := filepath.Dir(filepath.Dir(filepath.Dir(dest)))
		RememberMediaRoot(l, root)
		id := media.FileID(root, filepath.Base(dest))
		// A vacant pathname may still have an unlink awaiting synchronization.
		// Preserve that debt and its owner; the caller can choose another name.
		if state.Remove[id] {
			reason, pending = "media state unavailable", true
			return
		}
		state.Files[id] = attachment
		if state.Sources == nil {
			state.Sources = make(map[string]time.Time)
		}
		state.Sources[id] = mediaRevision(m)
	})
	if err != nil {
		return "media state unavailable", false
	}
	return reason, pending
}

func (p *Processor) mediaStore() *MediaStore {
	return &MediaStore{Snapshot: p.snapshot, Update: p.update}
}

func mediaLock(ctx context.Context) bool {
	select {
	case mediaNormalizationGate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// An accepted snapshot durably records which attachments it retained, including
// snapshots deduplicated for delivery. Cleanup is independent of turn delivery.
func (p *Processor) editMedia(ctx context.Context, m *api.Message) error {
	if !mediaLock(ctx) {
		return ctx.Err()
	}
	defer func() { <-mediaNormalizationGate }()
	return p.update(func(l *ledger.Ledger) {
		state := mediaState(l, m.Name)
		revision := mediaRevision(m)
		if revision.Before(state.Revision) {
			return
		}
		state.Revision = revision
		state.Retained = nil
		for _, a := range m.Attachment {
			if a != nil {
				state.Retained = append(state.Retained, a.Name)
			}
		}
		for id, attachment := range state.Files {
			if !state.Sources[id].After(revision) && !slices.Contains(state.Retained, attachment) {
				if state.Remove == nil {
					state.Remove = make(map[string]bool)
				}
				state.Remove[id] = true
			}
		}
	})
}

// cleanupMedia makes one local attempt. Its debt was committed before this
// point; failure never Nacks a turn or holds later turns behind that message.
func (p *Processor) cleanupMedia(ctx context.Context, name string) {
	p.mediaStore().cleanup(ctx, p.opts.Config.Endpoints.MediaDir, name, p.opts.Logger)
}

func (s *MediaStore) cleanup(ctx context.Context, root, name string, logger puller.Logger) {
	l, err := s.Snapshot()
	if err != nil {
		return
	}
	state := l.Media[name]
	if state == nil || (!state.DeletePending && len(state.Remove) == 0) {
		return
	}
	if !mediaLock(ctx) {
		return
	}
	defer func() { <-mediaNormalizationGate }()
	l, err = s.Snapshot()
	if err != nil {
		return
	}
	state = l.Media[name]
	if state == nil || (!state.DeletePending && len(state.Remove) == 0) {
		return
	}
	result := media.Removal{Remaining: -1, Cause: "unsafe"}
	parts := strings.Split(name, "/")
	if len(parts) == 4 && parts[0] == "spaces" && parts[2] == "messages" {
		selection := state.Remove
		if state.DeletePending {
			selection = nil
		}
		roots := append(slices.Clone(l.MediaRoots), root)
		seen := make(map[string]bool)
		result = media.Removal{}
		for _, candidate := range roots {
			canonical, err := filepath.Abs(candidate)
			if err != nil || candidate == "" {
				result.Remaining, result.Cause = -1, "unsafe"
				continue
			}
			if seen[canonical] {
				continue
			}
			seen[canonical] = true
			next := media.Remove(canonical, parts[1], parts[3], selection)
			result.Removed += next.Removed
			if result.Remaining < 0 || next.Remaining < 0 {
				result.Remaining = -1
			} else {
				result.Remaining += next.Remaining
			}
			if next.Cause != "" {
				result.Cause = next.Cause
			}
		}
	}
	if result.Removed > 0 {
		logger.Info("saved attachments removed", "message", name, "count", result.Removed)
	}
	if result.Cause == "" {
		err = s.Update(func(l *ledger.Ledger) {
			current := mediaState(l, name)
			if state.DeletePending {
				delete(l.Media, name)
			} else {
				for id := range state.Remove {
					delete(current.Files, id)
					delete(current.Sources, id)
					delete(current.Remove, id)
				}
			}
		})
		if err != nil {
			result.Cause = "ledger"
		}
	}
	if result.Cause != "" {
		logger.Warn("saved attachment removal deferred", "message", name, "remaining", result.Remaining, "cause", result.Cause)
	}
}

func (p *Processor) cleanupMediaPass(ctx context.Context, l *ledger.Ledger) {
	p.mediaStore().recoverSnapshot(ctx, p.opts.Config.Endpoints.MediaDir, l, p.opts.Logger)
}

// Recover needs only local storage, so the service can run it before Google or
// gc is ready. It enrolls no tombstone that lacks an explicit cleanup debt.
func (s *MediaStore) Recover(ctx context.Context, root string, logger puller.Logger) error {
	l, err := s.Snapshot()
	if err != nil {
		return err
	}
	s.recoverSnapshot(ctx, root, l, logger)
	return nil
}

func (s *MediaStore) recoverSnapshot(ctx context.Context, root string, l *ledger.Ledger, logger puller.Logger) {
	for name, state := range l.Media {
		if ctx.Err() != nil {
			return
		}
		if state != nil && (state.DeletePending || len(state.Remove) > 0) {
			s.cleanup(ctx, root, name, logger)
		}
	}
}
