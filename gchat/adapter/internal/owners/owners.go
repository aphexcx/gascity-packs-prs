// Package owners answers who is an owner of this box, and calls the box's
// root-side door. The adapter keeps no owner, no claim and no try.
//
// Where root's directory /run/gchat-owner-claim exists, owners come from
// root's copy of its owners file alone, trusted only because root owns it;
// any owner the settings name is ignored. Where that directory is absent, the
// owners are the ones the settings name (GCHAT_OWNERS), as before.
//
// The directory is fixed. No setting, environment variable or flag moves it
// or relaxes the trust rule: on a box the settings belong to the same user
// the adapter runs as. Tests stand in for "owned by root" and for the
// directory only through ForTest, which refuses to run outside a test binary.
package owners

import (
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Dir is root's directory on a box: the copy of the owners and the door's
// socket are in it.
const Dir = "/run/gchat-owner-claim"

const (
	copyName = "owners.json"
	doorName = "door.sock"
	// copyLimit holds 50 owners.
	copyLimit = 16384
	// MaxOwners is the most owners a copy may name.
	MaxOwners = 50
	// DoorWait bounds one call to the door, from dialing to its answer.
	DoorWait = 2 * time.Second
	// warnEvery bounds the "gchat owners file" line.
	warnEvery = time.Minute
)

var (
	// ChatUserID is the one shape of a Chat user id.
	ChatUserID = regexp.MustCompile(`^users/[0-9]{1,32}$`)
	// MessageName is the one shape of a message's name (at most 256 characters).
	messageName = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_.-]+$`)
	claimID     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	since       = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

// ValidMessage reports whether name has the one shape of a message's name.
func ValidMessage(name string) bool { return len(name) <= 256 && messageName.MatchString(name) }

// Record is one owner of root's copy. It holds no name and no address.
type Record struct {
	Since string `json:"since"`
	By    string `json:"by"`
	Claim string `json:"claim"`
}

// Entry is one owner as gc gchat owners lists it.
type Entry struct {
	ID    string `json:"id"`
	Since string `json:"since"`
	By    string `json:"by"`
	Claim string `json:"claim"`
}

// Book answers "is this sender an owner". Books made by With share the root
// side (the copy's cache and the log limits) and differ only in the settings.
type Book struct {
	settings []string
	set      map[string]bool
	r        *rootSide
}

type rootSide struct {
	dir    string
	isRoot func(fs.FileInfo) bool
	log    *slog.Logger
	now    func() time.Time
	wait   time.Duration

	mu      sync.Mutex
	have    bool
	key     statKey
	owners  map[string]Record
	warned  time.Time
	ignored bool
}

// New returns the Book of the fixed directory, with the settings' owners
// (addresses or Chat ids). The log may be nil.
func New(settings []string, log *slog.Logger) *Book {
	return newBook(settings, &rootSide{dir: Dir, isRoot: ownedByRoot, log: log, now: time.Now, wait: DoorWait})
}

// TestOptions are the values only a test may set.
type TestOptions struct {
	Settings []string
	// Dir stands in for Dir.
	Dir string
	// IsRoot stands in for "owned by user id 0". It is asked about the opened
	// file and about its directory, each by itself. Nil trusts nothing.
	IsRoot func(fs.FileInfo) bool
	Log    *slog.Logger
	Now    func() time.Time
	// Wait stands in for DoorWait; zero keeps DoorWait.
	Wait time.Duration
}

// ForTest returns a Book on the test's own directory and stand-ins. It
// panics outside a test binary, so no running adapter can reach it.
func ForTest(o TestOptions) *Book {
	if !testing.Testing() {
		panic("owners: ForTest outside a test binary")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Wait <= 0 {
		o.Wait = DoorWait
	}
	return newBook(o.Settings, &rootSide{dir: o.Dir, isRoot: o.IsRoot, log: o.Log, now: o.Now, wait: o.Wait})
}

func newBook(settings []string, r *rootSide) *Book {
	b := &Book{set: map[string]bool{}, r: r}
	for _, owner := range settings {
		if owner = strings.TrimSpace(owner); owner != "" {
			b.settings = append(b.settings, owner)
			b.set[strings.ToLower(owner)] = true
		}
	}
	return b
}

// With returns a Book with other settings and the same root side.
func (b *Book) With(settings []string) *Book { return newBook(settings, b.r) }

// Dir is the directory this Book reads: Dir, except in a test.
func (b *Book) Dir() string { return b.r.dir }

// Wait is this Book's bound on one call to the door: DoorWait, except in a test.
func (b *Book) Wait() time.Duration { return b.r.wait }

// Present reports whether anything has root's directory's name. A name that
// cannot be looked up counts as present: owners then come from root's copy
// alone, and without a trusted copy there is none.
func (b *Book) Present() bool {
	_, err := os.Lstat(b.r.dir)
	return err == nil || !os.IsNotExist(err)
}

// view is the one source of the three answers below: whether root's
// directory is present, and the owners that count.
func (b *Book) view() (root bool, owners map[string]Record) {
	if !b.Present() {
		return false, nil
	}
	if len(b.settings) != 0 {
		b.r.noteIgnored()
	}
	return true, b.r.current(false)
}

// Owner is the one answer to "is this sender an owner": where root's
// directory is present, the sender's Chat user id is in root's trusted copy;
// where it is absent, the sender's address or Chat id is in the settings.
func (b *Book) Owner(id, address string) bool {
	root, owners := b.view()
	if root {
		_, ok := owners[id]
		return id != "" && ok
	}
	return (address != "" && b.set[strings.ToLower(address)]) || (id != "" && b.set[strings.ToLower(id)])
}

// Any reports whether any owner is configured, by the same rule as Owner.
func (b *Book) Any() bool {
	root, owners := b.view()
	if root {
		return len(owners) != 0
	}
	return len(b.set) != 0
}

// Listing is what gc gchat owners shows.
type Listing struct {
	// Root is true where root's directory is present.
	Root bool
	// Owners are root's trusted copy, sorted by id.
	Owners []Entry
	// Settings are the settings' entries where root's directory is absent.
	Settings []string
}

// List returns the owners that count, by the same rule as Owner.
func (b *Book) List() Listing {
	root, owners := b.view()
	if !root {
		return Listing{Settings: slices.Clone(b.settings)}
	}
	out := Listing{Root: true, Owners: []Entry{}}
	for _, id := range slices.Sorted(maps.Keys(owners)) {
		r := owners[id]
		out.Owners = append(out.Owners, Entry{ID: id, Since: r.Since, By: r.By, Claim: r.Claim})
	}
	return out
}

// Refresh reads root's copy again whether or not its file looks changed. The
// adapter calls it after the door answers taken.
func (b *Book) Refresh() {
	if b.Present() {
		b.r.current(true)
	}
}

// NoteSettings writes the one "gchat owners settings ignored" line when root's
// directory is present and the settings name an owner. The app calls it at
// start; the answers above call it too, so a directory that appears later is
// still said once.
func (b *Book) NoteSettings() {
	if len(b.settings) != 0 && b.Present() {
		b.r.noteIgnored()
	}
}

func (r *rootSide) noteIgnored() {
	r.mu.Lock()
	first := !r.ignored
	r.ignored = true
	r.mu.Unlock()
	if first && r.log != nil {
		r.log.Warn("gchat owners settings ignored", "dir", r.dir, "reason", "root's directory is present, so owners come from root's file alone")
	}
}

// current returns the owners of the trusted copy, reading it again when its
// file or directory looks changed, or when forced. Not trusted is no owner.
func (r *rootSide) current(force bool) map[string]Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, data, why := readTrusted(r.dir, copyName, copyLimit, r.isRoot, func(k statKey) bool { return !force && r.have && k == r.key })
	if why == reasonUnchanged {
		return r.owners
	}
	r.have, r.owners = false, nil
	if why == "" {
		var owners map[string]Record
		if owners, why = parseCopy(data); why == "" {
			r.have, r.key, r.owners = true, key, owners
			return owners
		}
	}
	if why != reasonAbsent && r.log != nil && (r.warned.IsZero() || r.now().Sub(r.warned) >= warnEvery) {
		r.warned = r.now()
		r.log.Warn("gchat owners file", "path", filepath.Join(r.dir, copyName), "reason", why)
	}
	return nil
}
