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
	PubSubID string
	// CloudEventID may be empty when ce-id is absent. Deduplication must treat
	// an empty value as absent and fall back to PubSubID.
	CloudEventID  string
	Type, Subject string
	Changes       []Change
	Raw           []byte
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
// Handlers must be idempotent by PubSubID: after a stream failure neither Ack nor
// Nack reaches the server, and the message returns when its lease expires.
// Run waits for the in-flight handler on stop or reconnect without a time limit;
// handlers must return promptly when their context is cancelled.
// Returned errors are logged verbatim as the cause and must not quote message text.
type Handler func(context.Context, *Event) (Outcome, error)

// Logger is implemented by slog.Logger. A nil logger uses slog.Default().
type Logger interface {
	Info(string, ...any)
	Warn(string, ...any)
	Error(string, ...any)
}

// RunOptions supplies the storage configuration missing from the transport.
// The optional callback lets the application persist quarantine status in its
// ledger. It must be idempotent by PubSubID: failed writes are redelivered.
type RunOptions struct {
	// MediaDir and all its parent directory components must be free of symlinks.
	// Quarantine uses the sibling directory "quarantine". Before recording a
	// path, Run checks that this location still names the opened directory;
	// a move, removal or storage error triggers one verified reopen and retry.
	MediaDir         string
	RecordQuarantine func(ctx context.Context, ev *Event, path string, cause error) error
	// InitialBackoff and MaxBackoff are the initial and maximum ceilings for
	// full-jitter exponential reconnection delays, drawn from [0, ceiling).
	// Zero uses ceilings of one second and one minute respectively.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// MaxDurationPerAckExtension caps each lease extension, bounding how long a
	// dropped ack can delay redelivery after a reconnect. Zero preserves sub's
	// setting (and the client default); a positive value must be 10s to 600s.
	// Run rejects a subscriber MinDurationPerAckExtension greater than the
	// effective positive maximum, whether supplied here or on the subscriber.
	MaxDurationPerAckExtension time.Duration
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
// concurrently. Cancel ctx before closing the client. A closed client returns
// codes.Canceled when ctx is still live. Exactly one connection and one handler
// keep the ledger updates serialized. The Pub/Sub client automatically retries
// in-stream failures and extends leases while the callback is running. Run
// reconnects after recoverable Receive errors with full-jitter backoff. Fatal
// permission, authentication, configuration and missing-subscription errors
// return to the application for credential/subscription reconciliation.
// Handlers must be idempotent by PubSubID: after a stream failure neither Ack nor
// Nack reaches the server, and the message returns when its lease expires even
// if handling succeeded. MaxDurationPerAckExtension can shorten that delay.
// The caller's ReceiveSettings.ShutdownOptions is discarded. On stop or
// reconnect, Run waits without limit for the in-flight handler, which must
// return promptly on context cancellation.
// A negative MaxExtension (the client's disable setting) is refused: any
// nonpositive value is replaced by the client default so handling extends leases.
// An unrecovered quarantine storage failure or a record failure nacks. Without
// MediaDir quarantine fails closed, so callers should supply it at startup.
// The quarantine directory is verified at startup and its handle is reused.
// Before returning a recorded path, its configured location must still name
// the opened directory. A missing store, changed directory or write failure
// triggers one fresh verified walk and retry; a successful retry replaces the
// cached handle. Startup storage failures warn but can recover on a later
// quarantine in the same Run; other events can still be handled.
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
	// Freeze relative configuration before the first open, including retries
	// after a startup failure or a later process working-directory change.
	if opts.MediaDir != "" {
		var err error
		opts.MediaDir, err = filepath.Abs(opts.MediaDir)
		if err != nil {
			return fmt.Errorf("resolve quarantine MediaDir: %w", err)
		}
	}
	storage, storageErr := openQuarantine(opts.MediaDir)
	defer func() {
		if storage != nil {
			storage.root.Close()
		}
	}()
	var warnings []string
	if opts.MediaDir == "" {
		warnings = append(warnings, "MediaDir is empty: malformed events and Quarantine outcomes will be nacked")
	} else if storageErr != nil {
		warnings = append(warnings, fmt.Sprintf("quarantine storage unavailable: %v; recovery will be attempted on each quarantine; events will be nacked only if recovery fails", storageErr))
	}
	if opts.RecordQuarantine == nil {
		warnings = append(warnings, "RecordQuarantine is nil: quarantines will not be counted")
	}
	if len(warnings) > 0 {
		log.Warn("quarantine configuration incomplete", "consequence", strings.Join(warnings, "; "))
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
	if d := opts.MaxDurationPerAckExtension; d != 0 && (d < 10*time.Second || d > 600*time.Second) {
		return errors.New("puller MaxDurationPerAckExtension must be zero or between 10s and 600s")
	}
	maxAckExtension := opts.MaxDurationPerAckExtension
	if maxAckExtension == 0 {
		maxAckExtension = sub.ReceiveSettings.MaxDurationPerAckExtension
	}
	if maxAckExtension > 0 && sub.ReceiveSettings.MinDurationPerAckExtension > maxAckExtension {
		return errors.New("puller MinDurationPerAckExtension must not exceed MaxDurationPerAckExtension")
	}
	sub.ReceiveSettings.NumGoroutines = 1
	sub.ReceiveSettings.MaxOutstandingMessages = 1
	// Timed shutdown may return before the handler finishes, allowing the next
	// Receive to overlap it. Always wait for processing to drain on reconnect.
	sub.ReceiveSettings.ShutdownOptions = nil
	if opts.MaxDurationPerAckExtension != 0 {
		sub.ReceiveSettings.MaxDurationPerAckExtension = opts.MaxDurationPerAckExtension
	}
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
			var path string
			var writeErr error
			if storage != nil {
				path, writeErr = storage.write(ev)
			}
			if storage == nil || writeErr != nil {
				// Retry once through the same verified walk used at startup.
				// Keep a failed replacement out of the cache and close every
				// superseded handle; handlers are serialized by Receive.
				var fresh *quarantineStore
				fresh, writeErr = openQuarantine(opts.MediaDir)
				if writeErr == nil {
					path, writeErr = fresh.write(ev)
					if writeErr == nil {
						if storage != nil {
							storage.root.Close()
						}
						storage = fresh
					} else {
						fresh.root.Close()
					}
				}
			}
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
	case codes.Canceled, codes.PermissionDenied, codes.NotFound, codes.Unauthenticated, codes.FailedPrecondition, codes.InvalidArgument:
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

// quarantineDirectory keeps writes anchored to the opened directory. The
// interface also lets tests rotate real directories at exact I/O boundaries.
type quarantineDirectory interface {
	Stat(string) (os.FileInfo, error)
	Open(string) (*os.File, error)
	OpenFile(string, int, os.FileMode) (*os.File, error)
	Rename(string, string) error
	Remove(string) error
	Close() error
}

type quarantineStore struct {
	root quarantineDirectory
	path string
}

func openQuarantine(mediaDir string) (*quarantineStore, error) {
	if mediaDir == "" {
		return nil, errors.New("quarantine requires MediaDir")
	}
	path, err := filepath.Abs(filepath.Join(filepath.Dir(filepath.Clean(mediaDir)), "quarantine"))
	if err != nil {
		return nil, err
	}
	root, err := openDurableDirectory(path)
	if err != nil {
		return nil, err
	}
	return &quarantineStore{root: root, path: path}, nil
}

func (q *quarantineStore) write(ev *Event) (string, error) {
	if ev.PubSubID == "" {
		return "", errors.New("quarantine requires Pub/Sub message ID")
	}
	if err := q.verifyPath(); err != nil {
		return "", err
	}
	// Escape the untrusted transport ID into one filename, including any slash.
	name := url.PathEscape(ev.PubSubID) + ".json"
	if err := writeQuarantine(q.root, name, ev.Raw); err != nil {
		return "", err
	}
	// A rotation during the write must not hand the recorder a stale path.
	if err := q.verifyPath(); err != nil {
		return "", err
	}
	return filepath.Join(q.path, name), nil
}

func (q *quarantineStore) verifyPath() error {
	// SameFile alone cannot detect a parent changed to a symlink pointing back
	// into the opened tree. Recheck the resolved absolute path as well. Writes
	// remain anchored to the open root even if a swap races these checks.
	resolved, err := filepath.EvalSymlinks(q.path)
	if err != nil {
		return err
	}
	if resolved != q.path {
		return fmt.Errorf("quarantine path contains a symlink: %s", q.path)
	}
	info, err := os.Lstat(q.path)
	if err != nil {
		return err
	}
	opened, err := q.root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return fmt.Errorf("quarantine directory changed: %s", q.path)
	}
	return nil
}

func writeQuarantine(root quarantineDirectory, name string, raw []byte) error {
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
