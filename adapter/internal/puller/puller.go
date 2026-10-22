// Package puller decodes Workspace CloudEvents and owns Pub/Sub acknowledgments.
package puller

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"cloud.google.com/go/pubsub/v2"
	chat "google.golang.org/api/chat/v1"
	events "google.golang.org/api/workspaceevents/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Event struct {
	PubSubID, CloudEventID, Type, Subject string
	Changes                               []Change
	Raw                                   []byte
}

type Change struct {
	Kind         string
	Message      *chat.Message
	Reaction     *chat.Reaction
	Membership   *chat.Membership
	Subscription *events.Subscription
	Space        string
}

type Outcome uint8

const (
	// Nack is the safe zero value: an unset outcome must never discard an event.
	Nack Outcome = iota
	Ack
	Quarantine
)

// Handler returns Ack only after EVERY change is terminal or durably retained
// with a recovery obligation. A transient error overrides Ack. An error with
// Quarantine describes the permanent failure and is recorded with the raw data.
// Run invokes Handler synchronously, keeping the client's lease management alive.
type Handler func(context.Context, *Event) (Outcome, error)

// Logger is implemented by slog.Logger. A nil logger uses slog.Default().
type Logger interface {
	Info(string, ...any)
	Error(string, ...any)
}

// RunOptions supplies the storage configuration missing from the transport.
// The optional callback lets the application persist quarantine status in its
// ledger. It must be idempotent by PubSubID: failed writes are redelivered.
type RunOptions struct {
	MediaDir         string
	RecordQuarantine func(ctx context.Context, ev *Event, path string, cause error) error
	// InitialBackoff and MaxBackoff bound full-jitter exponential reconnection
	// delays. Zero uses one second and one minute respectively.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

// Decode reads a binary-mode CloudEvent: ce-* attributes carry the envelope and
// Data carries the JSON resource wrapper(s). Unknown types need no JSON parsing.
// On a malformed known event it returns its envelope and raw bytes, but no
// partial changes, so the caller can preserve the entire event in quarantine.
func Decode(msg *pubsub.Message) (*Event, error) {
	if msg == nil {
		return nil, errors.New("nil Pub/Sub message")
	}
	ev := &Event{
		PubSubID: msg.ID, CloudEventID: msg.Attributes["ce-id"],
		Type: msg.Attributes["ce-type"], Subject: msg.Attributes["ce-subject"],
		Raw: append([]byte(nil), msg.Data...),
	}
	if ev.Type == "" {
		return ev, errors.New("missing ce-type")
	}
	resource, action, batch := eventKind(ev.Type)
	if resource == "" {
		return ev, nil
	}
	items := []json.RawMessage{msg.Data}
	if batch {
		// RawMessage.UnmarshalJSON reuses its destination buffer. Do not seed
		// the batch destination with the caller's data.
		items = nil
		var body map[string]json.RawMessage
		if err := json.Unmarshal(msg.Data, &body); err != nil {
			return ev, fmt.Errorf("decode %s: %w", ev.Type, err)
		}
		if err := json.Unmarshal(body[resource+"s"], &items); err != nil {
			return ev, fmt.Errorf("decode %s wrappers: %w", ev.Type, err)
		}
		if len(items) == 0 {
			return ev, fmt.Errorf("empty %s batch", resource)
		}
	}
	changes := make([]Change, 0, len(items))
	for i, item := range items {
		change, err := decodeChange(item, resource, action)
		if err != nil {
			return ev, fmt.Errorf("decode %s change %d: %w", ev.Type, i, err)
		}
		changes = append(changes, change)
	}
	ev.Changes = changes
	return ev, nil
}

func eventKind(typ string) (resource, action string, batch bool) {
	const lifecycle = "google.workspace.events.subscription.v1."
	if strings.HasPrefix(typ, lifecycle) {
		action = strings.TrimPrefix(typ, lifecycle)
		switch action {
		case "expirationReminder", "expired", "suspended":
			return "subscription", action, false
		}
		return "", "", false
	}
	for _, resource = range []string{"message", "reaction", "membership"} {
		prefix := "google.workspace.chat." + resource + ".v1."
		if !strings.HasPrefix(typ, prefix) {
			continue
		}
		action = strings.TrimPrefix(typ, prefix)
		switch action {
		case "batchCreated":
			action, batch = "created", true
		case "batchUpdated":
			action, batch = "updated", true
		case "batchDeleted":
			action, batch = "deleted", true
		}
		if action == "created" || action == "deleted" || (action == "updated" && resource != "reaction") {
			return resource, action, batch
		}
	}
	return "", "", false
}

func decodeChange(raw json.RawMessage, resource, action string) (Change, error) {
	var body struct {
		Message      *chat.Message        `json:"message"`
		Reaction     *chat.Reaction       `json:"reaction"`
		Membership   *chat.Membership     `json:"membership"`
		Subscription *events.Subscription `json:"subscription"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Change{}, err
	}
	c := Change{Kind: resource + "." + action}
	var name string
	switch resource {
	case "message":
		c.Message = body.Message
		if c.Message != nil {
			name = c.Message.Name
		}
	case "reaction":
		c.Reaction = body.Reaction
		if c.Reaction != nil {
			name = c.Reaction.Name
		}
	case "membership":
		c.Membership = body.Membership
		if c.Membership != nil {
			name = c.Membership.Name
		}
	case "subscription":
		c.Subscription = body.Subscription
		if c.Subscription != nil {
			name = c.Subscription.Name
		}
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return Change{}, fmt.Errorf("missing or invalid %s name %q", resource, name)
		}
	}
	valid := false
	switch resource {
	case "message":
		valid = len(parts) == 4 && parts[0] == "spaces" && parts[2] == "messages"
	case "reaction":
		valid = len(parts) == 6 && parts[0] == "spaces" && parts[2] == "messages" && parts[4] == "reactions"
	case "membership":
		valid = len(parts) == 4 && parts[0] == "spaces" && parts[2] == "members"
	case "subscription":
		valid = len(parts) == 2 && parts[0] == "subscriptions"
	}
	if !valid {
		return Change{}, fmt.Errorf("invalid %s name %q", resource, name)
	}
	if resource != "subscription" {
		c.Space = strings.Join(parts[:2], "/")
	}
	return c, nil
}

// Run owns sub until cancellation or a fatal receive error; do not use sub
// concurrently. Exactly one streaming-pull connection and one in-flight handler
// keep the ledger updates serialized. The Pub/Sub client automatically retries
// in-stream failures and extends leases while the callback is running. Run
// reconnects after recoverable Receive errors with full-jitter backoff. Fatal
// permission, authentication, configuration and missing-subscription errors
// return to the application for credential/subscription reconciliation.
// A quarantine storage/record failure always nacks. Without MediaDir quarantine
// fails closed, so callers should supply their configured directory at startup.
func Run(ctx context.Context, sub *pubsub.Subscriber, h Handler, log Logger, options ...RunOptions) error {
	if sub == nil || h == nil {
		return errors.New("puller requires a subscriber and handler")
	}
	if len(options) > 1 {
		return errors.New("puller accepts at most one RunOptions")
	}
	var opts RunOptions
	if len(options) == 1 {
		opts = options[0]
	}
	if log == nil {
		log = slog.Default()
	}
	if opts.InitialBackoff == 0 {
		opts.InitialBackoff = time.Second
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = time.Minute
	}
	if opts.InitialBackoff < 0 || opts.MaxBackoff < opts.InitialBackoff {
		return errors.New("puller backoff requires 0 < InitialBackoff <= MaxBackoff")
	}
	sub.ReceiveSettings.NumGoroutines = 1
	sub.ReceiveSettings.MaxOutstandingMessages = 1
	if sub.ReceiveSettings.MaxExtension <= 0 {
		sub.ReceiveSettings.MaxExtension = pubsub.DefaultReceiveSettings.MaxExtension
	}
	var delivered atomic.Bool
	handle := func(ctx context.Context, msg *pubsub.Message) {
		delivered.Store(true)
		ev, err := Decode(msg)
		outcome := Quarantine
		if err == nil {
			if len(ev.Changes) == 0 {
				log.Info("unknown event acknowledged", "pubsub_id", ev.PubSubID, "type", ev.Type, "subject", ev.Subject)
				msg.Ack()
				return
			}
			outcome, err = h(ctx, ev)
		}
		if outcome == Quarantine {
			path, writeErr := quarantine(opts.MediaDir, ev)
			if writeErr == nil && opts.RecordQuarantine != nil {
				writeErr = opts.RecordQuarantine(ctx, ev, path, err)
			}
			if writeErr != nil {
				log.Error("quarantine failed", "pubsub_id", ev.PubSubID, "subject", ev.Subject, "cause", writeErr)
				msg.Nack()
				return
			}
			log.Info("event quarantined", "pubsub_id", ev.PubSubID, "subject", ev.Subject, "path", path, "cause", err)
			msg.Ack()
			return
		}
		if err != nil || outcome != Ack {
			log.Error("event nacked", "pubsub_id", ev.PubSubID, "subject", ev.Subject, "outcome", outcome, "cause", err)
			msg.Nack()
			return
		}
		msg.Ack()
	}
	backoff := receiveBackoff{initial: opts.InitialBackoff, max: opts.MaxBackoff}
	for ctx.Err() == nil {
		delivered.Store(false)
		started := time.Now()
		err := sub.Receive(ctx, handle)
		if ctx.Err() != nil {
			return nil // Preserve Receive's graceful cancellation contract.
		}
		if !recoverableReceiveError(err) {
			return err
		}
		delay := backoff.pause(time.Since(started), delivered.Load())
		log.Error("pubsub receive retry", "code", status.Code(err).String(), "cause", err, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func recoverableReceiveError(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.NotFound, codes.Unauthenticated, codes.FailedPrecondition, codes.InvalidArgument:
		return false
	default:
		// Includes non-status transport errors (Unknown). Only the application
		// can repair the fatal codes above; other failures reconnect here.
		return true
	}
}

type receiveBackoff struct {
	initial, max, current time.Duration
}

func (b *receiveBackoff) pause(stretch time.Duration, delivered bool) time.Duration {
	if b.current == 0 || stretch >= time.Minute || delivered {
		b.current = b.initial
	}
	delay := time.Duration(rand.Int64N(int64(b.current)))
	if b.current > b.max/2 {
		b.current = b.max
	} else {
		b.current *= 2
	}
	return delay
}

func quarantine(mediaDir string, ev *Event) (string, error) {
	if mediaDir == "" {
		return "", errors.New("quarantine requires MediaDir")
	}
	if ev.PubSubID == "" {
		return "", errors.New("quarantine requires Pub/Sub message ID")
	}
	parent, err := openDurableDirectory(filepath.Dir(filepath.Clean(mediaDir)))
	if err != nil {
		return "", err
	}
	defer parent.Close()
	root, err := openDurableChild(parent, "quarantine")
	if err != nil {
		return "", err
	}
	defer root.Close()
	// Escape the untrusted transport ID into one filename, including any slash.
	name := url.PathEscape(ev.PubSubID) + ".json"
	if err := writeQuarantine(root, name, ev.Raw); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Clean(mediaDir)), "quarantine", name), nil
}

func writeQuarantine(root *os.Root, name string, raw []byte) error {
	temp := ".event-" + cryptorand.Text() + ".tmp"
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	return syncDirectory(root)
}

// Walk from the filesystem root using directory handles, refusing symlinks in
// every component (including existing ancestors). Each opened child must match
// its lstat identity, so a concurrent replacement cannot redirect the walk.
func openDurableDirectory(path string) (*os.Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs) + string(filepath.Separator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(abs, volume), string(filepath.Separator)) {
		if name == "" {
			continue
		}
		child, err := openDurableChild(root, name)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = child
	}
	return root, nil
}

// Persist each parent entry, including existing directories that may have been
// created by an earlier attempt whose sync failed, before creating children.
func openDurableChild(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("quarantine path is not a directory (symlinks are refused): %s", name)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err == nil && !os.SameFile(info, opened) {
		err = fmt.Errorf("quarantine directory changed while opening: %s", name)
	}
	if err == nil {
		err = syncDirectory(parent)
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func syncDirectory(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
