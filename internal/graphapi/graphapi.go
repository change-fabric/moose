// Package graphapi is the shared Microsoft Graph HTTP core: base URL, bearer
// auth from an oauth2.TokenSource, the Prefer headers every request carries,
// and the mapping of Graph and Entra error answers onto provider sentinels. It
// is hand-written over net/http and golang.org/x/oauth2; there is no
// msgraph-sdk-go and no MSAL. Mail (internal/email/graph) builds on it, and
// the calendar will too.
package graphapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/email/provider"
)

// DefaultBaseURL is Microsoft Graph v1.0.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// PreferImmutableID asks Graph for ids that survive a move between folders.
// Every request carries it, so ids stored in approvals and ledgers stay valid.
const PreferImmutableID = `IdType="ImmutableId"`

// maxErrorBody bounds how much of an error answer is read.
const maxErrorBody = 64 << 10

// Client is a Graph HTTP client for one account.
type Client struct {
	// BaseURL is the Graph root without a trailing slash. Tests point it at an
	// httptest server.
	BaseURL string
	// HTTP performs the requests. New wires bearer auth into it.
	HTTP *http.Client
}

// New returns a Client that authorizes every request from ts. A nil ts sends no
// Authorization header, which is for tests against a stub.
func New(ts oauth2.TokenSource) *Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	if ts != nil {
		hc.Transport = &oauth2.Transport{Source: oauth2.ReuseTokenSource(nil, ts), Base: http.DefaultTransport}
	}
	return &Client{BaseURL: DefaultBaseURL, HTTP: hc}
}

// Request describes one call. Path is relative to BaseURL, already escaped
// (use Segment for ids). Prefer values are added to ImmutableId.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Body   io.Reader
	// Header is added to the request. Prefer values are merged, not replaced.
	Header http.Header
	// ContentType is set when Body is non-nil.
	ContentType string
}

// Segment escapes one path segment, such as a message id.
func Segment(s string) string { return url.PathEscape(s) }

// Do sends r. A 2xx answer is returned with its body open for the caller to
// close; anything else is closed and mapped to an error wrapping a provider
// sentinel.
func (c *Client) Do(ctx context.Context, r Request) (*http.Response, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	u := strings.TrimRight(c.BaseURL, "/") + r.Path
	if len(r.Query) > 0 {
		// OData accepts %20 for a space everywhere; "+" is not reliably a space.
		u += "?" + strings.ReplaceAll(r.Query.Encode(), "+", "%20")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r.Body)
	if err != nil {
		return nil, fmt.Errorf("graph: %w", err)
	}
	for k, vs := range r.Header {
		if http.CanonicalHeaderKey(k) == "Prefer" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	prefer := append([]string{PreferImmutableID}, r.Header.Values("Prefer")...)
	req.Header.Set("Prefer", strings.Join(prefer, ", "))
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if r.Body != nil && r.ContentType != "" {
		req.Header.Set("Content-Type", r.ContentType)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, mapTransportErr(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, MapResponse(resp)
}

// GetJSON GETs path and decodes the JSON answer into out.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, header http.Header, out any) error {
	resp, err := c.Do(ctx, Request{Path: path, Query: q, Header: header})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("graph: decode %s: %w", path, err)
	}
	return nil
}

// mapTransportErr classifies a failure before any HTTP answer. A token refresh
// that Entra refused for a reason only reauthorization fixes is ErrAuth.
func mapTransportErr(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if isReauthText(string(re.Body)) || isReauthText(re.ErrorCode) {
			return fmt.Errorf("%w: token refresh refused: %s", provider.ErrAuth, re.ErrorCode)
		}
		return fmt.Errorf("graph: token refresh: %w", err)
	}
	return fmt.Errorf("graph: %w", err)
}

// reauthMarkers are the Entra answers that mean the stored refresh token or
// consent is gone and the account needs the owner to reconnect it.
var reauthMarkers = []string{
	"invalid_grant", "interaction_required", "AADSTS70008", "AADSTS700082",
}

func isReauthText(s string) bool {
	for _, m := range reauthMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// graphError is the body shape of a Graph error answer.
type graphError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// MapResponse turns a non-2xx Graph answer into an error that wraps a provider
// sentinel. The body is read here (bounded); the caller closes it.
//
//	401, or a claims challenge, or an AADSTS reauth code  -> ErrAuth
//	404, ErrorItemNotFound, ErrorInvalidIdMalformed       -> ErrNotFound
//	429, or 503 with Retry-After                          -> RateLimitError
//	400 InefficientFilter and other invalid requests      -> ErrInvalidQuery
//	any other 4xx                                         -> ErrRejected
func MapResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var ge graphError
	_ = json.Unmarshal(raw, &ge)
	return &Error{Status: resp.StatusCode, Code: ge.Error.Code, Err: mapResponse(resp, raw)}
}

// Error is a mapped Graph answer. It keeps the HTTP status and Graph error code
// next to the sentinel-wrapping error, so a caller that must tell one refusal
// from another (a send that was provably rejected) can, without parsing text.
type Error struct {
	Status int
	Code   string
	Err    error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func mapResponse(resp *http.Response, raw []byte) error {
	var ge graphError
	_ = json.Unmarshal(raw, &ge)
	code, msg := ge.Error.Code, ge.Error.Message
	detail := fmt.Sprintf("graph %d %s: %s", resp.StatusCode, code, truncate(msg, 300))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &provider.RateLimitError{RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), Err: errors.New(detail)}
	case resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") != "":
		return &provider.RateLimitError{RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), Err: errors.New(detail)}
	case resp.StatusCode == http.StatusUnauthorized,
		strings.Contains(resp.Header.Get("WWW-Authenticate"), "claims="),
		isReauthText(msg), isReauthText(code), isReauthText(string(raw)):
		return fmt.Errorf("%w: %s", provider.ErrAuth, detail)
	case resp.StatusCode == http.StatusNotFound, isNotFoundCode(code):
		return fmt.Errorf("%w: %s", provider.ErrNotFound, detail)
	case code == "InefficientFilter", strings.Contains(msg, "InefficientFilter"),
		resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: %s", provider.ErrInvalidQuery, detail)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: %s", provider.ErrRejected, detail)
	}
	return fmt.Errorf("graph: %s", detail)
}

func isNotFoundCode(code string) bool {
	switch code {
	case "ErrorItemNotFound", "ErrorInvalidIdMalformed", "ResourceNotFound", "itemNotFound", "ErrorFolderNotFound":
		return true
	}
	return false
}

// ParseRetryAfter reads a Retry-After value, whole seconds (what Graph sends)
// or an HTTP date. Anything unusable is zero, meaning no hint.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
