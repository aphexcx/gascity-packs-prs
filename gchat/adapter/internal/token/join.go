package token

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// JoinScope is deliberately absent from both standing scope lists.
const JoinScope = "https://www.googleapis.com/auth/chat.memberships"

// MintJoin asks once for exactly the join permission. It has no Source, cache,
// identity lookup, watch, disk write, health callback, or notification callback.
// The caller either discards the token after a check or uses it for one create.
func MintJoin(ctx context.Context, socket string) (*oauth2.Token, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	k := &keyholder{socket: socket, scopes: []string{JoinScope}, client: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	tok, err := k.mint(ctx)
	if err == nil && !tok.Expiry.After(time.Now()) {
		return nil, errors.New("expired join token")
	}
	return tok, err
}

// JoinRefusal exposes only known codes, never a key holder's detail or socket.
func JoinRefusal(err error) string {
	var e keyholderError
	if errors.As(err, &e) && e.status >= 400 && e.status < 500 && permanentKeyholderCodes[e.code] {
		return e.code
	}
	return ""
}
