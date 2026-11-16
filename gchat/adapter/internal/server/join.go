package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/token"
	"golang.org/x/oauth2"
)

var joinSpaceName = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+$`)
var joinMessageName = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+/messages/[A-Za-z0-9_.-]+$`)

type joinInput struct {
	Space   string `json:"space"`
	Request bool   `json:"request"`
	Yes     string `json:"yes"`
	Check   bool   `json:"check"`
}

type joinResult struct {
	Result      string `json:"result"`
	Message     string `json:"message"`
	Code        string `json:"code,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Recorded    bool   `json:"recorded"`
	RecordError string `json:"record_error,omitempty"`
}

// decodeJoin rejects duplicate fields as well as unknown fields: a second room
// cannot silently overwrite the first even if a caller bypasses the shell.
func decodeJoin(w http.ResponseWriter, r *http.Request) (joinInput, error) {
	var input joinInput
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return input, errors.New("Supply one join request object.")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return input, errors.New("Supply one join request object.")
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return input, errors.New("Supply each join field once; only one room is allowed.")
		}
		seen[name] = true
		var target any
		switch name {
		case "space":
			target = &input.Space
		case "request":
			target = &input.Request
		case "yes":
			target = &input.Yes
		case "check":
			target = &input.Check
		default:
			return input, errors.New("Unknown join field; only space, request, yes and check are allowed.")
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || string(raw) == "null" || json.Unmarshal(raw, target) != nil {
			return input, errors.New("Supply one room id and one join action with the expected value types.")
		}
	}
	if _, err = d.Token(); err != nil {
		return input, errors.New("Supply one join request object.")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return input, errors.New("Supply one join request object.")
	}
	if input.Check {
		if len(seen) != 1 {
			return input, errors.New("--check takes no room and no approval action.")
		}
		return input, nil
	}
	if !validSpace(input.Space) || !joinSpaceName.MatchString(input.Space) {
		return input, errors.New("Supply one exact room id as --space spaces/ID; lists and wildcards are not allowed.")
	}
	if input.Request && seen["yes"] {
		return input, errors.New("Choose one action: --request or --yes MESSAGE.")
	}
	return input, nil
}

func (s *Server) joinHTTP(w http.ResponseWriter, r *http.Request) {
	input, err := decodeJoin(w, r)
	status := http.StatusOK
	var result joinResult
	if err != nil {
		status = http.StatusBadRequest
		result = joinResult{Result: "invalid_request", Message: err.Error()}
	} else {
		result = s.runJoin(r.Context(), input)
	}
	// Even a refused attempt gets a minimal audit. Malformed values never enter
	// it: neither arbitrary request text nor a code passed as a bad argument.
	space, owner := input.Space, input.Yes
	if !joinSpaceName.MatchString(space) {
		space = ""
	}
	if !joinMessageName.MatchString(owner) {
		owner = ""
	}
	at := time.Now().UTC()
	audit := ledger.JoinAttempt{At: at, Space: space, OwnerMessage: owner, Result: result.Result}
	err = s.settle(r.Context(), func(l *ledger.Ledger) error { l.JoinAttempts = append(l.JoinAttempts, audit); return nil })
	result.Recorded = err == nil
	if err != nil {
		result.RecordError = "The local attempt record could not be saved."
	}
	s.opts.Logger.Info("gchat join", "operation", "join", "at", at, "space", space, "owner_message", owner, "result", result.Result)
	respond(w, status, result)
}

func noJoinYes(reason string) joinResult { return joinResult{Result: "no_owner_yes", Message: reason} }

func joinPermission(ctx context.Context, cfg config.Config) (*oauth2.Token, joinResult) {
	tok, err := token.MintJoin(ctx, cfg.Secrets.KeyholderSocket)
	if err == nil {
		return tok, joinResult{}
	}
	if code := token.JoinRefusal(err); code != "" {
		message := joinAdminStep
		if code == "scope_not_allowed" {
			message = joinSoftwareStep
		}
		return nil, joinResult{Result: "permission_missing", Message: message}
	}
	return nil, joinResult{Result: "google_no_answer", Message: "The key holder did not answer with a usable permission. Nothing changed; run it again."}
}

func (s *Server) runJoin(ctx context.Context, input joinInput) joinResult {
	cfg := s.configuration()
	if cfg.Secrets.TokenSource != config.TokenSourceKeyholder {
		return joinReply("not_available")
	}
	if input.Check {
		_, failure := joinPermission(ctx, cfg) // Deliberately discard this token.
		if failure.Result != "" {
			return failure
		}
		return joinReply("permission_present")
	}
	unlock, err := s.lock(ctx, "join:"+input.Space)
	if err != nil {
		return joinReply("google_no_answer")
	}
	defer unlock()
	if s.available() != nil {
		return joinResult{Result: "google_no_answer", Message: "The adapter is not ready to check this invitation. Nothing changed; run it again."}
	}
	c, _ := s.clients()
	identity, err := c.JoinIdentity()
	if err != nil {
		return noJoinYes("The identity is not known safely yet. Nothing changed; try again after the adapter learns its identity.")
	}
	membership, err := c.ReadOwnMembership(ctx, input.Space, identity)
	state := ""
	if membership != nil {
		state = membership.State
	}
	answer := joinAnswer("read", state, err)
	if answer.Result != "" {
		return answer
	}
	// Only an INVITED read reaches here. Already-member never consults an
	// owner's message or requests a write token.
	if len(cfg.Secrets.Owners) == 0 {
		return noJoinYes("No owner is configured for this box.")
	}
	if input.Request {
		return s.requestJoin(ctx, cfg, input.Space, identity)
	}
	if input.Yes == "" {
		return noJoinYes("Supply the owner's direct message with --yes MESSAGE.")
	}
	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return noJoinYes("The approval record could not be read. Nothing changed.")
	}
	pending := snapshot.JoinRequests[input.Space]
	if pending == nil {
		return noJoinYes("Request approval for this room first with --request.")
	}
	if pending.Used {
		return noJoinYes("This request was already used.")
	}
	if !time.Now().Before(pending.ExpiresAt) {
		return noJoinYes("This request has expired.")
	}
	if pending.Identity != identity {
		return noJoinYes("This request belongs to a different identity. Request approval again.")
	}
	if !joinMessageName.MatchString(input.Yes) {
		return noJoinYes("Supply the owner's message name as spaces/ID/messages/ID.")
	}
	if reason := checkJoinYes(ctx, c, cfg, input.Yes, identity, pending); reason != "" {
		return noJoinYes(reason)
	}
	tok, failure := joinPermission(ctx, cfg)
	if failure.Result != "" {
		return failure
	}
	// Commit consumption before the only write. A crash or an ambiguous response
	// can never revive this approval; a retry first reads the actual membership.
	if err = s.update(ctx, func(l *ledger.Ledger) error {
		p := l.JoinRequests[input.Space]
		if p == nil || p.Used || p.CodeHash != pending.CodeHash || !time.Now().Before(p.ExpiresAt) {
			return errors.New("approval changed or expired")
		}
		p.Used = true
		return nil
	}); err != nil {
		return noJoinYes("The approval could not be saved as used, or it expired. Nothing changed; request approval again.")
	}
	membership, err = c.JoinSelf(ctx, input.Space, identity, tok)
	state = ""
	if membership != nil {
		state = membership.State
	}
	answer = joinAnswer("create", state, err)
	if answer.Result == "joined" {
		name := input.Space
		if space, e := c.GetSpace(ctx, input.Space); e == nil && space != nil && space.DisplayName != "" {
			name = space.DisplayName
		}
		answer.Message = fmt.Sprintf("Joined %s. People in this room can now write to this identity.", name)
	}
	return answer
}

func (s *Server) requestJoin(ctx context.Context, cfg config.Config, space, identity string) joinResult {
	_, failure := joinPermission(ctx, cfg) // Check only; the write mints a new token.
	if failure.Result != "" {
		return failure
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return noJoinYes("An approval code could not be made. Nothing changed.")
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	code := string(raw)
	now := time.Now().UTC()
	pending := &ledger.JoinRequest{CodeHash: joinCodeHash(code), Identity: identity, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if err := s.update(ctx, func(l *ledger.Ledger) error {
		if l.JoinRequests == nil {
			l.JoinRequests = map[string]*ledger.JoinRequest{}
		}
		l.JoinRequests[space] = pending
		return nil
	}); err != nil {
		return noJoinYes("The approval request could not be saved. Nothing changed.")
	}
	result := joinReply("requested")
	result.Code = code
	result.ExpiresAt = pending.ExpiresAt.Format(time.RFC3339Nano)
	return result
}

func joinCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func checkJoinYes(ctx context.Context, c *chat.Client, cfg config.Config, name, identity string, pending *ledger.JoinRequest) string {
	m, err := c.GetMessage(ctx, name)
	if err != nil || m == nil || m.Name != name {
		return "The owner's message could not be read. Nothing changed."
	}
	if m.Sender != nil && m.Sender.Name == identity {
		return "The identity cannot approve its own join."
	}
	if !c.JoinOwner(ctx, cfg.Secrets.Owners, m.Sender) {
		return "The message sender is not an owner of this box."
	}
	parts := strings.Split(name, "/")
	space, err := c.GetSpace(ctx, strings.Join(parts[:2], "/"))
	if err != nil || space == nil || space.SpaceType != "DIRECT_MESSAGE" {
		return "The owner's yes must be in a direct message."
	}
	posted, err := time.Parse(time.RFC3339Nano, m.CreateTime)
	if err != nil {
		return "The owner's message has no valid creation time."
	}
	if !posted.After(pending.CreatedAt) {
		return "The message was written before this request."
	}
	if !posted.Before(pending.ExpiresAt) {
		return "The message was written after this request expired."
	}
	for _, word := range strings.FieldsFunc(m.Text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if subtle.ConstantTimeCompare([]byte(joinCodeHash(word)), []byte(pending.CodeHash)) == 1 {
			return ""
		}
	}
	return "The message does not carry this room's code."
}
