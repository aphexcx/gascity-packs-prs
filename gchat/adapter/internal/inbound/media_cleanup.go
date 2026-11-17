package inbound

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/media"
	api "google.golang.org/api/chat/v1"
)

// MediaStore shares copy-on-save persistence with the caller. Both operations
// use its ledger mutex, released before filesystem or network I/O. Normalizers
// call it while holding mediaNormalizationGate, as do cleanup and edit changes.
type MediaStore struct {
	Snapshot func() (*ledger.Ledger, error)
	Update   func(func(*ledger.Ledger)) error
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

func mediaAllowed(l *ledger.Ledger, m *api.Message, attachment string) string {
	if state := l.Messages[m.Name]; state != nil && state.Deleted {
		return "message deleted"
	}
	if state := l.Media[m.Name]; state != nil {
		if state.DeletePending {
			return "message deleted"
		}
		revision := m.LastUpdateTime
		if revision == "" {
			revision = m.CreateTime
		}
		at, _ := time.Parse(time.RFC3339Nano, revision)
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
func (s *MediaStore) reserve(m *api.Message, attachment, dest string) string {
	if s == nil {
		return ""
	}
	reason := ""
	err := s.Update(func(l *ledger.Ledger) {
		reason = mediaAllowed(l, m, attachment)
		if reason != "" {
			return
		}
		state := mediaState(l, m.Name)
		if state.Files == nil {
			state.Files = make(map[string]string)
		}
		state.Files[media.FileID(filepath.Base(dest))] = attachment
	})
	if err != nil {
		return "media state unavailable"
	}
	return reason
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

// An accepted edit durably records which attachments it retained, including
// edits that wake no session. Cleanup is independent of turn delivery.
func (p *Processor) editMedia(ctx context.Context, m *api.Message) error {
	if !mediaLock(ctx) {
		return ctx.Err()
	}
	defer func() { <-mediaNormalizationGate }()
	return p.update(func(l *ledger.Ledger) {
		state := mediaState(l, m.Name)
		state.Revision, _ = time.Parse(time.RFC3339Nano, m.LastUpdateTime)
		state.Retained = nil
		for _, a := range m.Attachment {
			if a != nil {
				state.Retained = append(state.Retained, a.Name)
			}
		}
		for id, attachment := range state.Files {
			if !slices.Contains(state.Retained, attachment) {
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
	l, err := p.snapshot()
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
	l, err = p.snapshot()
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
		result = media.Remove(p.opts.Config.Endpoints.MediaDir, parts[1], parts[3], selection)
	}
	if result.Removed > 0 {
		p.opts.Logger.Info("saved attachments removed", "message", name, "count", result.Removed)
	}
	if result.Cause == "" {
		err = p.update(func(l *ledger.Ledger) {
			current := mediaState(l, name)
			if state.DeletePending {
				delete(l.Media, name)
			} else {
				for id := range state.Remove {
					delete(current.Files, id)
					delete(current.Remove, id)
				}
			}
		})
		if err != nil {
			result.Cause = "ledger"
		}
	}
	if result.Cause != "" {
		p.opts.Logger.Warn("saved attachment removal deferred", "message", name, "remaining", result.Remaining, "cause", result.Cause)
	}
}

func (p *Processor) cleanupMediaPass(ctx context.Context, l *ledger.Ledger) {
	for name, state := range l.Media {
		if ctx.Err() != nil {
			return
		}
		if state != nil && (state.DeletePending || len(state.Remove) > 0) {
			p.cleanupMedia(ctx, name)
		}
	}
}
