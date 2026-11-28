package inbound

import (
	"context"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/owners"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

// The fixed lines of the turn, from the door's answer to a take. The door
// decides; the adapter only tells the mayor what it answered.
var claimLines = map[string]string{
	"taken":         "[gchat: this sender has just become an owner of this box, by the one-time code from the owner page]",
	"already_owner": "[gchat: this message carried the live owner code and this sender was already an owner of this box; that code is now spent and nothing else changed]",
	"wrong":         "[gchat: this message has the shape of an owner code and is not the live one]",
	"tries_ended":   "[gchat: this message has the shape of an owner code and is not the live one; that code is now ended, a new one comes from the owner page]",
	"not_live":      "[gchat: this message has the shape of an owner code and no owner code is live; a new one comes from the owner page]",
	"full":          "[gchat: this message carried the live owner code and this box already has 50 owners; nobody was added and that code is now spent; one owner must be removed in Chat, and a new code comes from the owner page]",
	"no_answer":     "[gchat: this message has the shape of an owner code and this box could not check it]",
}

// ownerCode answers the 12 characters of a code-shaped message: newly
// created, never an edit (a catch-up snapshot edited after its creation is
// an edit), alone (no attachment), in a direct chat, from a human who is not
// the identity, not a bot and not a deleted user, and only where root's
// directory is present. Every other message is untouched.
func (p *Processor) ownerCode(c puller.Change, m *api.Message, kind, local string, edited bool) (string, bool) {
	identity := p.identity()
	u := m.Sender
	if c.Kind != "message.created" || edited || local != "" || kind != "dm" || len(m.Attachment) != 0 ||
		u == nil || u.Type != "HUMAN" || u.IsAnonymous || identity == "" || u.Name == identity ||
		!owners.ChatUserID.MatchString(u.Name) || !owners.ValidMessage(m.Name) {
		return "", false
	}
	code, ok := owners.CodeShaped(m.Text)
	if !ok || !p.opts.Principals.Book().Present() {
		return "", false
	}
	return code, true
}

// knock hands the code, the sender's Chat id and the message's name to the
// door, once, before the message is framed and handed to the mayor. It
// returns the turn's fixed last line and, for taken and tries_ended, the
// event the turn's own save records.
func (p *Processor) knock(ctx context.Context, code string, m *api.Message) (string, func(*ledger.Ledger)) {
	book := p.opts.Principals.Book()
	answer := book.Take(ctx, code, m.Sender.Name, m.Name)
	result := answer.Result
	if claimLines[result] == "" {
		result = "no_answer"
	}
	if result == "taken" {
		// The door writes its copy before it answers: read it again now.
		book.Refresh()
	}
	at := p.opts.Now().UTC()
	fields := []any{"at", at, "result", result, "sender", m.Sender.Name, "message", m.Name}
	if answer.Claim != "" {
		fields = append(fields, "claim", answer.Claim)
	}
	p.opts.Logger.Info("gchat owner claim", fields...)
	var event func(*ledger.Ledger)
	if result == "taken" || result == "tries_ended" {
		e := ledger.OwnerEvent{At: at.Truncate(time.Second), Event: result, Claim: answer.Claim, Owner: m.Sender.Name, Message: m.Name}
		event = func(l *ledger.Ledger) { l.AddOwnerEvent(e) }
	}
	return claimLines[result], event
}
