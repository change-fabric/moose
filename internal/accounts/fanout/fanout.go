// Package fanout resolves account references and runs one call across many
// accounts under shared limits, with signed per-account paging cursors.
package fanout

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/email/provider"
)

// Fan-out limits (R3): at most 8 accounts at once, 6 seconds for each, 20
// seconds for the whole call. There is no cap on the number of accounts.
const (
	DefaultConcurrency = 8
	DefaultPerAccount  = 6 * time.Second
	DefaultOverall     = 20 * time.Second
	// HintAbove is the account count past which results suggest narrowing.
	HintAbove = 16
)

// Limits bounds one fan-out call. Zero fields take the defaults.
type Limits struct {
	Concurrency int
	PerAccount  time.Duration
	Overall     time.Duration
}

func (l Limits) WithDefaults() Limits {
	if l.Concurrency <= 0 {
		l.Concurrency = DefaultConcurrency
	}
	if l.PerAccount <= 0 {
		l.PerAccount = DefaultPerAccount
	}
	if l.Overall <= 0 {
		l.Overall = DefaultOverall
	}
	return l
}

// Per-account statuses in the `accounts` array of a fan-out result.
const (
	StatusOK          = "ok"
	StatusTimeout     = "timeout"
	StatusError       = "error"
	StatusNeedsReauth = "needs_reauth"
	StatusSkipped     = "skipped"
)

// AccountReport says how one account fared in a fan-out call.
type AccountReport struct {
	AccountID string `json:"account_id"`
	Address   string `json:"address"`
	Status    string `json:"status"`
	Returned  int    `json:"returned"`
	HasMore   bool   `json:"has_more"`
	// Reason is a short machine-readable cause for timeout, error and skipped:
	// rate_limited, provider_error, deadline, account_disconnected, and so on.
	Reason string `json:"reason,omitempty"`
}

// PerAccountResult is what one account's run returns.
type PerAccountResult[T any] struct {
	Items []T
	// Next is the provider cursor for the next page, empty when exhausted.
	Next string
}

type outcome[T any] struct {
	acct store.Account
	res  PerAccountResult[T]
	err  error
}

// Run runs run for every ok account, at most lim.Concurrency at a time,
// each under lim.PerAccount and all under lim.Overall. Accounts that need
// reauthorization or are otherwise not ok are reported and not called. It
// returns each answering account's page by account id, a report per account in
// account order, and the ids of accounts whose position should be kept for a
// later call (timeout, rate limit).
func Run[T any](ctx context.Context, lim Limits, accts []store.Account,
	run func(ctx context.Context, a store.Account) (PerAccountResult[T], error)) (map[string]PerAccountResult[T], []AccountReport, map[string]bool) {
	lim = lim.WithDefaults()
	reports := make([]AccountReport, len(accts))
	results := map[string]PerAccountResult[T]{}
	keep := map[string]bool{}
	index := map[string]int{}

	var live []store.Account
	for i, a := range accts {
		index[a.ID] = i
		reports[i] = AccountReport{AccountID: a.ID, Address: a.Address, Status: StatusOK}
		switch a.Status {
		case store.StatusOK:
			live = append(live, a)
		case store.StatusNeedsReauth:
			reports[i].Status, reports[i].Reason = StatusNeedsReauth, "reconnect_on_accounts_page"
		default:
			reports[i].Status, reports[i].Reason = StatusSkipped, "account_"+a.Status
		}
	}

	octx, cancel := context.WithTimeout(ctx, lim.Overall)
	defer cancel()
	sem := make(chan struct{}, lim.Concurrency)
	done := make(chan outcome[T], len(live)) // buffered: a late goroutine never blocks
	for _, a := range live {
		go func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-octx.Done():
				done <- outcome[T]{acct: a, err: octx.Err()}
				return
			}
			actx, acancel := context.WithTimeout(octx, lim.PerAccount)
			defer acancel()
			res, err := run(actx, a)
			if err == nil && actx.Err() != nil {
				err = actx.Err()
			}
			done <- outcome[T]{acct: a, res: res, err: err}
		}()
	}

	pending := map[string]bool{}
	for _, a := range live {
		pending[a.ID] = true
	}
	handle := func(o outcome[T]) {
		delete(pending, o.acct.ID)
		i := index[o.acct.ID]
		if o.err == nil {
			results[o.acct.ID] = o.res
			reports[i].Returned, reports[i].HasMore = len(o.res.Items), o.res.Next != ""
			return
		}
		st, reason, retry := Classify(o.err)
		reports[i].Status, reports[i].Reason, reports[i].HasMore = st, reason, retry
		keep[o.acct.ID] = retry
	}
	for len(pending) > 0 {
		select {
		case o := <-done:
			handle(o)
		case <-octx.Done():
			// Take whatever finished at the deadline; the rest timed out.
		drain:
			for len(pending) > 0 {
				select {
				case o := <-done:
					handle(o)
				default:
					break drain
				}
			}
			for id := range pending {
				i := index[id]
				reports[i].Status, reports[i].Reason, reports[i].HasMore = StatusTimeout, "deadline", true
				keep[id] = true
			}
			pending = nil
		}
	}
	return results, reports, keep
}

// Classify maps a provider error to a report status, a reason and whether the
// account's position should be kept for a later call.
func Classify(err error) (status, reason string, retry bool) {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return StatusTimeout, "deadline", true
	case errors.Is(err, provider.ErrAuth):
		return StatusNeedsReauth, "reconnect_on_accounts_page", false
	case errors.Is(err, provider.ErrRateLimited):
		return StatusError, "rate_limited", true
	case errors.Is(err, provider.ErrInvalidQuery):
		return StatusError, "invalid_query", false
	}
	return StatusError, "provider_error", false
}

// Retry429 runs fn, retrying with exponential backoff (200ms, 400ms) while it
// reports provider.ErrRateLimited. Three attempts at most; the last error is
// returned, so a persistent 429 fails that account only.
func Retry429(ctx context.Context, sleep func(context.Context, time.Duration) error, fn func() error) error {
	var err error
	delay := 200 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		if err = fn(); !errors.Is(err, provider.ErrRateLimited) {
			return err
		}
		if attempt == 2 {
			break
		}
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
		delay *= 2
	}
	return err
}

// Cursor: an opaque, HMAC-signed position per account, so a page boundary
// survives across calls and Lambda instances, and a tampered or foreign cursor
// is rejected.
// CursorBody is the signed payload of a cursor.
type CursorBody struct {
	V     int               `json:"v"`
	Owner string            `json:"o"`
	Query string            `json:"q"`
	Pos   map[string]string `json:"p"`
}

// ErrBadCursor is returned for a cursor that fails verification.
var ErrBadCursor = errors.New("cursor is invalid or was issued for a different search")

func SealCursor(key []byte, c CursorBody) string {
	c.V = 1
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(cursorMAC(key, raw))
}

func OpenCursor(key []byte, s, owner, query string) (CursorBody, error) {
	body, sig, ok := strings.Cut(s, ".")
	if !ok {
		return CursorBody{}, ErrBadCursor
	}
	raw, err1 := base64.RawURLEncoding.DecodeString(body)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, cursorMAC(key, raw)) {
		return CursorBody{}, ErrBadCursor
	}
	var c CursorBody
	if err := json.Unmarshal(raw, &c); err != nil || c.V != 1 || c.Owner != owner || c.Query != query {
		return CursorBody{}, ErrBadCursor
	}
	return c, nil
}

func cursorMAC(key []byte, raw []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("moose-email/cursor/v1\x00"))
	m.Write(raw)
	return m.Sum(nil)
}

// Fingerprint is a short stable digest of v, used to bind a cursor to a search.
func Fingerprint(v any) string {
	raw, _ := json.Marshal(v)
	h := sha256.Sum256(raw)
	return fmt.Sprintf("%x", h[:8])
}
