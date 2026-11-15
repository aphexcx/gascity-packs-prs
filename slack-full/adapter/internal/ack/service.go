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

// sessionMarkAge keeps the session's reaction mark longer than any placement
// record written after it: the acknowledgement's record comes within moments
// of its message, so a mark pruned with the records could leave a younger
// record behind, and the session's repeat would take it.
const sessionMarkAge = 2 * MarkerAge

var markerName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// PruneAttempts removes acknowledgement markers older than MarkerAge from
// <dir>/ack-attempts and placement records (NotePlacing) from
// <dir>/ack-placed, and the session's reaction marks (SessionPlaced) older
// than sessionMarkAge from <dir>/ack-session, by modification time. Only
// empty files named by 64 hex characters are markers, records or marks;
// nothing else in the state directory is touched. Errors are ignored: a
// marker left behind only costs an inode.
func PruneAttempts(dir string, now time.Time) {
	for _, sub := range []string{"ack-attempts", "ack-placed", "ack-session"} {
		age := MarkerAge
		if sub == "ack-session" {
			age = sessionMarkAge
		}
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
			if err != nil || info.Size() != 0 || now.Sub(info.ModTime()) <= age {
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
	// placing makes each read and write of the placement records and the
	// session's reaction marks one step: one process owns the state directory.
	placing sync.Mutex
	// reacting guards turns, the react verb's one turn per reaction that a
	// session call holds or waits for (HoldReaction).
	reacting sync.Mutex
	turns    map[string]*reactionTurn
}

// reactionTurn is held by one session call on a reaction at a time; callers
// counts the holder and the calls waiting, and the turn goes with the last.
type reactionTurn struct {
	sync.Mutex
	callers int
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

// reactionKey names one emoji on one message. A Slack name may come with its
// colons; Google may add or drop the emoji presentation selector. Neither
// changes the reaction.
func reactionKey(conversation, message, emoji string) string {
	emoji = strings.ReplaceAll(strings.Trim(emoji, ":"), "\ufe0f", "")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(messageDigest(conversation, message)+"\x00"+emoji)))
}

// reactionPath names one emoji on one message under <dir>/<sub>.
func reactionPath(dir, sub, conversation, message, emoji string) string {
	return filepath.Join(dir, sub, reactionKey(conversation, message, emoji))
}

// HoldReaction gives the react verb its turn on one reaction. The verb holds
// the turn across its channel call and that call's bookkeeping
// (SessionPlaced, TakePlaced, SessionRemoved), then calls release, so the
// session's next call on the same conversation, message and emoji reaches
// the channel only after this call's answer is handled. The mark then
// follows the order in which the channel applied the session's calls. A call
// on another message or another emoji never waits, and the acknowledgement's
// own call takes no turn. A turn lasts as long as the holder's channel call,
// which that call's own timeout bounds. While the acknowledgement is off it
// holds nothing.
func (s *Service) HoldReaction(conversation, message, emoji string) (release func()) {
	if s == nil || !s.Choice.Enabled() {
		return func() {}
	}
	key := reactionKey(conversation, message, emoji)
	s.reacting.Lock()
	if s.turns == nil {
		s.turns = map[string]*reactionTurn{}
	}
	turn := s.turns[key]
	if turn == nil {
		turn = &reactionTurn{}
		s.turns[key] = turn
	}
	turn.callers++
	s.reacting.Unlock()
	turn.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			turn.Unlock()
			s.reacting.Lock()
			turn.callers--
			if turn.callers == 0 {
				delete(s.turns, key)
			}
			s.reacting.Unlock()
		})
	}
}

// placedPath is the record of the acknowledgement's emoji on one message.
func placedPath(dir, conversation, message, emoji string) string {
	return reactionPath(dir, "ack-placed", conversation, message, emoji)
}

// sessionPath is the mark of the session's own reaction of emoji on one
// message: it is there as it would be with the acknowledgement off.
func sessionPath(dir, conversation, message, emoji string) string {
	return reactionPath(dir, "ack-session", conversation, message, emoji)
}

// NotePlacing records, just before the acknowledgement's channel call, that
// the call may place emoji on in's message as the identity the adapter's
// react verb uses. The record stands unless the channel answers that the call
// placed nothing (DropPlacing): a lost answer or a crash keeps it, so the
// session's own reaction of that emoji, which the channel answers "already
// there", still counts for never-silent exactly as it would with the
// acknowledgement off (TakePlaced), whether it comes before, during or after
// the call. A record whose reaction was never placed is harmless: the
// session's first reaction of that emoji is then created (SessionPlaced).
func (s *Service) NotePlacing(in Input, emoji string) error {
	if s == nil {
		return nil
	}
	s.placing.Lock()
	defer s.placing.Unlock()
	return touch(placedPath(s.Dir, in.Conversation, in.Message, emoji))
}

// DropPlacing removes NotePlacing's record when the channel answers that the
// call placed nothing: the identity's reaction was already there (the
// session's own, placed first), or the call was refused.
func (s *Service) DropPlacing(in Input, emoji string) {
	if s == nil {
		return
	}
	s.placing.Lock()
	defer s.placing.Unlock()
	remove(placedPath(s.Dir, in.Conversation, in.Message, emoji))
}

// TakePlaced is asked by the react verb when the channel answers the
// session's reaction of emoji "already there". It reports whether that
// reaction is the acknowledgement's, so that the session's counts as new: a
// placement record stands and the session's own reaction is not marked
// there. Told true, the record is taken and the session's reaction marked, so
// of two "already there" answers at once only one is told true. A marked
// reaction is the session's own: its "already there" is a repeat, as with the
// acknowledgement off, even while the acknowledgement's record still stands
// because that call's own "already there" answer was lost or is in flight.
// While the acknowledgement is off it is always false and reads and writes
// nothing, whatever an earlier run left on disk.
func (s *Service) TakePlaced(conversation, message, emoji string) bool {
	if s == nil || !s.Choice.Enabled() {
		return false
	}
	s.placing.Lock()
	defer s.placing.Unlock()
	mark := sessionPath(s.Dir, conversation, message, emoji)
	if _, err := os.Stat(mark); err == nil {
		return false
	}
	if !remove(placedPath(s.Dir, conversation, message, emoji)) {
		return false
	}
	// A mark lost to a crash leaves no record to take: a repeat still counts
	// as nothing.
	s.markError(touch(mark))
	return true
}

// SessionPlaced is told by the react verb when the channel answers that the
// session's reaction of emoji was placed. That reaction counts as it always
// has; it is marked, so its later "already there" is a repeat whatever
// placement record the acknowledgement writes after it. A record stays: if
// the session removes its reaction before the acknowledgement's call lands,
// the reaction there is then the acknowledgement's. While the acknowledgement
// is off it does nothing.
func (s *Service) SessionPlaced(conversation, message, emoji string) {
	if s == nil || !s.Choice.Enabled() {
		return
	}
	s.placing.Lock()
	defer s.placing.Unlock()
	s.markError(touch(sessionPath(s.Dir, conversation, message, emoji)))
}

// markError logs a mark that could not be written: the session's repeat of
// that reaction may then count as new if the acknowledgement's record stands.
func (s *Service) markError(err error) {
	if err != nil && s.Log != nil {
		s.Log("ACK_REACTION: session reaction mark unavailable; a repeat of that reaction may count for never-silent")
	}
}

// SessionRemoved is told by the react verb when the channel answers the
// session's removal of emoji definitely (removed, or none there): the
// session's own reaction is not there now, and its mark goes. While the
// acknowledgement is off it does nothing.
func (s *Service) SessionRemoved(conversation, message, emoji string) {
	if s == nil || !s.Choice.Enabled() {
		return
	}
	s.placing.Lock()
	defer s.placing.Unlock()
	remove(sessionPath(s.Dir, conversation, message, emoji))
}

// touch makes path an empty file that survives a crash.
func touch(path string) error {
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

// remove reports whether path was there and is now gone for good: a file that
// came back after a crash would count a repeat.
func remove(path string) bool {
	if os.Remove(path) != nil {
		return false
	}
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
