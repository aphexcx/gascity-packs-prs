// Package owners answers who is an owner of this box, and calls the box's
// root-side door. Declarations only: the tests come first.
package owners

import (
	"context"
	"io/fs"
	"log/slog"
	"regexp"
	"time"
)

const Dir = "/run/gchat-owner-claim"

const (
	MaxOwners = 50
	DoorWait  = 2 * time.Second
)

var ChatUserID = regexp.MustCompile(`^users/[0-9]{1,32}$`)

func ValidMessage(name string) bool { return false }

type Record struct {
	Since string `json:"since"`
	By    string `json:"by"`
	Claim string `json:"claim"`
}

type Entry struct {
	ID    string `json:"id"`
	Since string `json:"since"`
	By    string `json:"by"`
	Claim string `json:"claim"`
}

type Book struct{}

func New(settings []string, log *slog.Logger) *Book { return &Book{} }

type TestOptions struct {
	Settings []string
	Dir      string
	IsRoot   func(fs.FileInfo) bool
	Log      *slog.Logger
	Now      func() time.Time
	Wait     time.Duration
}

func ForTest(o TestOptions) *Book { return &Book{} }

func (b *Book) With(settings []string) *Book  { return b }
func (b *Book) Dir() string                   { return "" }
func (b *Book) Wait() time.Duration           { return 0 }
func (b *Book) Present() bool                 { return false }
func (b *Book) Owner(id, address string) bool { return false }
func (b *Book) Any() bool                     { return false }

type Listing struct {
	Root     bool
	Owners   []Entry
	Settings []string
}

func (b *Book) List() Listing { return Listing{} }
func (b *Book) Refresh()      {}
func (b *Book) NoteSettings() {}

type Answer struct {
	Result string
	Claim  string
}

func (b *Book) Take(ctx context.Context, code, sender, message string) Answer { return Answer{} }
func (b *Book) Remove(ctx context.Context, owner, message string) Answer      { return Answer{} }
func (b *Book) Here(ctx context.Context) bool                                 { return false }

func CodeShaped(text string) (string, bool) { return "", false }
