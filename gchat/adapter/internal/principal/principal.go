// Package principal decides which class a Chat sender belongs to. Owners are
// configured by address (GCHAT_OWNERS); the organization is a mail domain
// (GCHAT_ORGANIZATION_DOMAIN) or, failing that, the Workspace domain id the
// identity itself belongs to, learned from its own messages. Addresses come
// from the Workspace directory (People API, directory.readonly); without
// that scope the frame shows Chat's users/<id> instead.
package principal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"gchat-adapter/internal/provenance"
	api "google.golang.org/api/chat/v1"
)

// Lookup answers a Chat user's address from the directory, or "" with an error.
type Lookup func(ctx context.Context, userID string) (string, error)

// Options configures a Resolver.
type Options struct {
	// Owners are addresses (user@example.com) or Chat ids (users/123).
	Owners []string
	// OrganizationDomain is the organization's mail domain; empty means unknown.
	OrganizationDomain string
	// Lookup resolves users/<id> to an address; nil means no directory.
	Lookup Lookup
	// Now and FailureTTL bound how often a failed lookup is retried (default 10 minutes).
	Now        func() time.Time
	FailureTTL time.Duration
}

// Resolver classifies senders, caching directory answers for the process.
type Resolver struct {
	owners  map[string]bool
	domain  string
	lookup  Lookup
	now     func() time.Time
	ttl     time.Duration
	mu      sync.Mutex
	address map[string]string    // users/<id> -> address, "" when the directory has none
	failed  map[string]time.Time // users/<id> -> when a lookup last failed
}

func New(o Options) *Resolver {
	r := &Resolver{owners: map[string]bool{}, domain: strings.ToLower(strings.TrimSpace(o.OrganizationDomain)), lookup: o.Lookup,
		now: o.Now, ttl: o.FailureTTL, address: map[string]string{}, failed: map[string]time.Time{}}
	for _, owner := range o.Owners {
		if owner = strings.ToLower(strings.TrimSpace(owner)); owner != "" {
			r.owners[owner] = true
		}
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.ttl == 0 {
		r.ttl = 10 * time.Minute
	}
	return r
}

// Speaker is the frame's view of a sender. identityDomain is the Workspace
// domain id of the identity itself ("" when not yet known).
func (r *Resolver) Speaker(ctx context.Context, u *api.User, identityDomain string) provenance.Speaker {
	if u == nil {
		return provenance.Speaker{Class: provenance.External}
	}
	s := provenance.Speaker{Name: u.DisplayName, Address: u.Name, Class: provenance.External, Bot: u.Type == "BOT"}
	if u.IsAnonymous {
		if s.Name == "" {
			s.Name = "deleted user"
		}
		return s
	}
	if s.Bot {
		return s
	}
	address := r.resolve(ctx, u.Name)
	if address != "" {
		s.Address = address
	}
	id := strings.ToLower(u.Name)
	switch {
	case address != "" && r.owners[strings.ToLower(address)], id != "" && r.owners[id]:
		s.Class = provenance.Owner
	case address != "" && r.domain != "" && strings.HasSuffix(strings.ToLower(address), "@"+r.domain):
		s.Class = provenance.Organization
	case identityDomain != "" && u.DomainId == identityDomain:
		s.Class = provenance.Organization
	}
	return s
}

func (r *Resolver) resolve(ctx context.Context, id string) string {
	if r.lookup == nil || id == "" {
		return ""
	}
	r.mu.Lock()
	address, known := r.address[id]
	failedAt, failed := r.failed[id]
	r.mu.Unlock()
	if known {
		return address
	}
	if failed && r.now().Sub(failedAt) < r.ttl {
		return ""
	}
	// Inbound holds its delivery gate while classifying a speaker. A stalled
	// directory must not hold that gate indefinitely.
	lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	address, err := r.lookup(lookupCtx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failed[id] = r.now()
		return ""
	}
	delete(r.failed, id)
	r.address[id] = address
	return address
}

var userID = regexp.MustCompile(`^users/(\d+)$`)

// DirectoryLookup reads a person's primary address from the People API with
// the authenticated client; peopleBase is the API root. A user outside the
// directory answers "" without error, so it is not asked again.
func DirectoryLookup(client *http.Client, peopleBase string) Lookup {
	base := strings.TrimRight(peopleBase, "/")
	return func(ctx context.Context, id string) (string, error) {
		m := userID.FindStringSubmatch(id)
		if m == nil {
			return "", fmt.Errorf("not a Chat user id: %q", id)
		}
		// people.get takes ReadSourceType values only: PROFILE answers for a
		// Workspace user with directory.readonly. The DIRECTORY_* names belong to
		// searchDirectoryPeople; a wrong one here is HTTP 400 (seen live).
		q := url.Values{"personFields": {"emailAddresses"}, "sources": {"READ_SOURCE_TYPE_PROFILE"}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/people/"+m[1]+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		switch res.StatusCode {
		case http.StatusOK:
		case http.StatusNotFound:
			return "", nil
		default:
			return "", fmt.Errorf("people.get: HTTP %d", res.StatusCode)
		}
		var person struct {
			EmailAddresses []struct {
				Value    string `json:"value"`
				Metadata struct {
					Primary bool `json:"primary"`
				} `json:"metadata"`
			} `json:"emailAddresses"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&person); err != nil {
			return "", err
		}
		for _, e := range person.EmailAddresses {
			if e.Metadata.Primary && e.Value != "" {
				return e.Value, nil
			}
		}
		for _, e := range person.EmailAddresses {
			if e.Value != "" {
				return e.Value, nil
			}
		}
		return "", errors.New("people.get: no address")
	}
}
