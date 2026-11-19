package ack

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MarkerAge bounds an attempt marker's use: it matters only within MaxAge of
// its message, so a day is ample. PruneInterval is the running pass.
const (
	MarkerAge     = 24 * time.Hour
	PruneInterval = 24 * time.Hour
)

var markerName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PruneAttempts removes acknowledgement markers older than MarkerAge from
// <dir>/ack-attempts, and placement records (NotePlacing) from
// <dir>/ack-placed, by modification time. Only empty files named by 64 hex
// characters are markers or records; nothing else in the state directory is
// touched. Errors are ignored: a marker left behind only costs an inode.
func PruneAttempts(dir string, now time.Time) {
	for _, sub := range []string{"ack-attempts", "ack-placed"} {
		root := filepath.Join(dir, sub)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !markerName.MatchString(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil || info.Size() != 0 || now.Sub(info.ModTime()) <= MarkerAge {
				continue
			}
			_ = os.Remove(filepath.Join(root, e.Name()))
		}
	}
}

// KeepPruned runs PruneAttempts once per PruneInterval until ctx ends; the
// start runs PruneAttempts itself first.
func KeepPruned(ctx context.Context, dir string) {
	ticker := time.NewTicker(PruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			PruneAttempts(dir, now)
		}
	}
}

type Input struct {
	Conversation, Message, Thread, Text string
	Time, Seen                          time.Time
	Human, Own, Edit, Replay, Addressed bool
}

// Service is independent of delivery locks and queues. Its durable marker is
// written before the vendor or channel call, including unsuccessful attempts:
// an uncertain channel result must never cause a different second reaction.
type Service struct {
	Choice *Choice
	Dir    string
	Send   func(context.Context, Input, Row) error
	// SendChoice lets a paced channel overlap choice with its shared send queue.
	SendChoice func(context.Context, Input, func(context.Context) (Row, string)) (string, error)
	Inbound    func(Input)
	Reaction   func(Input, string, time.Time)
	Log        func(string)
	workers    sync.WaitGroup
	warning    sync.Once
}

func (s *Service) Observe(in Input) {
	if s != nil {
		s.ObserveUsing(in, s.Send)
	}
}

// ObserveUsing preserves the same choices and attempt registry when a channel
// needs a different authenticated sender for a particular conversation.
func (s *Service) ObserveUsing(in Input, send func(context.Context, Input, Row) error) {
	if s == nil || !in.Human || in.Own || in.Edit || in.Replay || in.Time.IsZero() || in.Seen.Sub(in.Time) > MaxAge || (!in.Addressed && !s.Choice.All()) {
		return
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		if s.Inbound != nil {
			s.Inbound(in)
		}
		if !s.Choice.Enabled() || (send == nil && s.SendChoice == nil) {
			return
		}
		claimed, err := s.claim(in)
		if err != nil {
			s.warn("ACK_REACTION: attempt state unavailable; acknowledgement skipped")
			return
		}
		if !claimed {
			return
		}
		var how string
		if s.SendChoice != nil {
			// A paced channel bounds HTTP after granting a slot. Its backlog is
			// retained, including overflow beyond the reaction target deadline.
			how, err = s.SendChoice(context.Background(), in, func(choiceCtx context.Context) (Row, string) { return s.Choice.Choose(choiceCtx, in.Text, in.Seen) })
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var row Row
			row, how = s.Choice.Choose(ctx, in.Text, in.Seen)
			err = send(ctx, in, row)
		}
		if err != nil {
			s.warn("ACK_REACTION: channel refused acknowledgement; no retry")
			return
		}
		if s.Reaction != nil {
			s.Reaction(in, how, time.Now())
		}
	}()
}

func (s *Service) Wait() {
	if s != nil {
		s.workers.Wait()
	}
}
func (s *Service) warn(message string) {
	s.warning.Do(func() {
		if s.Log != nil {
			s.Log(message)
		}
	})
}

// messageDigest names one message: the attempt marker's key, and the first
// half of a placement record's.
func messageDigest(conversation, message string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(conversation+"\x00"+message)))
}

// placedPath is the record of the acknowledgement's emoji on one message. A
// Slack name may come with its colons; Google may add or drop the emoji
// presentation selector. Neither changes the reaction.
func placedPath(dir, conversation, message, emoji string) string {
	emoji = strings.ReplaceAll(strings.Trim(emoji, ":"), "\ufe0f", "")
	return filepath.Join(dir, "ack-placed", fmt.Sprintf("%x", sha256.Sum256([]byte(messageDigest(conversation, message)+"\x00"+emoji))))
}

// NotePlacing records, just before the acknowledgement's channel call, that
// the call may place emoji on in's message as the identity the adapter's
// react verb uses. The record stands unless the channel answers that the call
// placed nothing (DropPlacing): a lost answer or a crash keeps it, so the
// session's own reaction of that emoji, which the channel answers "already
// there", still counts for never-silent exactly as it would with the
// acknowledgement off (TakePlaced), whether it comes before, during or after
// the call. A record whose reaction was never placed is harmless: the
// session's first reaction of that emoji is then created, and takes it.
func (s *Service) NotePlacing(in Input, emoji string) error {
	if s == nil {
		return nil
	}
	path := placedPath(s.Dir, in.Conversation, in.Message, emoji)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// DropPlacing removes NotePlacing's record when the channel answers that the
// call placed nothing: the identity's reaction was already there (the
// session's own, placed first), or the call was refused.
func (s *Service) DropPlacing(in Input, emoji string) {
	if s == nil {
		return
	}
	path := placedPath(s.Dir, in.Conversation, in.Message, emoji)
	if os.Remove(path) == nil {
		_ = syncDir(filepath.Dir(path))
	}
}

// TakePlaced is asked by the react verb on the channel's definite answer to
// the session's reaction of emoji: placed, or already there. It reports
// whether the acknowledgement's placement of that emoji was still unmet, and
// meets it: only the first caller is told true. An "already there" told true
// is the acknowledgement's reaction, and the session's counts as new; any
// later one is the session's own repeat. While the acknowledgement is off it
// is always false, whatever an earlier run left on disk.
func (s *Service) TakePlaced(conversation, message, emoji string) bool {
	if s == nil || !s.Choice.Enabled() {
		return false
	}
	path := placedPath(s.Dir, conversation, message, emoji)
	if os.Remove(path) != nil {
		return false
	}
	// A record that comes back after a crash would count a repeat.
	_ = syncDir(filepath.Dir(path))
	return true
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Service) claim(in Input) (bool, error) {
	root := filepath.Join(s.Dir, "ack-attempts")
	if err := os.MkdirAll(root, 0700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(root, messageDigest(in.Conversation, in.Message)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	dir, err := os.Open(root)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return false, err
	}
	return true, nil
}
