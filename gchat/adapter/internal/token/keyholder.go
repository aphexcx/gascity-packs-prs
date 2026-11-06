package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gchat-adapter/internal/config"
	"golang.org/x/oauth2"
)

// A key holder is a separate process that owns a long-lived Google credential
// and mints short-lived access tokens for one Workspace user on request, over a
// Unix socket:
//
//	POST /v1/token {"scopes": [...]}  -> 200 {"access_token": "...", "expires_at": <ms since epoch>}
//	                                  -> 4xx/5xx {"error": "<code>", "detail": "..."}
//	GET  /v1/info                     -> 200 {"subject": "user@example.com", ...}
//
// The adapter never sees the credential and has nothing to sign in with: a
// permanent refusal is repaired outside it (the key holder's own enrollment),
// so a managed Source re-tries on its own instead of waiting for a token file.

// keyholderRetry is how often a managed Source re-tries a refused mint while in
// NeedsSignIn, and how often it re-tries learning its Chat identity.
const keyholderRetry = 30 * time.Second

// Key holder error codes that no retry fixes until something changes outside
// the adapter. Everything else is transient.
var permanentKeyholderCodes = map[string]bool{
	"delegation_not_authorized": true, "subject_not_found": true, "scope_not_allowed": true,
	"key_rejected": true, "invalid_scope": true, "grant_rejected": true, "bad_request": true,
}

var peopleResourceID = regexp.MustCompile(`^people/(\d+)$`)

type keyholder struct {
	socket     string
	scopes     []string
	peopleBase string
	chatBase   string
	client     *http.Client
}

// NewKeyholder returns a managed Source: tokens come from the key holder at
// socket, nothing is read from or watched on disk, and the Chat identity is
// learned from the Workspace directory (People API) with the token itself.
func NewKeyholder(socket string, ep config.Endpoints, scopes []string) (*Source, error) {
	if strings.TrimSpace(socket) == "" {
		return nil, errors.New("keyholder socket path is required")
	}
	if strings.TrimSpace(ep.ChatAPIBase) == "" {
		return nil, errors.New("ChatAPIBase is required for credential validation")
	}
	k := &keyholder{socket: socket, scopes: append([]string(nil), scopes...), peopleBase: strings.TrimRight(ep.PeopleAPIBase, "/"), chatBase: strings.TrimRight(ep.ChatAPIBase, "/")}
	k.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	s := &Source{tokenFile: "", state: Ready, refreshGate: make(chan struct{}, 1),
		watchInterval: keyholderRetry, now: time.Now, refreshDelay: 200 * time.Millisecond,
		mint: k.mint, identify: k.identify, managed: true}
	s.validateFn = func(context.Context, *http.Client) error { return nil }
	return s, nil
}

// SetRetryInterval changes how often a managed Source re-asks a refusing key
// holder (the production value is keyholderRetry). Tests shorten it.
func (s *Source) SetRetryInterval(d time.Duration) {
	if s.managed && d > 0 {
		s.watchInterval = d
	}
}

// Managed reports whether sign-in happens outside the adapter (a key holder).
// In that mode a Chat refusal of a freshly minted token is a setup problem to
// keep retrying, never a reason to wait for gc gchat auth.
func (s *Source) Managed() bool { return s.managed }

type keyholderError struct {
	status int
	code   string
}

func (e keyholderError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("key holder: HTTP %d", e.status)
	}
	return fmt.Sprintf("key holder: %s (HTTP %d)", e.code, e.status)
}

func (k *keyholder) mint(ctx context.Context) (*oauth2.Token, error) {
	body, _ := json.Marshal(map[string]any{"scopes": k.scopes})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://keyholder/v1/token", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("key holder at %s: %w", k.socket, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("key holder at %s: %w", k.socket, err)
	}
	var reply struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   int64  `json:"expires_at"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &reply)
	if res.StatusCode != http.StatusOK {
		kerr := keyholderError{status: res.StatusCode, code: reply.Error}
		if res.StatusCode < 500 && permanentKeyholderCodes[reply.Error] {
			return nil, fmt.Errorf("%w: %w", ErrNeedsSignIn, kerr)
		}
		return nil, kerr
	}
	if reply.AccessToken == "" || reply.ExpiresAt <= 0 {
		return nil, keyholderError{status: res.StatusCode, code: "malformed token reply"}
	}
	return &oauth2.Token{AccessToken: reply.AccessToken, TokenType: "Bearer", Expiry: time.UnixMilli(reply.ExpiresAt)}, nil
}

// identify resolves the user's Chat id (users/<id>). First by searching the
// Workspace directory for the key holder's subject (Chat and People share the
// numeric id; needs directory.readonly and a directory the org shares), then
// by reading the memberships of the user's own direct messages, where Chat
// reports each member's address. Both failing, the identity stays unknown and
// is learned from the first outbound message instead.
func (k *keyholder) identify(ctx context.Context, tok *oauth2.Token) (string, error) {
	subject, err := k.subject(ctx)
	if err != nil {
		return "", err
	}
	id, dirErr := k.identifyFromDirectory(ctx, tok, subject)
	if dirErr == nil {
		return id, nil
	}
	id, chatErr := k.identifyFromMemberships(ctx, tok, subject)
	if chatErr == nil {
		return id, nil
	}
	return "", fmt.Errorf("%w; %w", dirErr, chatErr)
}

func (k *keyholder) subject(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://keyholder/v1/info", nil)
	if err != nil {
		return "", err
	}
	res, err := k.client.Do(req)
	if err != nil {
		return "", err
	}
	var info struct {
		Subject string `json:"subject"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&info)
	res.Body.Close()
	if err != nil || info.Subject == "" {
		return "", errors.New("key holder reported no subject")
	}
	return info.Subject, nil
}

// getJSON performs an authenticated GET and decodes a 200 answer into out.
func getJSON(ctx context.Context, tok *oauth2.Token, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	tok.SetAuthHeader(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

func (k *keyholder) identifyFromDirectory(ctx context.Context, tok *oauth2.Token, subject string) (string, error) {
	if k.peopleBase == "" {
		return "", errors.New("PeopleAPIBase is not configured")
	}
	query := url.Values{"query": {subject}, "readMask": {"emailAddresses"}, "sources": {"DIRECTORY_SOURCE_TYPE_DOMAIN_PROFILE"}}
	var page struct {
		People []struct {
			ResourceName   string `json:"resourceName"`
			EmailAddresses []struct {
				Value string `json:"value"`
			} `json:"emailAddresses"`
		} `json:"people"`
	}
	if err := getJSON(ctx, tok, k.peopleBase+"/v1/people:searchDirectoryPeople?"+query.Encode(), &page); err != nil {
		return "", fmt.Errorf("directory search: %w", err)
	}
	for _, p := range page.People {
		for _, e := range p.EmailAddresses {
			if strings.EqualFold(e.Value, subject) {
				if m := peopleResourceID.FindStringSubmatch(p.ResourceName); m != nil {
					return "users/" + m[1], nil
				}
			}
		}
	}
	return "", errors.New("directory search: subject not found")
}

// identifyFromMemberships reads the user's direct messages: Chat lists their
// members with addresses, and the member whose address is the subject is the
// user itself. Google creates a DM for a new user's first app notification, so
// this usually works before anyone has written to the user.
func (k *keyholder) identifyFromMemberships(ctx context.Context, tok *oauth2.Token, subject string) (string, error) {
	if k.chatBase == "" {
		return "", errors.New("ChatAPIBase is not configured")
	}
	var spaces struct {
		Spaces []struct {
			Name      string `json:"name"`
			SpaceType string `json:"spaceType"`
		} `json:"spaces"`
	}
	if err := getJSON(ctx, tok, k.chatBase+"/v1/spaces?pageSize=100", &spaces); err != nil {
		return "", fmt.Errorf("chat spaces: %w", err)
	}
	for _, space := range spaces.Spaces {
		if space.SpaceType != "DIRECT_MESSAGE" {
			continue
		}
		var members struct {
			Memberships []struct {
				Member struct {
					Name  string `json:"name"`
					Email string `json:"email"`
					Type  string `json:"type"`
				} `json:"member"`
			} `json:"memberships"`
		}
		if err := getJSON(ctx, tok, k.chatBase+"/v1/"+space.Name+"/members?pageSize=100", &members); err != nil {
			continue
		}
		for _, m := range members.Memberships {
			if m.Member.Type == "HUMAN" && strings.EqualFold(m.Member.Email, subject) && validUserID(m.Member.Name) {
				return m.Member.Name, nil
			}
		}
	}
	return "", errors.New("chat memberships: subject not found in any direct message")
}

// watchKeyholder is the managed counterpart of the token-file watch: while the
// key holder refuses to mint, it asks again every interval, and the first
// success is a credential replacement (clients rebuild, work resumes).
func (s *Source) watchKeyholder(ctx context.Context, onReload func()) {
	ticker := time.NewTicker(s.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if s.State() != NeedsSignIn {
			continue
		}
		mintCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		tok, err := s.mint(mintCtx)
		cancel()
		if err != nil || ctx.Err() != nil {
			continue
		}
		s.mu.Lock()
		s.current, s.state = tok, Ready
		s.generation++
		s.mu.Unlock()
		s.learnIdentity(ctx, tok)
		if onReload != nil {
			onReload()
		}
	}
}

// learnIdentity fills the identity from the directory once; failures are
// retried on later mints and never fail the token itself.
func (s *Source) learnIdentity(ctx context.Context, tok *oauth2.Token) {
	if s.identify == nil {
		return
	}
	s.mu.Lock()
	known := s.identityUserID != ""
	s.mu.Unlock()
	if known {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, err := s.identify(lookupCtx, tok)
	if err != nil || !validUserID(id) {
		return
	}
	s.mu.Lock()
	s.identityUserID = id
	s.mu.Unlock()
}
