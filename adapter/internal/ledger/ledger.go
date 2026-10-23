// Package ledger persists the adapter's delivery state. Callers must serialize
// access to a Ledger, including mutations and saves to the same path.
package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gchat-adapter/internal/wire"
)

const transportLimit = 10000

var ErrRecoveryRequired = errors.New("ledger recovery required")

// RecoveryError identifies the affected ledger and, when a copy was possible,
// the preserved bytes. No usable Ledger is returned with this error.
type RecoveryError struct {
	Path          string
	PreservedPath string
	Err           error
}

func (e *RecoveryError) Error() string {
	if e.PreservedPath != "" {
		return fmt.Sprintf("%v: %s: %v (preserved at %s)", ErrRecoveryRequired, e.Path, e.Err, e.PreservedPath)
	}
	return fmt.Sprintf("%v: %s: %v", ErrRecoveryRequired, e.Path, e.Err)
}

func (e *RecoveryError) Is(target error) bool { return target == ErrRecoveryRequired }
func (e *RecoveryError) Unwrap() error        { return e.Err }

type Ledger struct {
	saveGuard func() error

	Version        int                      `json:"version"`
	IdentityUserID string                   `json:"identity_user_id,omitempty"`
	IdentitySeed   string                   `json:"identity_seed,omitempty"`
	Subscription   SubscriptionState        `json:"subscription"`
	Spaces         map[string]*SpaceState   `json:"spaces"`
	Transport      TransportDedup           `json:"transport"`
	Messages       map[string]*MessageState `json:"messages"`
	Changes        map[string]*ChangeState  `json:"changes"`
	Sends          map[string]*SendState    `json:"sends"`
	Unresolved     map[string]*ChangeState  `json:"unresolved"`
	Quarantine     []QuarantinedEvent       `json:"quarantine"`
	Counters       map[string]int64         `json:"counters"`
}

type SubscriptionState struct {
	Name   string    `json:"name"`
	State  string    `json:"state"`
	Expiry time.Time `json:"expiry"`
	Target string    `json:"target"`
}

type SpaceState struct {
	Kind string `json:"kind"`
	// ChatType distinguishes named SPACE from GROUP_CHAT; both have gc kind room.
	ChatType      string    `json:"chat_type,omitempty"`
	Wake          string    `json:"wake"`
	MessageCursor time.Time `json:"message_cursor"`
	EventCursor   time.Time `json:"event_cursor"`
	Backlog       []string  `json:"backlog"`
	PageToken     string    `json:"page_token"`
	FrozenUpper   time.Time `json:"frozen_upper"`
}

type MessageState struct {
	Revision time.Time `json:"revision"`
	Deleted  bool      `json:"deleted"`
	Thread   string    `json:"thread"`
	Mine     bool      `json:"mine"`
	SendKey  string    `json:"send_key"`
}

type ChangeState struct {
	ID        string    `json:"id"`
	Space     string    `json:"space"`
	Message   string    `json:"message"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"` // intent | pending | committed | unresolved
	ReceiptID string    `json:"receipt_id"`
	Attempts  int       `json:"attempts"`
	Created   time.Time `json:"created"`
	Event     string    `json:"event"`
	Reason    string    `json:"reason"`
	CatchUp   bool      `json:"catch_up,omitempty"` // adapter added the delivery header
}

type SendState struct {
	Key      string               `json:"key"`
	Space    string               `json:"space"`
	Thread   string               `json:"thread"`
	Parts    []SendPart           `json:"parts"`
	Receipt  *wire.PublishReceipt `json:"receipt"`
	Recorded bool                 `json:"recorded"`
	Created  time.Time            `json:"created"`
	// Optional command recovery data; old callback-only intents remain readable.
	Request         *wire.PublishRequest `json:"request,omitempty"`
	Upload          *SendUpload          `json:"upload,omitempty"`
	RecordPending   bool                 `json:"record_pending,omitempty"`
	NotRecordable   bool                 `json:"not_recordable,omitempty"`
	RecordingReason string               `json:"recording_reason,omitempty"`
	// Separate from Receipt: a recording refusal must not downgrade delivery.
	RecordingFailureKind string `json:"recording_failure_kind,omitempty"`
}

type SendUpload struct {
	Path         string `json:"path"`
	ResourceName string `json:"resource_name,omitempty"`
	UploadToken  string `json:"upload_token,omitempty"`
}

type SendPart struct {
	RequestID   string `json:"request_id"`
	Text        string `json:"text"`
	MessageName string `json:"message_name"`
}

type QuarantinedEvent struct {
	PubSubID string    `json:"pubsub_id"`
	Reason   string    `json:"reason"`
	Path     string    `json:"path"`
	At       time.Time `json:"at"`
}

// TransportDedup is a FIFO ring of the last 10000 distinct handled IDs. Callers
// namespace IDs (for example, "pubsub:" and "cloudevent:") before passing them
// to Add or Seen. Only add IDs after all changes have a durable outcome.
// Its zero value is ready for use; it is not safe for concurrent use.
type TransportDedup struct {
	ids  []string
	next int
	seen map[string]struct{}
}

type transportJSON struct {
	IDs  []string `json:"ids"`
	Next *int     `json:"next"`
}

func (d *TransportDedup) Seen(id string) bool {
	_, ok := d.seen[id]
	return ok
}

func (d *TransportDedup) Len() int { return len(d.ids) }

// Add reports whether id was new. Empty IDs and duplicates do not change the
// window or refresh an existing ID's eviction order.
func (d *TransportDedup) Add(id string) bool {
	if id == "" || d.Seen(id) {
		return false
	}
	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}
	if len(d.ids) < transportLimit {
		d.ids = append(d.ids, id)
	} else {
		delete(d.seen, d.ids[d.next])
		d.ids[d.next] = id
		d.next = (d.next + 1) % transportLimit
	}
	d.seen[id] = struct{}{}
	return true
}

func (d TransportDedup) MarshalJSON() ([]byte, error) {
	ids := d.ids
	if ids == nil {
		ids = []string{}
	}
	return json.Marshal(transportJSON{IDs: ids, Next: &d.next})
}

func (d *TransportDedup) UnmarshalJSON(data []byte) error {
	var state transportJSON
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.IDs == nil || state.Next == nil {
		return errors.New("transport ring is missing ids or next")
	}
	if len(state.IDs) > transportLimit || *state.Next < 0 || *state.Next >= transportLimit || (len(state.IDs) < transportLimit && *state.Next != 0) {
		return errors.New("invalid transport ring size or position")
	}
	restored := TransportDedup{ids: []string{}, seen: make(map[string]struct{})}
	for _, id := range state.IDs {
		if !restored.Add(id) {
			return errors.New("empty or duplicate transport id")
		}
	}
	restored.next = *state.Next
	*d = restored
	return nil
}

func New() *Ledger {
	return &Ledger{
		Version:    1,
		Spaces:     make(map[string]*SpaceState),
		Transport:  TransportDedup{ids: []string{}, seen: make(map[string]struct{})},
		Messages:   make(map[string]*MessageState),
		Changes:    make(map[string]*ChangeState),
		Sends:      make(map[string]*SendState),
		Unresolved: make(map[string]*ChangeState),
		Quarantine: []QuarantinedEvent{},
		Counters:   make(map[string]int64),
	}
}

// Load never repairs or overwrites the source file. A leftover .tmp file is
// uncommitted and ignored. Only an absent file on a fresh install starts empty.
func Load(path string, established bool) (*Ledger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !established {
			return New(), nil
		}
		return nil, &RecoveryError{Path: path, Err: err}
	}
	var l *Ledger
	err = json.Unmarshal(data, &l)
	if err == nil {
		err = l.validate()
	}
	// UnmarshalJSON always initializes seen, even for an empty ring. A nil
	// index here means the transport field was absent, not an empty window.
	if err == nil && l.Transport.seen == nil {
		err = errors.New("ledger is missing transport state")
	}
	if err != nil {
		preserved, copyErr := preserve(path, data)
		return nil, &RecoveryError{Path: path, PreservedPath: preserved, Err: errors.Join(err, copyErr)}
	}
	return l, nil
}

func (l *Ledger) validate() error {
	if l == nil || l.Version != 1 {
		return errors.New("expected a version 1 ledger")
	}
	if l.Spaces == nil || l.Messages == nil || l.Changes == nil || l.Sends == nil || l.Unresolved == nil || l.Counters == nil {
		return errors.New("ledger is missing required state maps")
	}
	if hasNullRecord(l.Spaces) || hasNullRecord(l.Messages) || hasNullRecord(l.Changes) || hasNullRecord(l.Sends) || hasNullRecord(l.Unresolved) {
		return errors.New("ledger contains a null state record")
	}
	return nil
}

func hasNullRecord[T any](records map[string]*T) bool {
	for _, record := range records {
		if record == nil {
			return true
		}
	}
	return false
}

// SetSaveGuard installs a process-local ownership check before every write.
// Like mutations, installation must be serialized by the caller. Clone retains
// the guard; JSON persistence deliberately does not serialize it.
func (l *Ledger) SetSaveGuard(guard func() error) { l.saveGuard = guard }

// Replace installs restored state without relinquishing this process's guard.
// The caller must serialize access and relinquish ownership of replacement.
func (l *Ledger) Replace(replacement *Ledger) {
	guard := l.saveGuard
	*l = *replacement
	l.saveGuard = guard
}

// Save writes and syncs path.tmp before atomically replacing path, then syncs
// the containing directory. Callers create the parent directory first. Failed
// writes and renames remove the temporary file, reporting any cleanup error.
func (l *Ledger) Save(path string) error {
	if l.saveGuard != nil {
		if err := l.saveGuard(); err != nil {
			return fmt.Errorf("save ledger %s: %w", path, err)
		}
	}
	if err := l.validate(); err != nil {
		return fmt.Errorf("save ledger %s: %w", path, err)
	}
	data, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode ledger %s: %w", path, err)
	}
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open temporary ledger: %w", err)
	}
	if err := writeAndClose(f, append(data, '\n')); err != nil {
		return fmt.Errorf("write temporary ledger: %w", errors.Join(err, removeIfExists(tmpPath)))
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace ledger: %w", errors.Join(err, removeIfExists(tmpPath)))
	}
	return syncDir(filepath.Dir(path))
}

func preserve(path string, data []byte) (string, error) {
	// Exclusive creation preserves earlier evidence even on repeated loads in
	// the same second. On collision, choose the next unused Unix suffix.
	for stamp := time.Now().Unix(); ; stamp++ {
		copyPath := fmt.Sprintf("%s.corrupt-%d", path, stamp)
		f, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("preserve corrupt ledger: %w", err)
		}
		if err := writeAndClose(f, data); err != nil {
			return "", fmt.Errorf("preserve corrupt ledger %s: %w", copyPath, errors.Join(err, removeIfExists(copyPath)))
		}
		return copyPath, syncDir(filepath.Dir(path))
	}
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeAndClose(f *os.File, data []byte) (err error) {
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func syncDir(path string) (err error) {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open ledger directory: %w", err)
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync ledger directory: %w", err)
	}
	return nil
}

// Established fails closed on lookup errors: only confirmed absence of the
// marker permits initialization of an empty ledger.
func Established(secretsDir string) bool {
	_, err := os.Lstat(filepath.Join(secretsDir, ".gchat-established"))
	return !errors.Is(err, os.ErrNotExist)
}

// MarkEstablished must be called only after the first ledger has been saved.
// The secrets directory must already exist; marker write errors are returned.
func MarkEstablished(secretsDir string) error {
	f, err := os.OpenFile(filepath.Join(secretsDir, ".gchat-established"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("mark ledger established: %w", err)
	}
	if err := writeAndClose(f, nil); err != nil {
		return fmt.Errorf("mark ledger established: %w", err)
	}
	return syncDir(secretsDir)
}

// Clone returns an independent in-memory snapshot. Immutable strings are shared;
// mutable maps, slices and pointed-to records are copied. Callers must hold the
// same lock used for ledger mutations while cloning. Persistence still goes
// through Save, including validation and atomic, synced replacement.
func (l *Ledger) Clone() *Ledger {
	if l == nil {
		return nil
	}
	out := *l
	out.Spaces = copyRecords(l.Spaces)
	for _, space := range out.Spaces {
		if space != nil {
			space.Backlog = slices.Clone(space.Backlog)
		}
	}
	out.Messages = copyRecords(l.Messages)
	out.Changes = copyRecords(l.Changes)
	out.Unresolved = copyRecords(l.Unresolved)
	out.Sends = copyRecords(l.Sends)
	for _, send := range out.Sends {
		if send == nil {
			continue
		}
		send.Parts = slices.Clone(send.Parts)
		if send.Receipt != nil {
			r := *send.Receipt
			r.Metadata = maps.Clone(r.Metadata)
			send.Receipt = &r
		}
		if send.Request != nil {
			r := *send.Request
			r.Metadata = maps.Clone(r.Metadata)
			send.Request = &r
		}
		if send.Upload != nil {
			u := *send.Upload
			send.Upload = &u
		}
	}
	out.Transport.ids = slices.Clone(l.Transport.ids)
	out.Transport.seen = maps.Clone(l.Transport.seen)
	out.Quarantine = slices.Clone(l.Quarantine)
	out.Counters = maps.Clone(l.Counters)
	return &out
}

func copyRecords[T any](records map[string]*T) map[string]*T {
	if records == nil {
		return nil
	}
	out := make(map[string]*T, len(records))
	for key, value := range records {
		if value == nil {
			out[key] = nil
			continue
		}
		copy := *value
		out[key] = &copy
	}
	return out
}
