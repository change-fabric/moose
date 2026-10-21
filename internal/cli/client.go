package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitUsage    = 1
	ExitAuth     = 2
	ExitNotFound = 3
	ExitServer   = 4
)

// ExitError carries a process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode maps an error from Execute to a process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ExitUsage
}

func usageErr(format string, a ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, a...)}
}

// APIError is a non-2xx response from the hub.
type APIError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func statusExit(status int) int {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ExitAuth
	case status == http.StatusNotFound:
		return ExitNotFound
	case status >= 500:
		return ExitServer
	}
	return ExitUsage
}

// Credentials is the saved login.
type Credentials struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// CredentialsPath returns MOOSE_CREDENTIALS, or
// $XDG_CONFIG_HOME/moose/credentials.json, or ~/.config/moose/credentials.json.
func CredentialsPath(getenv func(string) string) (string, error) {
	if p := getenv("MOOSE_CREDENTIALS"); p != "" {
		return p, nil
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "moose", "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "moose", "credentials.json"), nil
}

func loadCredentials(path string) (Credentials, error) {
	var c Credentials
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func saveCredentials(path string, c Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Client calls the moose REST API.
type Client struct {
	BaseURL   string
	Token     string
	HTTP      *http.Client
	UserAgent string
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	if c.BaseURL == "" {
		return usageErr("no hub URL: run `moose login --url URL` or set MOOSE_URL")
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return usageErr("bad request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return &ExitError{Code: ExitServer, Err: fmt.Errorf("request failed: %w", err)}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return &ExitError{Code: ExitServer, Err: err}
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, ae)
		if ae.Code == "" {
			ae.Code = http.StatusText(resp.StatusCode)
		}
		return &ExitError{Code: statusExit(resp.StatusCode), Err: ae}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &ExitError{Code: ExitServer, Err: fmt.Errorf("decode response: %w", err)}
		}
	}
	return nil
}
