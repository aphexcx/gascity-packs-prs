package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/owners"
)

// ownersInput is gc gchat owners: no field lists, add asks to add, remove
// names the one owner to remove with request or yes.
type ownersInput struct {
	Add     bool   `json:"add"`
	Remove  string `json:"remove"`
	Request bool   `json:"request"`
	Yes     string `json:"yes"`
}

type ownersResult struct {
	Result    string         `json:"result"`
	Message   string         `json:"message"`
	Owners    []owners.Entry `json:"owners,omitempty"`
	Settings  []string       `json:"settings,omitempty"`
	Code      string         `json:"code,omitempty"`
	ExpiresAt string         `json:"expires_at,omitempty"`
	Recorded  *bool          `json:"recorded,omitempty"`
}

// doorUnavailable is how the owner is told that the box did not confirm a
// removal; it does not say that the removal failed.
const doorUnavailable = "[gchat owners: the box did NOT confirm this removal. The next answer of gc gchat owners shows who is an owner now. If the person is listed there and is still to be removed, make a new request and give a new yes]"

const (
	addByPage     = "On a box, a new owner comes by a one-time code from the owner page, sent alone to the mayor in a direct chat, and by nothing else. Nothing changed."
	addBySettings = "Root's directory is absent, so this is not a box: the owners are the ones the settings name (GCHAT_OWNERS), no command adds one, and no owner code is taken here. Nothing changed."
)

// decodeOwners refuses unknown and repeated fields, as the join does.
func decodeOwners(w http.ResponseWriter, r *http.Request) (ownersInput, map[string]bool, error) {
	var input ownersInput
	seen := map[string]bool{}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return input, seen, errors.New("Supply one owners request object.")
	}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return input, seen, errors.New("Supply each owners field once.")
		}
		seen[name] = true
		var target any
		switch name {
		case "add":
			target = &input.Add
		case "remove":
			target = &input.Remove
		case "request":
			target = &input.Request
		case "yes":
			target = &input.Yes
		default:
			return input, seen, errors.New("Unknown owners field; only add, remove, request and yes are allowed.")
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || string(raw) == "null" || json.Unmarshal(raw, target) != nil {
			return input, seen, errors.New("Supply the owners fields with the expected value types.")
		}
	}
	if _, err := d.Token(); err != nil {
		return input, seen, errors.New("Supply one owners request object.")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return input, seen, errors.New("Supply one owners request object.")
	}
	if seen["add"] {
		return input, seen, nil
	}
	switch {
	case !seen["remove"] && (seen["request"] || seen["yes"]):
		return input, seen, errors.New("--request and --yes name a removal: add --remove users/ID.")
	case seen["remove"] && !owners.ChatUserID.MatchString(input.Remove):
		return input, seen, errors.New("Supply one Chat user id as --remove users/ID.")
	case seen["remove"] && input.Request == seen["yes"]:
		return input, seen, errors.New("Choose one action: --request or --yes MESSAGE.")
	case seen["yes"] && !owners.ValidMessage(input.Yes):
		return input, seen, errors.New("Supply the owner's message name as spaces/ID/messages/ID.")
	}
	return input, seen, nil
}

func (s *Server) ownersHTTP(w http.ResponseWriter, r *http.Request) {
	ready := s.Health().State != "initializing" && s.Health().State != "recovery-required"
	if s.opts.LedgerReady != nil {
		ready = s.opts.LedgerReady()
	}
	if !ready {
		s.opts.Logger.Info("gchat owners", "action", "", "target", "", "owner_message", "", "result", "state_unavailable")
		respond(w, http.StatusServiceUnavailable, ownersResult{Result: "state_unavailable", Message: "The adapter is loading its state. Nothing changed; run it again."})
		return
	}
	input, seen, err := decodeOwners(w, r)
	status := http.StatusOK
	var result ownersResult
	action := "list"
	switch {
	case err != nil:
		status, result = http.StatusBadRequest, ownersResult{Result: "invalid_request", Message: err.Error()}
		action = "invalid"
	case seen["add"]:
		action, result = "add", ownersResult{Result: "add_by_page", Message: addByPage}
		if !s.ownerBook(s.configuration()).Present() {
			result.Message = addBySettings
		}
	case seen["remove"] && input.Request:
		action, result = "request", s.ownersRemove(r.Context(), input)
	case seen["remove"]:
		action, result = "yes", s.ownersRemove(r.Context(), input)
	default:
		result = s.ownersList()
	}
	// The log carries Chat ids and names of messages, never a code or a text.
	target, message := input.Remove, input.Yes
	if !owners.ChatUserID.MatchString(target) {
		target = ""
	}
	if !owners.ValidMessage(message) {
		message = ""
	}
	s.opts.Logger.Info("gchat owners", "action", action, "target", target, "owner_message", message, "result", result.Result)
	respond(w, status, result)
}

func (s *Server) ownersList() ownersResult {
	list := s.ownerBook(s.configuration()).List()
	switch {
	case !list.Root:
		return ownersResult{Result: "settings_owner", Settings: list.Settings, Message: "Root's directory is absent: these owners come from the settings (GCHAT_OWNERS), and no command adds or removes one. No owner code is taken here."}
	case len(list.Owners) == 0:
		return ownersResult{Result: "no_owner", Message: "Root's owners file names no owner, or is not there or not trusted: nobody is an owner of this box now. The first owner comes from the owner page."}
	}
	return ownersResult{Result: "owners", Owners: list.Owners, Message: "These owners come from root's owners file."}
}

func noOwnerYes(reason string) ownersResult {
	return ownersResult{Result: "no_owner_yes", Message: reason}
}

// ownersRemove is a removal's request or its yes. The yes is the join's
// check, rule for rule; on a good yes the request is used in one save and the
// door's remove is sent once, never again.
func (s *Server) ownersRemove(ctx context.Context, input ownersInput) ownersResult {
	cfg := s.configuration()
	book := s.ownerBook(cfg)
	if !book.Present() {
		return ownersResult{Result: "settings_owner", Message: "Root's directory is absent: the owners are the settings' (GCHAT_OWNERS), and no command adds or removes one. Nothing changed."}
	}
	unlock, err := s.lock(ctx, "owners:"+input.Remove)
	if err != nil {
		return noOwnerYes("The adapter is not ready to check an owner's yes. Nothing changed; run it again.")
	}
	defer unlock()
	if s.available() != nil {
		return noOwnerYes("The adapter is not ready to check an owner's yes. Nothing changed; run it again.")
	}
	c, _ := s.clients()
	const unknownIdentity = "The identity is not known safely yet. Nothing changed; try again after the adapter learns its identity."
	identity, err := c.JoinIdentity()
	if err != nil {
		return noOwnerYes(unknownIdentity)
	}
	if input.Remove == identity {
		return ownersResult{Result: "invalid_request", Message: "The mayor's own account is never an owner to remove. Nothing changed."}
	}
	if !book.Any() {
		return ownersResult{Result: "no_owner", Message: "Nobody is an owner of this box yet: the first owner comes from the owner page. Nothing changed."}
	}
	if input.Request {
		return s.requestRemoval(ctx, input.Remove, identity)
	}
	stillMine := func() bool { id, err := c.JoinIdentity(); return err == nil && id == identity }
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return noOwnerYes("The approval record could not be read. Nothing changed.")
	}
	pending := snapshot.OwnerRequests[input.Remove]
	switch {
	case pending == nil:
		return noOwnerYes("Request approval for this removal first with --request.")
	case pending.Used:
		return noOwnerYes("This request was already used.")
	case !time.Now().Before(pending.ExpiresAt):
		return noOwnerYes("This request has expired.")
	case pending.Identity != identity:
		return noOwnerYes("This request belongs to a different identity. Request approval again.")
	}
	reason, approverStill := checkOwnerYes(ctx, c, book, input.Yes, identity, pending.CreatedAt, pending.ExpiresAt, pending.CodeHash, "removal", "request's")
	if reason != "" {
		return noOwnerYes(reason)
	}
	if err = s.update(ctx, func(l *ledger.Ledger) error {
		p := l.OwnerRequests[input.Remove]
		if p == nil || p.Used || p.CodeHash != pending.CodeHash || !time.Now().Before(p.ExpiresAt) {
			return errors.New("approval changed or expired")
		}
		if !stillMine() {
			return errJoinIdentityChanged
		}
		p.Used = true
		return nil
	}); errors.Is(err, errJoinIdentityChanged) {
		return noOwnerYes(unknownIdentity)
	} else if err != nil {
		return noOwnerYes("The approval could not be saved as used, or it expired. Nothing changed; request approval again.")
	}
	// As the join does before its one write: the identity again, after the
	// yes is spent and before the one remove.
	if !stillMine() {
		return noOwnerYes("The identity is not known safely yet. Nothing was sent to the box, but this owner's yes is now used; request approval again after the adapter learns its identity.")
	}
	if !approverStill() {
		return noOwnerYes("The owner who said yes is no longer an owner of this box. Nothing was sent to the box, but this yes is now used.")
	}
	answer := book.Remove(ctx, input.Remove, input.Yes)
	switch answer.Result {
	case "removed":
		recorded := s.settle(ctx, func(l *ledger.Ledger) error {
			l.AddOwnerEvent(ledger.OwnerEvent{At: time.Now().UTC(), Event: "removed", Owner: input.Remove, Message: input.Yes})
			return nil
		}) == nil
		return ownersResult{Result: "removed", Recorded: &recorded, Message: fmt.Sprintf("Removed %s as an owner of this box. Tell the owner who asked that it is done.", input.Remove)}
	case "not_owner":
		return ownersResult{Result: "not_owner", Message: fmt.Sprintf("%s is not an owner of this box; nothing changed. This yes is now used.", input.Remove)}
	}
	return ownersResult{Result: "door_unavailable", Message: doorUnavailable}
}

func (s *Server) requestRemoval(ctx context.Context, target, identity string) ownersResult {
	code, err := approvalCode()
	if err != nil {
		return noOwnerYes("An approval code could not be made. Nothing changed.")
	}
	now := time.Now().UTC()
	pending := &ledger.OwnerRequest{CodeHash: joinCodeHash(code), Identity: identity, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := s.update(ctx, func(l *ledger.Ledger) error {
		if l.OwnerRequests == nil {
			l.OwnerRequests = map[string]*ledger.OwnerRequest{}
		}
		l.OwnerRequests[target] = pending
		return nil
	}); err != nil {
		return noOwnerYes("The approval request could not be saved. Nothing changed.")
	}
	return ownersResult{Result: "requested", Code: code, ExpiresAt: pending.ExpiresAt.Format(time.RFC3339Nano),
		Message: fmt.Sprintf("Ask an owner in that owner's own direct chat, naming the person (%s) and the change: removing them as an owner of this box. To say yes, the owner replies with this code alone, nothing else, before it expires in 24 hours.", target)}
}
