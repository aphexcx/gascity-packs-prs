// Package token owns the desktop OAuth credential and its sign-in lifecycle.
package token

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gchat-adapter/internal/config"
	"golang.org/x/oauth2"
)

type State string

const (
	Ready       State = "ready"
	NeedsSignIn State = "needs-sign-in"
	Missing     State = "missing"
)

var ErrNeedsSignIn = errors.New("OAuth credential needs sign-in")

var errMalformedJSON = errors.New("malformed JSON")
var errInvalidCredential = errors.New("invalid credential file")

// Source shares refreshed tokens across HTTP and Pub/Sub clients. The mutex
// protecting state is never held during network calls, so health stays readable.
type Source struct {
	config         *oauth2.Config
	tokenFile      string
	mu             sync.Mutex
	refreshGate    chan struct{}
	current        *oauth2.Token
	identityUserID string
	state          State
	generation     uint64
	observed       os.FileInfo // owned by the single Watch loop after New
	watchInterval  time.Duration
	retryAt        time.Time
	retryDelay     time.Duration
	now            func() time.Time
	refreshDelay   time.Duration
	validateFn     func(context.Context, *http.Client) error
}

// New loads the credential files without retaining a lifecycle context. Watch's
// context controls its polling and validation; Client uses each HTTP request's
// context for token acquisition and refresh, independently of the watcher.
func New(clientFile, tokenFile string, ep config.Endpoints, scopes []string) (*Source, error) {
	if strings.TrimSpace(ep.ChatAPIBase) == "" {
		return nil, errors.New("ChatAPIBase is required for credential validation")
	}
	cfg, err := loadConfig(clientFile, ep, scopes)
	if err != nil {
		return nil, err
	}
	s := &Source{config: cfg, tokenFile: tokenFile, state: Missing, refreshGate: make(chan struct{}, 1),
		watchInterval: 2 * time.Second, now: time.Now, refreshDelay: 200 * time.Millisecond}
	s.validateFn = func(ctx context.Context, client *http.Client) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(ep.ChatAPIBase, "/")+"/v1/spaces?pageSize=1", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return validationError(response.StatusCode)
		}
		return nil
	}
	tok, info, err := readToken(tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.current, s.observed, s.state = tok, info, Ready
	s.identityUserID, _ = tok.Extra("identity_user_id").(string)
	return s, nil
}

func (s *Source) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// IdentityUserID is the canonical Chat user learned during consent. An empty
// value denotes a legacy credential; refreshing an access token preserves it.
func (s *Source) IdentityUserID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identityUserID
}

// Invalidate expires the cached access token so the next call refreshes it.
// A permanent refresh failure then moves State to NeedsSignIn.
func (s *Source) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
}

// RefreshNow expires the access token and exercises the refresh credential.
// Transient failure leaves the source Ready for retry; only a permanent OAuth
// failure moves it to NeedsSignIn. A concurrent validated replacement wins.
func (s *Source) RefreshNow(ctx context.Context) (State, error) {
	s.Invalidate()
	_, err := s.token(ctx)
	return s.State(), err
}

func (s *Source) invalidateSent(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.AccessToken == accessToken {
		s.expireLocked()
	}
}

// expireLocked replaces the token rather than mutating one a refresh may use.
func (s *Source) expireLocked() {
	if s.current != nil {
		expired := *s.current
		expired.Expiry = time.Now().Add(-time.Hour)
		s.current = &expired
	}
}

// Client checks the credential immediately using ctx, but does not retain its
// cancellation or deadline. Later token acquisition (including waiting for a
// refresh) uses each request's context. Its transport continues to report
// permanent refresh failures through Source.State on later requests as well.
// The client has no total or response-header timeout: callers must set a context
// deadline on every request to bound the API call and response body transfer.
func (s *Source) Client(ctx context.Context) (*http.Client, error) {
	if _, err := s.token(ctx); err != nil {
		return nil, err
	}
	// Do not wrap in ReuseTokenSource: Source already caches, and a second
	// cache would retain the old access token after a validated file replacement.
	return authenticatedClient(ctx, s.token, s.invalidateSent), nil
}

// TokenSource adapts Source to oauth2's context-free interface. The supplied ctx
// bounds every Token call on this adapter, including its wait for a refresh.
// HTTP callers should use Client to acquire tokens with each request's context.
func (s *Source) TokenSource(ctx context.Context) oauth2.TokenSource {
	return &sourceToken{source: s, ctx: ctx}
}

type sourceToken struct {
	source *Source
	ctx    context.Context
}

func (ts *sourceToken) Token() (*oauth2.Token, error) {
	return ts.source.token(ts.ctx)
}

func (s *Source) token(ctx context.Context) (*oauth2.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.state != NeedsSignIn && s.current.Valid() {
		copy := *s.current
		s.mu.Unlock()
		return &copy, nil
	}
	s.mu.Unlock()
	select {
	case s.refreshGate <- struct{}{}:
		defer func() { <-s.refreshGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		current, generation, state := s.current, s.generation, s.state
		// Another refresh or watch reload may have populated the cache while
		// we waited. Return a copy without reinstalling it after unlocking,
		// which could otherwise undo a concurrent invalidation.
		if state != NeedsSignIn && current.Valid() {
			copy := *current
			s.mu.Unlock()
			return &copy, nil
		}
		s.mu.Unlock()
		if current == nil || state == NeedsSignIn {
			return nil, ErrNeedsSignIn
		}
		tok, err := s.refresh(ctx, current)
		s.mu.Lock()
		if generation != s.generation {
			// A watch reload won the race. Neither a token nor an error from the
			// previous credential may replace its result.
			s.mu.Unlock()
			continue
		}
		if errors.Is(err, ErrNeedsSignIn) {
			s.state = NeedsSignIn
		}
		if err == nil {
			s.current, s.state = tok, Ready
		}
		s.mu.Unlock()
		if err != nil {
			// Public callers may put this error in a status or health response.
			// Keep actionable sentinels, but never return an endpoint's body,
			// description, or URL from a transient failure.
			switch {
			case errors.Is(err, ErrNeedsSignIn):
				return nil, err // refresh already sanitizes permanent OAuth errors.
			case errors.Is(err, context.Canceled):
				return nil, context.Canceled
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			default:
				return nil, fmt.Errorf("OAuth refresh failed: %s", sanitizedCause(err))
			}
		}
		copy := *tok
		return &copy, nil
	}
}

// Watch blocks until cancellation. Call it once per Source. An invalid file is
// logged once per revision. Transient failures retry with exponential backoff
// from the poll interval up to one minute, with one warning per attempt; success
// or a new file revision resets the backoff.
// Both paths retain the previous token and its state until validation succeeds.
func (s *Source) Watch(ctx context.Context, onReload func()) {
	ticker := time.NewTicker(s.watchInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.reload(ctx, onReload)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Source) reload(ctx context.Context, onReload func()) {
	info, err := os.Stat(s.tokenFile)
	if errors.Is(err, os.ErrNotExist) {
		s.observed = nil
		s.retryAt, s.retryDelay = time.Time{}, 0
		return
	}
	if err != nil {
		slog.Warn("cannot inspect token file", "file", s.tokenFile, "error", err)
		return
	}
	if sameRevision(info, s.observed) {
		if s.retryAt.IsZero() || s.now().Before(s.retryAt) {
			return
		}
	} else {
		// A new revision may repair a credential immediately, even while a
		// previous revision is backing off during an outage.
		s.retryAt, s.retryDelay = time.Time{}, 0
	}
	tok, readInfo, err := readToken(s.tokenFile)
	var identity string
	if tok != nil {
		identity, _ = tok.Extra("identity_user_id").(string)
	}
	s.observed = info
	if readInfo != nil {
		s.observed = readInfo
	}
	if err == nil {
		validationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		tok, err = s.refresh(validationCtx, tok)
		if err == nil {
			err = s.validateFn(validationCtx, authenticatedClient(validationCtx, func(context.Context) (*oauth2.Token, error) { return tok, nil }, nil))
		}
	}
	if err != nil {
		// OAuth errors can include response bodies containing credentials. Do
		// not put those bodies in the log.
		if transient(err) {
			if s.retryDelay == 0 {
				s.retryDelay = min(s.watchInterval, time.Minute)
			} else {
				s.retryDelay = min(2*s.retryDelay, time.Minute)
			}
			s.retryAt = s.now().Add(s.retryDelay)
			if ctx.Err() == nil {
				slog.Warn("token validation temporarily failed; will retry", "file", s.tokenFile, "cause", sanitizedCause(err), "retry_after", s.retryDelay)
			}
		} else {
			s.retryAt, s.retryDelay = time.Time{}, 0
			if ctx.Err() == nil {
				slog.Warn("token replacement rejected; keeping previous credential", "file", s.tokenFile, "cause", sanitizedCause(err))
			}
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	s.retryAt, s.retryDelay = time.Time{}, 0
	s.mu.Lock()
	s.current, s.state = tok, Ready
	s.identityUserID = identity
	s.generation++
	s.mu.Unlock()
	if onReload != nil {
		onReload()
	}
}

func sameRevision(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

func (s *Source) refresh(ctx context.Context, current *oauth2.Token) (*oauth2.Token, error) {
	if current.Valid() {
		copy := *current
		return &copy, nil
	}
	if current.RefreshToken == "" {
		return nil, ErrNeedsSignIn
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ts := s.config.TokenSource(ctx, current)
	for attempt := 0; ; attempt++ {
		tok, err := ts.Token()
		if err == nil {
			return tok, nil
		}
		// oauth2 flattens response-body read errors, so restore the context
		// sentinel before classifying or sanitizing a canceled exchange.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var failure *oauth2.RetrieveError
		if errors.As(err, &failure) {
			if permanentOAuthCode(failure.ErrorCode) {
				// Preserve structured diagnostics without returning descriptions,
				// URIs or bodies that a caller might accidentally log.
				safe := &oauth2.RetrieveError{ErrorCode: failure.ErrorCode}
				if failure.Response != nil {
					safe.Response = &http.Response{StatusCode: failure.Response.StatusCode}
				}
				return nil, fmt.Errorf("%w: %w", ErrNeedsSignIn, safe)
			}
		}
		if attempt >= 2 || !transient(err) {
			return nil, err
		}
		timer := time.NewTimer(s.refreshDelay << attempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func permanentOAuthCode(code string) bool {
	switch code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "invalid_request",
		"invalid_scope", "unsupported_grant_type", "access_denied", "admin_policy_enforced",
		"invalid_rapt", "disabled_client", "deleted_client", "org_internal", "restricted_client":
		return true
	default:
		return false
	}
}

func transient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrNeedsSignIn) ||
		errors.Is(err, errMalformedJSON) || errors.Is(err, errInvalidCredential) {
		return false
	}
	var fileError *os.PathError
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &fileError) || errors.As(err, &syntaxError) || errors.As(err, &typeError) {
		return false
	}
	var validation validationError
	if errors.As(err, &validation) {
		return validation == http.StatusRequestTimeout || validation == http.StatusTooManyRequests || validation < 400 || validation >= 500
	}
	var failure *oauth2.RetrieveError
	if errors.As(err, &failure) {
		if permanentOAuthCode(failure.ErrorCode) {
			return false
		}
		if failure.Response != nil && failure.ErrorCode != "temporarily_unavailable" && failure.ErrorCode != "server_error" {
			status := failure.Response.StatusCode
			if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
				return false
			}
		}
	}
	// URL errors can wrap transport failures, including HTTP/2 stream resets
	// that do not implement net.Error. Reject only known configuration failures.
	var urlError *url.Error
	if errors.As(err, &urlError) {
		endpoint, parseErr := url.Parse(urlError.URL)
		if urlError.Op == "parse" || parseErr != nil ||
			(endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" {
			return false
		}
		var address *net.AddrError
		var verification *tls.CertificateVerificationError
		var record tls.RecordHeaderError
		var authority x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var certificate x509.CertificateInvalidError
		if errors.As(err, &address) || errors.As(err, &verification) || errors.As(err, &record) || errors.Is(err, http.ErrSchemeMismatch) ||
			errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &certificate) {
			return false
		}
	}
	// Unknown errors remain retryable. In particular, oauth2 flattens body-read
	// failures (including deadline exceeded) into strings without an error chain.
	// oauth2 v0.37.0 also flattens non-JSON 2xx token responses and responses
	// without access_token. These remain retryable too: matching their message
	// prefixes would depend on library wording rather than structured errors.
	return true
}

type validationError int

func (e validationError) Error() string {
	return fmt.Sprintf("chat credential validation: HTTP %d", e)
}

// Never format arbitrary OAuth, JSON or URL errors: they may contain token
// bodies, malformed token values, or sensitive query parameters.
func sanitizedCause(err error) string {
	var failure *oauth2.RetrieveError
	if errors.As(err, &failure) {
		code := failure.ErrorCode
		if !permanentOAuthCode(code) && code != "temporarily_unavailable" && code != "server_error" {
			code = "unrecognized"
		}
		if failure.Response != nil {
			return fmt.Sprintf("OAuth %s (HTTP %d)", code, failure.Response.StatusCode)
		}
		return "OAuth " + code
	}
	var validation validationError
	if errors.As(err, &validation) {
		return validation.Error()
	}
	if errors.Is(err, errMalformedJSON) {
		return errMalformedJSON.Error()
	}
	var fileError *os.PathError
	if errors.As(err, &fileError) {
		return fileError.Error()
	}
	if errors.Is(err, ErrNeedsSignIn) {
		return ErrNeedsSignIn.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "operation canceled"
	}
	if transient(err) {
		return "network interruption or timeout"
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		var record tls.RecordHeaderError
		if errors.As(err, &record) || errors.Is(err, http.ErrSchemeMismatch) {
			return "plain HTTP server behind an https URL"
		}
		return "invalid endpoint URL"
	}
	return "invalid credential or configuration"
}

func authenticatedClient(ctx context.Context, token func(context.Context) (*oauth2.Token, error), invalidate func(string)) *http.Client {
	base := http.DefaultTransport
	client, _ := ctx.Value(oauth2.HTTPClient).(*http.Client)
	if client != nil && client.Transport != nil {
		base = client.Transport
	}
	// Request contexts bound API calls; a total client timeout would also cut
	// off large media bodies after the headers have already arrived.
	return &http.Client{Transport: &tokenTransport{token: token, base: base, oauthClient: client, invalidate: invalidate}}
}

type tokenTransport struct {
	token       func(context.Context) (*oauth2.Token, error)
	base        http.RoundTripper
	oauthClient *http.Client
	invalidate  func(string)
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	// Preserve endpoint transport overrides without retaining the construction
	// context's cancellation, deadline, or other request-scoped values.
	if t.oauthClient != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, t.oauthClient)
	}
	tok, err := t.token(ctx)
	if err != nil {
		// RoundTripper owns the request body, even when no HTTP call is made.
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	request := req.Clone(req.Context())
	tok.SetAuthHeader(request)
	response, err := t.base.RoundTrip(request)
	if err == nil && response.StatusCode == http.StatusUnauthorized && t.invalidate != nil {
		// Do not replay the request: its caller owns retry policy. A late 401
		// must not expire a different token installed by a concurrent refresh.
		t.invalidate(tok.AccessToken)
	}
	return response, err
}

func readToken(path string) (*oauth2.Token, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("token file %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("token file %s: %w", path, err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, info, fmt.Errorf("token file %s: %w", path, err)
	}
	var saved struct {
		oauth2.Token
		IdentityUserID string `json:"identity_user_id"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		return nil, info, fmt.Errorf("token file %s: %w: %w", path, errMalformedJSON, err)
	}
	tok := saved.Token
	if saved.IdentityUserID != "" && !validUserID(saved.IdentityUserID) {
		return nil, info, fmt.Errorf("token file %s: %w: identity_user_id must be users/<numeric id>", path, errInvalidCredential)
	}
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		return nil, info, fmt.Errorf("token file %s: %w: access_token or refresh_token is required", path, errInvalidCredential)
	}
	return tok.WithExtra(map[string]any{"identity_user_id": saved.IdentityUserID}), info, nil
}

func loadConfig(path string, ep config.Endpoints, scopes []string) (*oauth2.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("OAuth client file %s: %w", path, err)
	}
	var file struct {
		Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			AuthURL      string `json:"auth_uri"`
			TokenURL     string `json:"token_uri"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("OAuth client file %s: %w", path, err)
	}
	if file.Installed == nil {
		return nil, fmt.Errorf("OAuth client file %s: installed (Desktop) client is required", path)
	}
	client := file.Installed
	if client.ClientID == "" || client.ClientSecret == "" {
		return nil, fmt.Errorf("OAuth client file %s: installed.client_id and installed.client_secret are required", path)
	}
	if ep.OAuthAuthURL == "" {
		ep.OAuthAuthURL = client.AuthURL
	}
	if ep.OAuthTokenURL == "" {
		ep.OAuthTokenURL = client.TokenURL
	}
	if ep.OAuthAuthURL == "" || ep.OAuthTokenURL == "" {
		return nil, fmt.Errorf("OAuth client file %s: auth_uri and token_uri or endpoint overrides are required", path)
	}
	return &oauth2.Config{
		ClientID: client.ClientID, ClientSecret: client.ClientSecret, Scopes: append([]string(nil), scopes...),
		Endpoint: oauth2.Endpoint{AuthURL: ep.OAuthAuthURL, TokenURL: ep.OAuthTokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}, nil
}

// RunAuth performs desktop consent without modifying the existing token until
// exchange, scope validation and the temporary file write have all succeeded.
// If directory syncing fails after the atomic rename, the new token is already
// installed; the returned error reports that partial success explicitly.
func RunAuth(ctx context.Context, clientFile, tokenFile string, ep config.Endpoints, scopes []string, stdout io.Writer, open func(string) error) error {
	return runAuth(ctx, clientFile, tokenFile, ep, scopes, stdout, open, (&net.ListenConfig{}).Listen)
}

func runAuth(ctx context.Context, clientFile, tokenFile string, ep config.Endpoints, scopes []string, stdout io.Writer, open func(string) error, listen func(context.Context, string, string) (net.Listener, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkTokenPath(tokenFile); err != nil {
		return err
	}
	cfg, err := loadConfig(clientFile, ep, scopes)
	if err != nil {
		return err
	}
	listener, err := listenLoopback(ctx, listen)
	if err != nil {
		return err
	}
	cfg.RedirectURL = "http://" + listener.Addr().String() + "/callback"
	state, verifier := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	type result struct {
		code string
		err  error
	}
	callback := make(chan result, 1)
	var accepted sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("state") != state {
			http.Error(w, "invalid OAuth callback", http.StatusBadRequest)
			return
		}
		value := result{code: r.URL.Query().Get("code")}
		if denied := r.URL.Query().Get("error"); denied != "" {
			value.err = fmt.Errorf("OAuth consent failed: %s", denied)
		} else if value.code == "" {
			value.err = errors.New("OAuth callback missing code")
		}
		fmt.Fprintln(w, "Consent received. You can close this window and return to the terminal.")
		accepted.Do(func() { callback <- value })
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		// The callback may still be flushing its response after signaling the
		// exchange. Give it a bounded grace period, even if auth was canceled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}()
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"), oauth2.S256ChallengeOption(verifier))
	if _, err := fmt.Fprintln(stdout, authURL); err != nil {
		return fmt.Errorf("print OAuth consent URL: %w", err)
	}
	if open != nil {
		if err := open(authURL); err != nil {
			// A missing desktop browser must not prevent manually using the
			// printed URL on a machine where the listener is reachable.
			if _, writeErr := fmt.Fprintf(stdout, "Could not open browser (%v); open the URL above manually.\n", err); writeErr != nil {
				return fmt.Errorf("print browser error: %w", writeErr)
			}
		}
	}
	var response result
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-serveErr:
		return fmt.Errorf("OAuth loopback listener %s: %w", listener.Addr(), err)
	case response = <-callback:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if response.err != nil {
		return response.err
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tok, err := cfg.Exchange(exchangeCtx, response.code, oauth2.VerifierOption(verifier))
	if err != nil {
		// oauth2 can flatten body-read failures and include response bodies in
		// other errors. Preserve cancellation sentinels, never endpoint text.
		if exchangeCtx.Err() != nil {
			return fmt.Errorf("OAuth code exchange: %w", exchangeCtx.Err())
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("OAuth code exchange: %w", context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("OAuth code exchange: %w", context.DeadlineExceeded)
		}
		return fmt.Errorf("OAuth code exchange: %s", sanitizedCause(err))
	}
	granted, _ := tok.Extra("scope").(string)
	grants := make(map[string]bool)
	for _, scope := range strings.Fields(granted) {
		grants[scope] = true
	}
	var missing []string
	for _, scope := range scopes {
		if !grants[scope] {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("OAuth consent missing scopes: %s", strings.Join(missing, ", "))
	}
	if tok.RefreshToken == "" {
		return errors.New("OAuth consent returned no refresh_token; repeat consent with offline access")
	}
	identity, err := identityFromIDToken(tok)
	if err != nil {
		return err
	}
	return writeToken(ctx, tokenFile, tok.WithExtra(map[string]any{"identity_user_id": identity}))
}

// The id_token is received directly from the configured OAuth token endpoint
// over TLS in production. It is never accepted from a browser callback.
func identityFromIDToken(tok *oauth2.Token) (string, error) {
	raw, _ := tok.Extra("id_token").(string)
	parts := strings.Split(raw, ".")
	failure := errors.New("OAuth consent returned no valid id_token sub; repeat gc gchat auth with the openid scope")
	if len(parts) != 3 {
		return "", failure
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", failure
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(data, &claims) != nil || !validUserID("users/"+claims.Sub) {
		return "", failure
	}
	return "users/" + claims.Sub, nil
}

func validUserID(id string) bool {
	digits, ok := strings.CutPrefix(id, "users/")
	if !ok || digits == "" {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func listenLoopback(ctx context.Context, listen func(context.Context, string, string) (net.Listener, error)) (net.Listener, error) {
	const address = "127.0.0.1:0"
	listener, err := listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("OAuth loopback listener %s (port 0 requests a free port): %w; check that the loopback interface is up, ephemeral ports are available, and the firewall permits local listeners", address, err)
	}
	return listener, nil
}

func writeToken(ctx context.Context, path string, tok *oauth2.Token) error {
	return writeTokenWithSync(ctx, path, tok, (*os.File).Sync)
}

func writeTokenWithSync(ctx context.Context, path string, tok *oauth2.Token, syncFile func(*os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkTokenPath(path); err != nil {
		return err
	}
	// Each sign-in owns a distinct 0600 file in the destination directory, so
	// overlapping runs cannot reclaim or rename each other's unfinished writes.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write token %s: %w", path, err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	identity, _ := tok.Extra("identity_user_id").(string)
	saved := struct {
		*oauth2.Token
		IdentityUserID string `json:"identity_user_id,omitempty"`
	}{tok, identity}
	if err = json.NewEncoder(f).Encode(saved); err == nil {
		err = syncFile(f)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write token %s: %w", tmp, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkTokenPath(path); err != nil {
		return err
	}
	// A path change after Lstat cannot redirect the write: Rename replaces a
	// symlink itself, rather than following it to overwrite the target.
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace token %s: %w", path, err)
	}
	renamed = true
	// Persist the directory entry as well as the file contents before reporting
	// success. Some filesystems do not support syncing directory descriptors.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("token written to %s but its directory could not be synced: %w", path, err)
	}
	defer dir.Close()
	if err := syncFile(dir); err != nil && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("token written to %s but its directory could not be synced: %w", path, err)
	}
	return nil
}

func checkTokenPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("write token %s: not a regular file", path)
	}
	return nil
}
