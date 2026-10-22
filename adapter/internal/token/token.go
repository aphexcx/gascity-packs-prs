// Package token owns the desktop OAuth credential and its sign-in lifecycle.
package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
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

// Source shares refreshed tokens across HTTP and Pub/Sub clients. The mutex
// protecting state is never held during network calls, so health stays readable.
type Source struct {
	config        *oauth2.Config
	tokenFile     string
	mu            sync.Mutex
	refreshMu     sync.Mutex
	current       *oauth2.Token
	state         State
	generation    uint64
	observed      os.FileInfo // owned by the single Watch loop after New
	watchInterval time.Duration
	retryAt       time.Time
	retryDelay    time.Duration
	now           func() time.Time
	refreshDelay  time.Duration
	validateFn    func(context.Context, *http.Client) error
}

func New(clientFile, tokenFile string, ep config.Endpoints, scopes []string) (*Source, error) {
	if strings.TrimSpace(ep.ChatAPIBase) == "" {
		return nil, errors.New("ChatAPIBase is required for credential validation")
	}
	cfg, err := loadConfig(clientFile, ep, scopes)
	if err != nil {
		return nil, err
	}
	s := &Source{config: cfg, tokenFile: tokenFile, state: Missing,
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
	return s, nil
}

func (s *Source) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Client checks the credential immediately. Its transport continues to report
// permanent refresh failures through Source.State on later requests as well.
func (s *Source) Client(ctx context.Context) (*http.Client, error) {
	ts := s.TokenSource(ctx)
	if _, err := ts.Token(); err != nil {
		return nil, err
	}
	// Do not wrap ts in ReuseTokenSource: Source already caches, and a second
	// cache would retain the old access token after a validated file replacement.
	return authenticatedClient(ctx, ts), nil
}

func (s *Source) TokenSource(ctx context.Context) oauth2.TokenSource {
	return &sourceToken{source: s, ctx: ctx}
}

type sourceToken struct {
	source *Source
	ctx    context.Context
}

func (ts *sourceToken) Token() (*oauth2.Token, error) {
	s := ts.source
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	for {
		if err := ts.ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		current, generation, state := s.current, s.generation, s.state
		s.mu.Unlock()
		if current == nil || state == NeedsSignIn {
			return nil, ErrNeedsSignIn
		}
		tok, err := s.refresh(ts.ctx, current)
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
			return nil, err
		}
		copy := *tok
		return &copy, nil
	}
}

// Watch blocks until cancellation. Call it once per Source. An invalid file is
// logged once per revision; transient validation failures retry on a later poll.
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
	s.observed = info
	if readInfo != nil {
		s.observed = readInfo
	}
	if err == nil {
		validationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		tok, err = s.refresh(validationCtx, tok)
		if err == nil {
			err = s.validateFn(validationCtx, authenticatedClient(validationCtx, oauth2.StaticTokenSource(tok)))
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
			slog.Warn("token validation temporarily failed; will retry", "file", s.tokenFile, "cause", sanitizedCause(err), "retry_after", s.retryDelay)
		} else {
			s.retryAt, s.retryDelay = time.Time{}, 0
			slog.Warn("token replacement rejected; keeping previous credential", "file", s.tokenFile, "cause", sanitizedCause(err))
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	s.retryAt, s.retryDelay = time.Time{}, 0
	s.mu.Lock()
	s.current, s.state = tok, Ready
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
		var failure *oauth2.RetrieveError
		if errors.As(err, &failure) {
			if permanentOAuthCode(failure.ErrorCode) {
				return nil, fmt.Errorf("%w: %w", ErrNeedsSignIn, failure)
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
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrNeedsSignIn) {
		return false
	}
	var validation validationError
	if errors.As(err, &validation) {
		return validation == 429 || validation >= 500
	}
	var failure *oauth2.RetrieveError
	if errors.As(err, &failure) {
		if permanentOAuthCode(failure.ErrorCode) {
			return false
		}
		return failure.ErrorCode == "temporarily_unavailable" || failure.ErrorCode == "server_error" ||
			(failure.Response != nil && (failure.Response.StatusCode == 429 || failure.Response.StatusCode >= 500))
	}
	// url.Error implements net.Error even for permanent configuration errors.
	// Classify its cause instead of assuming that the interface means network IO.
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return transient(urlError.Err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	// oauth2 v0.37 formats response-body read errors with %v, losing the
	// original error chain. Recognize only its read-error prefix and known
	// transport failures; never inspect a RetrieveError response body.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if detail, ok := strings.CutPrefix(cause.Error(), "oauth2: cannot fetch token: "); ok {
			return detail == "EOF" || detail == "unexpected EOF" ||
				strings.HasSuffix(detail, ": connection reset by peer") ||
				strings.HasSuffix(detail, ": i/o timeout")
		}
	}
	return false
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
		return "invalid endpoint URL"
	}
	return "invalid credential or configuration"
}

func authenticatedClient(ctx context.Context, ts oauth2.TokenSource) *http.Client {
	base := http.DefaultTransport
	if client, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && client.Transport != nil {
		base = client.Transport
	}
	// Request contexts bound API calls; a total client timeout would also cut
	// off large media bodies after the headers have already arrived.
	return &http.Client{Transport: &oauth2.Transport{Source: ts, Base: base}}
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
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, info, fmt.Errorf("token file %s: %w: %w", path, errMalformedJSON, err)
	}
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		return nil, info, fmt.Errorf("token file %s: access_token or refresh_token is required", path)
	}
	return &tok, info, nil
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
func RunAuth(ctx context.Context, clientFile, tokenFile string, ep config.Endpoints, scopes []string, stdout io.Writer, open func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := loadConfig(clientFile, ep, scopes)
	if err != nil {
		return err
	}
	const address = "127.0.0.1:0"
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("OAuth loopback listener %s (port 0 requests a free port): %w; retry with another port", address, err)
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
		accepted.Do(func() { callback <- value })
		fmt.Fprintln(w, "Consent received. You can close this window and return to the terminal.")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer server.Close()
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
		return fmt.Errorf("OAuth code exchange: %w", err)
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
	return writeToken(ctx, tokenFile, tok)
}

func writeToken(ctx context.Context, path string, tok *oauth2.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Exclusive creation also refuses stale files and symlinks instead of
	// truncating an existing credential through the temporary path.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("write token %s: %w", tmp, err)
	}
	renamed := false
	defer func() {
		// Once renamed, this path can belong to a concurrent sign-in. Only
		// clean up the temporary file when this write did not commit.
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	if err = json.NewEncoder(f).Encode(tok); err == nil {
		err = f.Sync()
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
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace token %s: %w", path, err)
	}
	renamed = true
	return nil
}
