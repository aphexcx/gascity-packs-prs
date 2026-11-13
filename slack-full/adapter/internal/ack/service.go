package ack

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// <dir>/ack-attempts, by modification time. Only empty files named by 64 hex
// characters are markers; nothing else in the state directory is touched.
// Errors are ignored: a marker left behind only costs an inode.
func PruneAttempts(dir string, now time.Time) {
	root := filepath.Join(dir, "ack-attempts")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
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
func (s *Service) claim(in Input) (bool, error) {
	root := filepath.Join(s.Dir, "ack-attempts")
	if err := os.MkdirAll(root, 0700); err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(in.Conversation + "\x00" + in.Message))
	f, err := os.OpenFile(filepath.Join(root, fmt.Sprintf("%x", digest)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
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
