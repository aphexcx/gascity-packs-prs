package neversilent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const DownLine = "I can't read your message right now. The team that runs me has been told, and I'll answer as soon as I'm back."
const NoSeatLine = "I can't answer yet because no Claude account is signed in for me. The team that runs me has been told."
const CapLine = "I've reached my usage limit for now and can't answer until it resets. The team that runs me has been told."

var meanings = map[string]string{
	"not_delivered": "the message did not land and will not without recovery",
	"no_seat":       "no seat is signed in",
	"seat_at_cap":   "the seat has reached its usage cap",
	"session_down":  "the session cannot answer",
}

type Message struct {
	// CanPost is a live dispatch guard, never part of durable state.
	CanPost      func() bool `json:"-"`
	ID           string      `json:"id"`
	Conversation string      `json:"conversation"`
	Thread       string      `json:"thread,omitempty"`
	Created      time.Time   `json:"created"`
	Targets      []string    `json:"targets,omitempty"`
	OwnerAgent   string      `json:"owner_agent,omitempty"`
	Observed     time.Time   `json:"observed,omitempty"`
	ObservedKind string      `json:"observed_kind,omitempty"`
	Delivered    time.Time   `json:"delivered,omitempty"`
	FailureSince time.Time   `json:"failure_since,omitempty"`
	FailureKind  string      `json:"failure_kind,omitempty"`
	Stopped      time.Time   `json:"stopped,omitempty"`
	Uncertain    bool        `json:"uncertain,omitempty"`
	Removed      bool        `json:"removed,omitempty"`
	Notified     bool        `json:"notified,omitempty"`
}

type act struct {
	Conversation string    `json:"conversation"`
	Thread       string    `json:"thread"`
	Session      string    `json:"session"`
	At           time.Time `json:"at"`
}

type ownerMark struct {
	At        time.Time `json:"at"`
	Posted    bool      `json:"posted"`
	Line      string    `json:"line"`
	Message   Message   `json:"message"`
	LocalMail mailMark  `json:"local_mail"`
}

type mailMark struct {
	Sent  bool      `json:"sent"`
	Tried time.Time `json:"tried,omitempty"`
}

type Outage struct {
	ID            string    `json:"id"`
	Cause         string    `json:"cause"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end,omitempty"`
	Conversations int       `json:"conversations"`
	Messages      int       `json:"messages"`
	Oldest        time.Time `json:"oldest"`
	OwnersTold    int       `json:"owners_told"`
	Health        Health    `json:"health"`
	Mail          mailMark  `json:"mail"`
	ClearedMail   mailMark  `json:"cleared_mail"`
}

type state struct {
	Messages map[string]Message   `json:"messages"`
	Acts     []act                `json:"acts"`
	Owners   map[string]ownerMark `json:"owners"`
	Posted   []time.Time          `json:"posted"`
	Outages  []Outage             `json:"outages"`
}

type Mail struct{ Subject, Body, Key string }

type Options struct {
	Enabled                                                             bool
	Path, HealthFile, City, Channel, OperatorContext, OperatorAgent, GC string
	Now                                                                 func() time.Time
	Post                                                                func(context.Context, Message, string) error
	Send                                                                func(context.Context, Mail) error
	SendLocal                                                           func(context.Context, Message, string) error
	Log                                                                 func(string)
}

type Engine struct {
	pollMu sync.Mutex
	mu     sync.Mutex
	opts   Options
	state  state
}

type Status struct {
	Enabled             bool     `json:"enabled"`
	Health              Health   `json:"health"`
	Outages             []Outage `json:"outages"`
	OwnerLinesSevenDays int      `json:"owner_lines_seven_days"`
}

func New(opts Options) (*Engine, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = func(s string) { log.Print(s) }
	}
	if opts.OperatorAgent == "" {
		opts.OperatorAgent = "mayor"
	}
	if opts.GC == "" {
		opts.GC = "gc"
	}
	e := &Engine{opts: opts, state: state{Messages: map[string]Message{}, Owners: map[string]ownerMark{}}}
	if !opts.Enabled {
		return e, nil
	}
	if opts.Path == "" || opts.Post == nil {
		return nil, errors.New("never-silent requires durable state and an owner sender")
	}
	b, err := os.ReadFile(opts.Path)
	if err == nil {
		if json.Unmarshal(b, &e.state) != nil || e.state.Messages == nil || e.state.Owners == nil {
			return nil, errors.New("never-silent state is invalid; preserve it for recovery")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("never-silent state cannot be read")
	}
	if e.opts.Send == nil {
		e.opts.Send = func(ctx context.Context, m Mail) error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, opts.GC, "mail", "send", "--context", opts.OperatorContext, opts.OperatorAgent, "--dedup", m.Key, "-s", m.Subject, "-m", m.Body)
			cmd.WaitDelay = time.Second
			return cmd.Run()
		}
	}
	return e, nil
}

// change is called with mu held. Failed persistence cannot spend an in-memory
// mark or permit an effect; a later pass retries the original durable state.
func (e *Engine) change(fn func(*state)) error {
	b, err := json.Marshal(e.state)
	if err != nil {
		return err
	}
	var next state
	if err = json.Unmarshal(b, &next); err != nil {
		return err
	}
	fn(&next)
	b, err = json.Marshal(next)
	if err != nil {
		return err
	}
	if err = atomicSave(e.opts.Path, b); err != nil {
		return err
	}
	e.state = next
	return nil
}

func atomicSave(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".never-silent-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Track is only called for newly admitted, addressed human messages. It is
// idempotent and never re-enrolls a removed message on replay or catch-up.
func (e *Engine) Track(m Message) error {
	if e == nil || !e.opts.Enabled {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m.ID == "" || m.Conversation == "" {
		return errors.New("never-silent: missing message coordinates")
	}
	if _, ok := e.state.Messages[m.ID]; ok {
		return nil
	}
	if m.Created.IsZero() {
		m.Created = e.opts.Now()
	}
	return e.change(func(s *state) { s.Messages[m.ID] = m })
}

// Result records an observed answer, never the age of a missing reply.
// pending (including an HTTP 200 pending) breaks both failure streaks.
func (e *Engine) Result(id, kind string, targets []string) error {
	return e.ResultAt(id, kind, targets, time.Time{})
}

// ResultAt replays a primary journal observation without advancing its clock.
// Older records cannot resurrect a stop after a newer delivery.
func (e *Engine) ResultAt(id, kind string, targets []string, at time.Time) error {
	if e == nil || !e.opts.Enabled {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.state.Messages[id]
	if !ok || m.Removed {
		return nil
	}
	now := at
	if now.IsZero() {
		now = e.opts.Now()
	}
	if !at.IsZero() && (now.Before(m.Observed) || (now.Equal(m.Observed) && kind == m.ObservedKind && (len(targets) == 0 || slices.Equal(targets, m.Targets)))) {
		return nil
	}
	m.Observed = now
	m.ObservedKind = kind
	// A newer delivery attempt supersedes terminal and positive evidence from
	// the previous attempt. Replayed identical observations return above.
	m.Stopped = time.Time{}
	m.Uncertain = false
	if kind != "delivered" {
		m.Delivered = time.Time{}
	}
	if len(targets) > 0 {
		m.Targets = slices.Clone(targets)
	}
	switch kind {
	case "failed", "unreachable":
		if m.FailureKind != kind || m.FailureSince.IsZero() {
			m.FailureSince = now
		}
		m.FailureKind = kind
	default:
		m.FailureKind = ""
		m.FailureSince = time.Time{}
	}
	if kind == "delivered" {
		if m.Delivered.IsZero() {
			m.Delivered = now
		}
		m.Stopped = time.Time{}
	}
	return e.change(func(s *state) { s.Messages[id] = m })
}

func (e *Engine) DefinitelyFailed(id string) bool {
	if e == nil || !e.opts.Enabled {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Messages[id].FailureKind == "failed"
}

func (e *Engine) Stop(id string, definite bool) error {
	return e.StopAt(id, definite, time.Time{})
}

func (e *Engine) StopAt(id string, definite bool, at time.Time) error {
	if e == nil || !e.opts.Enabled {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.state.Messages[id]
	if !ok || m.Removed {
		return nil
	}
	if at.IsZero() {
		at = e.opts.Now()
	}
	if at.Before(m.Observed) {
		return nil
	}
	if !m.Stopped.IsZero() && (!definite || !m.Uncertain) {
		return nil
	}
	m.Observed = at
	if m.Stopped.IsZero() {
		m.Stopped = at
		m.Uncertain = !definite
	}
	if definite {
		m.Uncertain = false
	}
	return e.change(func(s *state) { s.Messages[id] = m })
}

func (e *Engine) Enabled() bool { return e != nil && e.opts.Enabled }

func (e *Engine) Forget(id string) error {
	if e == nil || !e.opts.Enabled {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.state.Messages[id]
	if !ok || m.Removed {
		return nil
	}
	m.Removed = true
	return e.change(func(s *state) { s.Messages[id] = m })
}

func (e *Engine) Act(conversation, thread, session string, at time.Time) error {
	if e == nil || !e.opts.Enabled || session == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.state.Acts {
		if a.Conversation == conversation && a.Session == session && a.At.Equal(at) {
			return nil
		}
	}
	return e.change(func(s *state) { s.Acts = append(s.Acts, act{conversation, thread, session, at}) })
}

func (e *Engine) Reaction(id, session string, at time.Time) error {
	if e == nil || !e.opts.Enabled || session == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.state.Messages[id]
	if !ok {
		return nil
	}
	return e.change(func(s *state) { s.Acts = append(s.Acts, act{m.Conversation, m.Thread, session, at}) })
}

func (e *Engine) answered(m Message) bool {
	for _, a := range e.state.Acts {
		if a.Conversation == m.Conversation && !a.At.Before(m.Created) && (len(m.Targets) == 0 || slices.Contains(m.Targets, a.Session)) {
			return true
		}
	}
	return false
}

func (e *Engine) cause(m Message, h Health, now time.Time) (string, bool) {
	if m.Removed || e.answered(m) {
		return "", false
	}
	if !m.Stopped.IsZero() {
		return "not_delivered", !m.Uncertain || now.Sub(m.Stopped) >= FailureWait
	}
	if !m.FailureSince.IsZero() && now.Sub(m.FailureSince) >= FailureWait {
		return "not_delivered", true
	}
	if !m.Delivered.IsZero() && now.Sub(m.Delivered) >= FailureWait && meanings[h.State] != "" {
		return h.State, true
	}
	return "", false
}

func line(cause string) string {
	switch cause {
	case "no_seat":
		return NoSeatLine
	case "seat_at_cap":
		return CapLine
	default:
		return DownLine
	}
}

func (e *Engine) Poll(ctx context.Context) error {
	if e == nil || !e.opts.Enabled {
		return nil
	}
	e.pollMu.Lock()
	defer e.pollMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.opts.Now()
	health := ReadHealth(e.opts.HealthFile, now, HealthMaxAge)
	groups := map[string][]Message{}
	ready := map[string]bool{}
	for _, m := range e.state.Messages {
		cause, post := e.cause(m, health, now)
		if cause != "" {
			groups[cause] = append(groups[cause], m)
			ready[m.ID] = post
		}
	}
	for cause := range groups {
		sort.Slice(groups[cause], func(i, j int) bool {
			a, b := groups[cause][i], groups[cause][j]
			if a.Created.Equal(b.Created) {
				return a.ID < b.ID
			}
			return a.Created.Before(b.Created)
		})
	}
	type ownerPost struct {
		message Message
		line    string
	}
	var posts []ownerPost
	newOutages := 0
	err := e.change(func(s *state) {
		for i := range s.Outages {
			o := &s.Outages[i]
			if o.End.IsZero() && len(groups[o.Cause]) == 0 {
				o.End = now
			}
		}
		for _, cause := range []string{"not_delivered", "no_seat", "seat_at_cap", "session_down"} {
			messages := groups[cause]
			if len(messages) == 0 {
				continue
			}
			index := -1
			for i, o := range s.Outages {
				if o.Cause == cause && o.End.IsZero() {
					index = i
					break
				}
			}
			if index < 0 {
				s.Outages = append(s.Outages, Outage{ID: randomID(), Cause: cause, Start: now})
				index = len(s.Outages) - 1
				newOutages++
			}
			o := &s.Outages[index]
			o.Messages = len(messages)
			o.Oldest = messages[0].Created
			o.Health = health
			convs := map[string]bool{}
			for _, m := range messages {
				convs[m.Conversation] = true
				mark, exists := s.Owners[m.Conversation]
				if ready[m.ID] && !m.Notified {
					m.Notified = true
					s.Messages[m.ID] = m
					if !exists || now.Sub(mark.At) >= OwnerCooldown {
						text := line(cause)
						s.Owners[m.Conversation] = ownerMark{At: now, Line: text, Message: m}
						posts = append(posts, ownerPost{m, text})
					}
				}
			}
			o.Conversations = len(convs)
		}
	})
	if err != nil {
		return err
	}
	if e.opts.OperatorContext == "" {
		for i := 0; i < newOutages; i++ {
			e.opts.Log("never-silent: operator context unset; no operator mail will be sent")
		}
	}
	for _, p := range posts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cause, ready := e.cause(e.state.Messages[p.message.ID], ReadHealth(e.opts.HealthFile, e.opts.Now(), HealthMaxAge), e.opts.Now())
		if cause == "" || !ready || line(cause) != p.line {
			continue
		}
		p.message.CanPost = func() bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			now := e.opts.Now()
			cause, ready := e.cause(e.state.Messages[p.message.ID], ReadHealth(e.opts.HealthFile, now, HealthMaxAge), now)
			return ready && cause != "" && line(cause) == p.line
		}
		if err := e.effect(func() error { return e.opts.Post(ctx, p.message, p.line) }); err != nil {
			e.opts.Log("never-silent: owner post refused or uncertain; reserved mark retained")
			continue
		}
		if err = e.change(func(s *state) {
			mark := s.Owners[p.message.Conversation]
			mark.Posted = true
			s.Owners[p.message.Conversation] = mark
			s.Posted = append(s.Posted, now)
		}); err != nil {
			return err
		}
	}
	if err = e.change(func(s *state) {
		for i := range s.Outages {
			o := &s.Outages[i]
			if !o.End.IsZero() {
				continue
			}
			seen := map[string]bool{}
			for _, m := range groups[o.Cause] {
				mark := s.Owners[m.Conversation]
				if mark.Posted && now.Sub(mark.At) < OwnerCooldown {
					seen[m.Conversation] = true
				}
			}
			o.OwnersTold = len(seen)
		}
		s.Posted = slices.DeleteFunc(s.Posted, func(t time.Time) bool { return now.Sub(t) > 7*24*time.Hour })
	}); err != nil {
		return err
	}
	if e.opts.OperatorContext == "" {
		return e.sendLocal(ctx, now)
	}
	for i := range e.state.Outages {
		for _, cleared := range []bool{false, true} {
			o := e.state.Outages[i]
			mark := o.Mail
			start := o.Start
			if cleared {
				if o.End.IsZero() {
					continue
				}
				mark = o.ClearedMail
				start = o.End
			}
			if mark.Sent || now.Sub(start) > MailLifetime || (!mark.Tried.IsZero() && now.Sub(mark.Tried) < MailRetry) {
				continue
			}
			if err = e.change(func(s *state) {
				if cleared {
					s.Outages[i].ClearedMail.Tried = now
				} else {
					s.Outages[i].Mail.Tried = now
				}
			}); err != nil {
				return err
			}
			mail := e.mail(o, cleared, now)
			if err = e.effect(func() error { return e.opts.Send(ctx, mail) }); err != nil {
				e.opts.Log("never-silent: operator mail failed; retry in one minute")
				continue
			}
			if err = e.change(func(s *state) {
				if cleared {
					s.Outages[i].ClearedMail.Sent = true
				} else {
					s.Outages[i].Mail.Sent = true
				}
			}); err != nil {
				return err
			}
		}
	}
	return e.sendLocal(ctx, now)
}

// A local follow-up tells a returning agent what its owner read. It is separate
// from the existing stuck mail, which may already have gone out a minute before
// the owner line became due. Only the fixed line and local coordinates are used.
func (e *Engine) sendLocal(ctx context.Context, now time.Time) error {
	if e.opts.SendLocal == nil {
		return nil
	}
	for conversation, mark := range e.state.Owners {
		if !mark.Posted || mark.LocalMail.Sent || now.Sub(mark.At) > MailLifetime || (!mark.LocalMail.Tried.IsZero() && now.Sub(mark.LocalMail.Tried) < MailRetry) {
			continue
		}
		if err := e.change(func(s *state) { m := s.Owners[conversation]; m.LocalMail.Tried = now; s.Owners[conversation] = m }); err != nil {
			return err
		}
		if err := e.effect(func() error { return e.opts.SendLocal(ctx, mark.Message, mark.Line) }); err != nil {
			e.opts.Log("never-silent: local owner notice mail failed; retry in one minute")
			continue
		}
		if err := e.change(func(s *state) { m := s.Owners[conversation]; m.LocalMail.Sent = true; s.Owners[conversation] = m }); err != nil {
			return err
		}
	}
	return nil
}

// Polls are serialized separately; observations and status remain available
// while a channel or mail command is slow. Call with mu held.
func (e *Engine) effect(fn func() error) error {
	e.mu.Unlock()
	defer e.mu.Lock()
	return fn()
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (e *Engine) mail(o Outage, cleared bool, now time.Time) Mail {
	subject := fmt.Sprintf("never-silent: %s: %s: %s", e.opts.City, e.opts.Channel, o.Cause)
	key := "never-silent:" + o.ID
	var b strings.Builder
	fmt.Fprintf(&b, "city: %s\nchannel: %s\ncause: %s — %s\nstarted: %s\n", e.opts.City, e.opts.Channel, o.Cause, meanings[o.Cause], o.Start.UTC().Format(time.RFC3339))
	if cleared {
		subject += " cleared"
		key += ":cleared"
		fmt.Fprintf(&b, "ended: %s\n", o.End.UTC().Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "conversations: %d\nmessages: %d\noldest_age_seconds: %.0f\nowners_told: %t (%d conversations)\n", o.Conversations, o.Messages, max(0, now.Sub(o.Oldest).Seconds()), o.OwnersTold > 0, o.OwnersTold)
		if o.Cause != "not_delivered" {
			fmt.Fprintf(&b, "health: %s\nchecked_at: %s\n", o.Health.State, o.Health.CheckedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "status: gc %s status --json\n", e.opts.Channel)
	}
	return Mail{Subject: subject, Body: b.String(), Key: key}
}

func (e *Engine) Status() Status {
	if e == nil {
		return Status{Health: Health{State: "unknown", Reason: "disabled"}, Outages: []Outage{}}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.opts.Now()
	s := Status{Enabled: e.opts.Enabled, Health: ReadHealth(e.opts.HealthFile, now, HealthMaxAge), Outages: []Outage{}}
	for _, o := range e.state.Outages {
		if o.End.IsZero() {
			s.Outages = append(s.Outages, o)
		}
	}
	for _, at := range e.state.Posted {
		if now.Sub(at) <= 7*24*time.Hour {
			s.OwnerLinesSevenDays++
		}
	}
	return s
}

func (e *Engine) OwnerLine(conversation string) string {
	if e == nil || !e.opts.Enabled {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.state.Owners[conversation]
	if m.Posted && e.opts.Now().Sub(m.At) < OwnerCooldown {
		return m.Line
	}
	return ""
}
