// Package attach moves one email attachment from the provider into a private
// object store and reads its text. The bytes stream from the mailbox to the
// store (and into a size-capped temporary file for the parser) without being
// held in memory. The extracted text is returned to the caller and written
// nowhere: not the store, not the database, not a log.
package attach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/textextract"
)

const (
	// Prefix is the key prefix of every attachment object. The sweeper lists
	// and deletes only under it.
	Prefix = "att/"
	// LinkTTL is how long a presigned download link works.
	LinkTTL = 15 * time.Minute
	// MaxText is the most text one result carries.
	MaxText = 64 << 10
	// cleanupTimeout bounds the delete that follows a failed upload.
	cleanupTimeout = 10 * time.Second
)

// Object is one stored attachment.
type Object struct {
	Key      string
	Modified time.Time
}

// Store is a private object store. Put must leave nothing behind when it
// fails: an implementation aborts a partial multipart upload before returning
// the error.
type Store interface {
	// Put streams body to key.
	Put(ctx context.Context, key string, body io.Reader) error
	Delete(ctx context.Context, key string) error
	// PresignGet returns a time-limited download link for key that saves the
	// object as downloadName.
	PresignGet(ctx context.Context, key string, ttl time.Duration, downloadName string) (string, error)
	// List calls fn for every object under prefix. It stops at the first error fn returns.
	List(ctx context.Context, prefix string, fn func(Object) error) error
}

// Extraction statuses a Result reports.
const (
	StatusExtracted   = "extracted"
	StatusNoText      = "no_text"
	StatusUnsupported = "unsupported"
	StatusTooComplex  = "limit_exceeded"
	StatusUnreadable  = "unreadable"
	StatusTimeout     = "timeout"
	StatusBusy        = "busy"
)

// Result is one fetched attachment.
type Result struct {
	Meta provider.AttachmentMeta
	// Text is the extracted text, at most MaxText bytes, unwrapped. The caller
	// wraps it as untrusted before it leaves the server.
	Text      string
	Truncated bool
	Status    string
	// Note is moose's own explanation of an empty or partial result.
	Note    string
	URL     string
	Expires time.Time
}

// ErrTooLarge means the attachment is over the download cap.
var ErrTooLarge = textextract.ErrTooLarge

// Service fetches attachments.
type Service struct {
	Store Store
	// Extract defaults to the process-wide extractor.
	Extract *textextract.Extractor
	Now     func() time.Time
	// NewID names the object; it defaults to a random 128-bit id.
	NewID func() string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) extractor() *textextract.Extractor {
	if s.Extract != nil {
		return s.Extract
	}
	return textextract.Default()
}

func (s *Service) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("attach: random: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Fetch streams the attachment to att/<id> in the store while it spools to a
// temporary file, extracts the text, and presigns a download link. Any failure
// after the upload starts deletes the object, so a failed call leaves nothing.
//
// A file that cannot be read as text (an image, a corrupt PDF, a zip bomb)
// is not an error: the result carries Status and Note and still has its link.
func (s *Service) Fetch(ctx context.Context, p provider.Provider, messageID, attachmentID string) (res Result, err error) {
	ex := s.extractor()
	rc, meta, err := p.OpenAttachment(ctx, messageID, attachmentID)
	if err != nil {
		return Result{}, err
	}
	defer rc.Close()
	if meta.Size > ex.Limits().MaxDownload {
		return Result{}, ErrTooLarge
	}
	sp, err := ex.NewSpool()
	if err != nil {
		return Result{}, err
	}
	defer sp.Close()

	key := Prefix + s.newID()
	if perr := s.Store.Put(ctx, key, io.TeeReader(rc, sp)); perr != nil {
		s.cleanup(key) // the store aborts its own partial upload; this removes anything that still landed
		if sp.Exceeded() || errors.Is(perr, textextract.ErrTooLarge) {
			return Result{}, ErrTooLarge
		}
		return Result{}, fmt.Errorf("attach: upload: %w", perr)
	}
	defer func() {
		if err != nil {
			s.cleanup(key)
		}
	}()

	res = Result{Meta: meta}
	xr, xerr := ex.Extract(ctx, sp, textextract.Hint{MIME: meta.MIMEType, Filename: meta.Filename}, MaxText)
	res.Text, res.Truncated, res.Note = xr.Text, xr.Truncated, xr.Note
	res.Status = statusOf(xr, xerr)

	res.URL, err = s.Store.PresignGet(ctx, key, LinkTTL, downloadName(meta.Filename))
	if err != nil {
		return Result{}, fmt.Errorf("attach: presign: %w", err)
	}
	res.Expires = s.now().Add(LinkTTL).UTC()
	return res, nil
}

// cleanup deletes key even when ctx is already cancelled (the usual reason an
// upload fails is a request that ran out of time).
func (s *Service) cleanup(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = s.Store.Delete(ctx, key)
}

func statusOf(r textextract.Result, err error) string {
	switch {
	case err == nil && strings.TrimSpace(r.Text) != "":
		return StatusExtracted
	case err == nil:
		return StatusNoText
	case errors.Is(err, textextract.ErrUnsupported):
		return StatusUnsupported
	case errors.Is(err, textextract.ErrLimit), errors.Is(err, textextract.ErrTooLarge):
		return StatusTooComplex
	case errors.Is(err, textextract.ErrDeadline):
		return StatusTimeout
	case errors.Is(err, textextract.ErrBusy):
		return StatusBusy
	}
	return StatusUnreadable
}

// downloadName reduces a filename from outside to a safe ASCII name for the
// Content-Disposition header: letters, digits, dot, dash and underscore.
func downloadName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 100 {
			break
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "attachment"
	}
	return out
}
