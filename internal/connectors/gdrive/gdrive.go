// Package gdrive is the Google Drive connector. It authenticates as a service
// account (no impersonation) and sees exactly what has been shared with that
// account, checked live on every request.
package gdrive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/core"
)

// ID is the connector and source id.
const ID = "gdrive"

const (
	folderMime = "application/vnd.google-apps.folder"
	fileFields = "id,name,mimeType,modifiedTime,webViewLink,parents,trashed"
	listFields = "nextPageToken, files(" + fileFields + ")"
)

var (
	// ErrUpstreamUnavailable means Drive kept failing after retries.
	ErrUpstreamUnavailable = errors.New("upstream unavailable")
	// ErrDocumentTooLarge means Drive refused to export the document.
	ErrDocumentTooLarge = errors.New("document too large")
)

// DefaultBackoff is the fixed retry schedule: 4 retries after the first try.
var DefaultBackoff = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}

// Connector implements core.Connector over Drive v3.
type Connector struct {
	svc     *drive.Service
	backoff []time.Duration
	email   string
}

var (
	_ core.Connector = (*Connector)(nil)
	_ core.Describer = (*Connector)(nil)
)

// New builds a connector from a service account JSON key. Extra options are
// appended after the credentials (tests pass an endpoint).
func New(ctx context.Context, keyJSON []byte, opts ...option.ClientOption) (*Connector, error) {
	cfg, err := google.JWTConfigFromJSON(keyJSON, drive.DriveReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("gdrive: parse service account key: %w", err)
	}
	// No Subject: personal Gmail sharing, no domain-wide delegation.
	all := append([]option.ClientOption{option.WithHTTPClient(cfg.Client(ctx))}, opts...)
	c, err := NewWithOptions(ctx, all...)
	if err != nil {
		return nil, err
	}
	c.email = cfg.Email
	return c, nil
}

// NewWithOptions builds a connector from raw client options (used by tests
// with option.WithEndpoint and option.WithoutAuthentication).
func NewWithOptions(ctx context.Context, opts ...option.ClientOption) (*Connector, error) {
	svc, err := drive.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gdrive: new service: %w", err)
	}
	return &Connector{svc: svc, backoff: DefaultBackoff}, nil
}

// SetBackoff overrides the retry schedule (tests use zero delays).
func (c *Connector) SetBackoff(b []time.Duration) { c.backoff = b }

// ID implements core.Connector.
func (c *Connector) ID() string { return ID }

// Name implements core.Describer.
func (c *Connector) Name() string { return "Google Drive" }

// Kind implements core.Describer.
func (c *Connector) Kind() string { return ID }

// Detail implements core.Describer: whom to share content with.
func (c *Connector) Detail(ctx context.Context) string {
	if c.email == "" {
		_ = c.Health(ctx)
	}
	if c.email == "" {
		return ""
	}
	return "share files with " + c.email
}

// Health checks credentials and records the service account email.
func (c *Connector) Health(ctx context.Context) error {
	var about *drive.About
	err := c.retry(ctx, func() error {
		var err error
		about, err = c.svc.About.Get().Fields("user(emailAddress)").Context(ctx).Do()
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	if about.User != nil && about.User.EmailAddress != "" {
		c.email = about.User.EmailAddress
	}
	return nil
}

// List returns items shared directly with the service account.
func (c *Connector) List(ctx context.Context, pageToken string, limit int) (core.SearchResult, error) {
	return c.list(ctx, "sharedWithMe = true and trashed = false", pageToken, limit)
}

// Search matches full text or name across everything the account can see.
func (c *Connector) Search(ctx context.Context, q core.SearchQuery) (core.SearchResult, error) {
	t := escapeQuery(q.Text)
	query := fmt.Sprintf("(fullText contains '%s' or name contains '%s') and trashed = false", t, t)
	return c.list(ctx, query, q.PageToken, q.Limit)
}

func (c *Connector) list(ctx context.Context, query, pageToken string, limit int) (core.SearchResult, error) {
	if limit <= 0 {
		limit = core.DefaultLimit
	}
	if limit > core.MaxLimit {
		limit = core.MaxLimit
	}
	var fl *drive.FileList
	err := c.retry(ctx, func() error {
		call := c.svc.Files.List().
			Q(query).
			Corpora("user").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			OrderBy("modifiedTime desc").
			PageSize(int64(limit)).
			Fields(listFields).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		var err error
		fl, err = call.Do()
		return err
	})
	if err != nil {
		return core.SearchResult{}, mapErr(err)
	}
	out := core.SearchResult{Items: make([]core.DocRef, 0, len(fl.Files)), NextPageToken: fl.NextPageToken}
	for _, f := range fl.Files {
		if f.Trashed {
			continue
		}
		out.Items = append(out.Items, toRef(f))
	}
	return out, nil
}

// escapeQuery escapes backslash and single quote for a Drive q string literal.
func escapeQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

func toRef(f *drive.File) core.DocRef {
	r := core.DocRef{Source: ID, ID: f.Id, Title: f.Name, MimeType: f.MimeType, URL: f.WebViewLink}
	if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
		r.Modified = t
	}
	return r
}

// retry runs fn, retrying transient Drive errors on the fixed schedule.
func (c *Connector) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = fn(); err == nil || !retryable(err) {
			return err
		}
		if attempt >= len(c.backoff) {
			return fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
		}
		d := c.backoff[attempt]
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d {
			return fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w: %v", ErrUpstreamUnavailable, ctx.Err())
		case <-t.C:
		}
	}
}

func retryable(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	switch ge.Code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		return hasReason(ge, "userRateLimitExceeded", "rateLimitExceeded")
	}
	return false
}

func hasReason(ge *googleapi.Error, reasons ...string) bool {
	for _, e := range ge.Errors {
		for _, r := range reasons {
			if e.Reason == r {
				return true
			}
		}
	}
	return false
}

// mapErr converts Drive errors to connector sentinels. Not-found never says
// whether the file exists.
func mapErr(err error) error {
	if err == nil || errors.Is(err, ErrUpstreamUnavailable) {
		return err
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch {
		case ge.Code == http.StatusNotFound,
			ge.Code == http.StatusForbidden && hasReason(ge, "notFound", "insufficientFilePermissions", "forbidden"):
			return core.ErrNotFound
		case hasReason(ge, "exportSizeLimitExceeded"):
			return ErrDocumentTooLarge
		}
		return fmt.Errorf("gdrive: drive api error %d", ge.Code)
	}
	return fmt.Errorf("gdrive: %w", err)
}
