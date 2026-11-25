// Package accountprov turns a stored account into a live provider.Provider.
//
// It owns the per-account access-token cache (R10): the sealed Google refresh
// token is opened and exchanged at most once per token lifetime however many
// calls arrive together, an invalid_grant answer moves the account to
// needs_reauth, and nothing here ever logs or returns a token.
package accountprov

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/gmail"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/store"
)

// refreshSkew is how early a cached access token is treated as expired.
const refreshSkew = time.Minute

// Source builds providers for accounts. The zero value is not usable: set
// Store, Seal and OAuth. It is safe for concurrent use.
type Source struct {
	Store store.Store
	Seal  envelope.Sealer
	// OAuth is the tenant's Google OAuth client; its token endpoint serves refreshes.
	OAuth *oauth2.Config
	// HTTPClient carries token endpoint calls. Nil means http.DefaultClient.
	HTTPClient *http.Client
	// GmailOptions are extra options for the Gmail client (tests pass
	// option.WithEndpoint).
	GmailOptions []option.ClientOption
	Now          func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	mu  sync.Mutex
	tok *oauth2.Token
}

// Provider returns the provider for account a. It never contacts Google: the
// access token is fetched lazily by the first request that needs it.
func (s *Source) Provider(ctx context.Context, a store.Account) (provider.Provider, error) {
	switch a.Provider {
	case "gmail":
		opts := append([]option.ClientOption{option.WithTokenSource(tokenFunc(func() (*oauth2.Token, error) {
			return s.Token(ctx, a)
		}))}, s.GmailOptions...)
		return gmail.New(ctx, opts...)
	}
	return nil, fmt.Errorf("accountprov: unsupported provider %q", a.Provider)
}

type tokenFunc func() (*oauth2.Token, error)

func (f tokenFunc) Token() (*oauth2.Token, error) { return f() }

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) entry(id string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]*entry{}
	}
	e := s.entries[id]
	if e == nil {
		e = &entry{}
		s.entries[id] = e
	}
	return e
}

// Token returns a valid access token for a. Concurrent callers for one
// account share a single refresh: the first exchanges the refresh token and
// the rest find the fresh token in the cache. A refresh answered with
// invalid_grant marks the account needs_reauth and returns provider.ErrAuth.
func (s *Source) Token(ctx context.Context, a store.Account) (*oauth2.Token, error) {
	e := s.entry(a.ID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tok != nil && e.tok.AccessToken != "" && e.tok.Expiry.After(s.now().Add(refreshSkew)) {
		return e.tok, nil
	}
	if len(a.RefreshToken) == 0 {
		return nil, fmt.Errorf("%w: account has no stored credential", provider.ErrAuth)
	}
	refresh, err := s.Seal.Open(ctx, a.RefreshToken, envelope.Context{Owner: a.Owner, AccountID: a.ID})
	if err != nil {
		return nil, fmt.Errorf("accountprov: open stored credential: %w", err)
	}
	xctx := ctx
	if s.HTTPClient != nil {
		xctx = context.WithValue(ctx, oauth2.HTTPClient, s.HTTPClient)
	}
	tok, err := s.OAuth.TokenSource(xctx, &oauth2.Token{RefreshToken: string(refresh)}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && (re.ErrorCode == "invalid_grant" || strings.Contains(string(re.Body), "invalid_grant")) {
			s.markReauth(ctx, a)
			return nil, fmt.Errorf("%w: Google no longer accepts this account's credential (invalid_grant)", provider.ErrAuth)
		}
		return nil, fmt.Errorf("accountprov: refresh: %w", err)
	}
	e.tok = tok
	return tok, nil
}

// markReauth records needs_reauth on the stored account. A failure to record
// it is not fatal: the caller still gets ErrAuth, and the next call retries.
func (s *Source) markReauth(ctx context.Context, a store.Account) {
	cur, err := s.Store.GetAccount(ctx, a.Owner, a.ID)
	if err != nil || cur.Status == store.StatusDisconnected {
		return
	}
	cur.Status, cur.LastError, cur.Updated = store.StatusNeedsReauth, "invalid_grant", s.now().UTC()
	_ = s.Store.UpdateAccount(ctx, cur)
}
