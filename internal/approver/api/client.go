package api

import (
	"bytes"
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
)

// ClientTimeout is moose-email's per-call timeout against the approver.
const ClientTimeout = 5 * time.Second

// Error is a non-2xx answer from the approver.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("approver: %d %s: %s", e.Status, e.Code, e.Message)
}

// ErrAlreadyConsumed is returned by Consume on a 409 already_consumed.
var ErrAlreadyConsumed = errors.New("approver: request already consumed")

// Client is the HMAC client moose-email uses. Every call has a 5 second
// timeout and is retried once on a 5xx answer. That is safe because creates
// are idempotent by key, reads are reads, and a consume is idempotent for the
// same claimant.
type Client struct {
	BaseURL string
	Secret  []byte
	HTTP    *http.Client
	Now     func() time.Time
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: ClientTimeout}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return 0, err
		}
	}
	u, err := url.JoinPath(c.BaseURL, path)
	if err != nil {
		return 0, err
	}
	var status int
	for attempt := 0; attempt < 2; attempt++ {
		status, err = c.once(ctx, method, u, body, out)
		var ae *Error
		if errors.As(err, &ae) && ae.Status >= 500 && attempt == 0 {
			continue
		}
		break
	}
	return status, err
}

func (c *Client) once(ctx context.Context, method, rawURL string, body []byte, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, ClientTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := c.now().Unix()
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderSignature, Sign(c.Secret, method, req.URL.RequestURI(), ts, body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode)}
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && ae.Code != "" {
			e.Code, e.Message = ae.Code, ae.Message
		}
		return resp.StatusCode, e
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("approver: undecodable answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Create asks for a request. created is false when the idempotency key already
// had one and the existing request came back.
func (c *Client) Create(ctx context.Context, in CreateBody) (resp CreateResponse, created bool, err error) {
	status, err := c.do(ctx, http.MethodPost, Prefix+"requests", in, &resp)
	return resp, status == http.StatusCreated, err
}

// Status reads a request's status and, once approved, its receipt.
func (c *Client) Status(ctx context.Context, id string) (StatusResponse, error) {
	var out StatusResponse
	_, err := c.do(ctx, http.MethodGet, Prefix+"requests/"+url.PathEscape(id), nil, &out)
	return out, err
}

// Consume flips an approved request to consumed as claimant. Calling again
// with the same claimant succeeds; another claimant gets ErrAlreadyConsumed.
func (c *Client) Consume(ctx context.Context, id, claimant string) (StatusResponse, error) {
	var out StatusResponse
	_, err := c.do(ctx, http.MethodPost, Prefix+"requests/"+url.PathEscape(id)+"/consume", map[string]string{"claimant": claimant}, &out)
	var ae *Error
	if errors.As(err, &ae) && ae.Status == http.StatusConflict && strings.HasPrefix(ae.Code, "already_consumed") {
		return out, ErrAlreadyConsumed
	}
	return out, err
}
