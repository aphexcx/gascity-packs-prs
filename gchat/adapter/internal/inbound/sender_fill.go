package inbound

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"time"

	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/token"
	api "google.golang.org/api/chat/v1"
)

type senderSource interface {
	GetMessage(context.Context, string) (*api.Message, error)
	GetMembership(context.Context, string, string) (*api.Membership, error)
}

const senderFillTimeout = 3 * time.Second

var fillUserID = regexp.MustCompile(`^users/[0-9]+$`)

type senderEntry struct {
	displayName string
	domainID    string // empty for a failed lookup
	expires     time.Time
}

// SenderFill resolves incomplete senders for one processor generation.
// Its processor's delivery gate serializes access, including during catch-up.
type SenderFill struct {
	source                       senderSource
	log                          *principal.WarningLog
	now                          func() time.Time
	bound                        time.Duration
	identityUser, identityDomain string
	entries                      map[string]senderEntry
	warned                       map[[2]string]bool
}

func NewSenderFill(source senderSource, log *principal.WarningLog) *SenderFill {
	return &SenderFill{source: source, log: log, now: time.Now, bound: senderFillTimeout,
		entries: make(map[string]senderEntry), warned: make(map[[2]string]bool)}
}

// Fill only local copies. The original puller event may also be used by the
// arrival observer. An unused answer leaves the resource pointers untouched.
func (p *Processor) fillSender(ctx context.Context, c puller.Change, identityDomain string) (puller.Change, context.Context) {
	f := p.opts.SenderFill
	if f == nil || ctx.Err() != nil {
		return c, ctx
	}
	identity := p.identity()
	if identity != f.identityUser || identityDomain != f.identityDomain {
		clear(f.entries)
		f.identityUser, f.identityDomain = identity, identityDomain
	}
	u := historyUser(c)
	if u == nil || u.IsAnonymous || u.Type == "BOT" || !fillUserID.MatchString(u.Name) ||
		(u.DisplayName != "" && u.DomainId != "") || (u.Name == identity && identityDomain != "") {
		return c, ctx
	}
	entry, ok := f.entries[u.Name]
	if !ok || !f.now().Before(entry.expires) {
		ctx = p.opts.Principals.BeforeFill(ctx)
		entry = f.lookup(ctx, c, u.Name)
	}
	if entry.domainID == "" {
		return c, ctx
	}
	user := *u
	if user.DomainId == "" {
		user.DomainId = entry.domainID
	}
	if user.DisplayName == "" {
		user.DisplayName = entry.displayName
	}
	if c.Message != nil {
		m := *c.Message
		m.Sender = &user
		c.Message = &m
	} else {
		r := *c.Reaction
		r.User = &user
		c.Reaction = &r
	}
	return c, ctx
}

func (f *SenderFill) lookup(parent context.Context, c puller.Change, id string) senderEntry {
	ctx, cancel := context.WithTimeout(parent, f.bound)
	defer cancel()
	var user *api.User
	var err error
	var source string
	switch c.Kind {
	case "message.created", "message.updated":
		source = "message"
		var m *api.Message
		m, err = f.source.GetMessage(ctx, c.Message.Name)
		if m != nil {
			user = m.Sender
		}
	case "reaction.created", "reaction.deleted":
		source = "member"
		var m *api.Membership
		m, err = f.source.GetMembership(ctx, c.Space, id)
		if m != nil {
			user = m.Member
		}
	default:
		// Deletions of messages can use a cached identity only.
		return senderEntry{}
	}
	if parent.Err() != nil {
		return senderEntry{}
	}
	cause := senderAnswerCause(ctx, err, id, user)
	entry := senderEntry{expires: f.now().Add(10 * time.Minute)}
	if cause == "" {
		entry = senderEntry{displayName: user.DisplayName, domainID: user.DomainId, expires: f.now().Add(time.Hour)}
		if user.DisplayName == "" {
			cause = "no_display_name"
		}
	}
	f.entries[id] = entry
	key := [2]string{source, cause}
	if cause != "" && f.log != nil && !f.warned[key] {
		f.warned[key] = true
		f.log.Report(source, cause)
	}
	return entry
}

func senderAnswerCause(ctx context.Context, err error, id string, user *api.User) string {
	if errors.Is(err, token.ErrLookupCredential) {
		return "credential"
	}
	if status := principal.LookupHTTPStatus(err); status != 0 {
		return "http_" + strconv.Itoa(status)
	}
	switch {
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case err != nil:
		return "error"
	case user == nil:
		return "no_user"
	case user.Name != id:
		return "other_user"
	case user.IsAnonymous || user.Type == "BOT":
		return "not_a_person"
	case user.DomainId == "":
		return "no_domain_id"
	default:
		return ""
	}
}
