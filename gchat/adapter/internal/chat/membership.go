package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/provenance"
	"golang.org/x/oauth2"
	api "google.golang.org/api/chat/v1"
)

// MembershipError retains only protocol status, never Google's response body.
type MembershipError struct {
	HTTP   int
	Status string
}

func (e *MembershipError) Error() string { return fmt.Sprintf("Google HTTP %d %s", e.HTTP, e.Status) }

var membershipSpace = regexp.MustCompile(`^spaces/[A-Za-z0-9_-]+$`)
var googleStatus = regexp.MustCompile(`^[A-Z][A-Z_0-9]{0,63}$`)

func (c *Client) JoinIdentity() (string, error) { return c.reactionIdentity() }

func (c *Client) GetSpace(ctx context.Context, name string) (*api.Space, error) {
	return c.api.Spaces.Get(name).Context(ctx).Do()
}

func (c *Client) JoinOwner(ctx context.Context, owners []string, sender *api.User) bool {
	r := principal.New(principal.Options{Owners: owners, Lookup: principal.DirectoryLookup(c.httpClient, c.ep.PeopleAPIBase)})
	return r.Speaker(ctx, sender, "").Class == provenance.Owner
}

func (c *Client) ReadOwnMembership(ctx context.Context, space, identity string) (*api.Membership, error) {
	if !membershipSpace.MatchString(space) || !validUserID.MatchString(identity) {
		return nil, errors.New("invalid membership resource")
	}
	return membershipCall(ctx, c.httpClient, c.ep.ChatAPIBase, http.MethodGet, space+"/members/"+strings.TrimPrefix(identity, "users/"), nil)
}

// PrepareJoinSelf validates the candidate acceptance call before approval is
// consumed. The returned function sends it after consumption is durable, without
// a second local expiry check that could spend approval without sending a write.
// UNPROVEN: Google documents
// create's shape (G3/G4), not whether an invited user can accept by naming herself
// (U1). There is exactly one POST: no retries, refresh, or redirects.
func (c *Client) PrepareJoinSelf(ctx context.Context, space, identity string, tok *oauth2.Token) (func() (*api.Membership, error), error) {
	if !membershipSpace.MatchString(space) || !validUserID.MatchString(identity) || tok == nil || !tok.Valid() {
		return nil, errors.New("invalid self join")
	}
	body, err := json.Marshal(&api.Membership{Member: &api.User{Name: identity, Type: "HUMAN"}})
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(tok)}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func() (*api.Membership, error) {
		return membershipCall(ctx, client, c.ep.ChatAPIBase, http.MethodPost, space+"/members", body)
	}, nil
}

func membershipCall(ctx context.Context, client *http.Client, base, method, name string, body []byte) (*api.Membership, error) {
	if base == "" {
		base = "https://chat.googleapis.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/v1/"+name, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var reply struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&reply)
		status := reply.Error.Status
		if !googleStatus.MatchString(status) {
			status = "UNKNOWN"
		}
		return nil, &MembershipError{HTTP: res.StatusCode, Status: status}
	}
	var membership api.Membership
	if err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&membership); err != nil {
		return nil, err
	}
	return &membership, nil
}
