// Package textextract turns an untrusted PDF, docx or plain-text file into
// text without letting a hostile file exhaust the host.
//
// Both moose servers use it (Drive for PDFs, email for attachments). The
// limits are the audit result of the original Drive extractor, which held a
// whole PDF in memory, had no page cap, no parser deadline and no bound on
// concurrent parses:
//
//   - the download is streamed to a temporary file, never held in memory, and
//     refused past MaxDownload (25 MiB);
//   - a docx may inflate to at most MaxDecompressed (100 MiB) in total and
//     hold at most MaxEntries (1000) archive entries;
//   - a PDF is read for at most MaxPages (500) pages;
//   - one parse gets Deadline (10 seconds), after which the caller is released;
//   - at most Concurrency (2) parses run at once in this process, because a
//     parse holds a page's worth of decoded objects and Lambda memory is small.
//
// Extracted text is returned to the caller and written nowhere.
package textextract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Limits bounds one extraction. DefaultLimits holds the deployed values.
type Limits struct {
	// MaxDownload is the most bytes a source file may have.
	MaxDownload int64
	// MaxDecompressed is the most bytes a docx may inflate to while being read.
	MaxDecompressed int64
	// MaxEntries is the most entries a docx archive may hold.
	MaxEntries int
	// MaxPages is the most PDF pages read.
	MaxPages int
	// Deadline bounds one parse.
	Deadline time.Duration
	// Concurrency is the most parses running at once.
	Concurrency int
}

// DefaultLimits returns the deployed limits.
func DefaultLimits() Limits {
	return Limits{
		MaxDownload:     25 << 20,
		MaxDecompressed: 100 << 20,
		MaxEntries:      1000,
		MaxPages:        500,
		Deadline:        10 * time.Second,
		Concurrency:     2,
	}
}

// Errors an extraction returns.
var (
	// ErrTooLarge means the source file is over MaxDownload.
	ErrTooLarge = errors.New("textextract: file too large")
	// ErrLimit means a docx broke the entry or decompression cap.
	ErrLimit = errors.New("textextract: archive limit exceeded")
	// ErrMalformed means the file could not be parsed.
	ErrMalformed = errors.New("textextract: malformed file")
	// ErrUnsupported means the file type is not one of PDF, docx or text.
	ErrUnsupported = errors.New("textextract: unsupported file type")
	// ErrDeadline means the parse ran past Deadline or the caller's context.
	ErrDeadline = errors.New("textextract: parse deadline exceeded")
	// ErrBusy means every parse slot stayed taken until the caller's context ended.
	ErrBusy = errors.New("textextract: all extraction slots are busy")
)

// Kind is a supported file type.
type Kind string

const (
	KindPDF  Kind = "pdf"
	KindDocx Kind = "docx"
	KindText Kind = "text"
)

// Hint says what a file claims to be. The extension and MIME type come from
// outside and only choose a parser; a wrong guess fails as ErrMalformed.
type Hint struct {
	MIME     string
	Filename string
}

const docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"

// Detect returns the kind a hint names, or ErrUnsupported.
func Detect(h Hint) (Kind, error) {
	mt, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(h.MIME)), ";")
	mt = strings.TrimSpace(mt)
	switch {
	case mt == "application/pdf":
		return KindPDF, nil
	case mt == docxMIME:
		return KindDocx, nil
	case strings.HasPrefix(mt, "text/"), mt == "application/json":
		return KindText, nil
	}
	// Mail clients often send documents as application/octet-stream.
	switch strings.ToLower(filepath.Ext(h.Filename)) {
	case ".pdf":
		return KindPDF, nil
	case ".docx":
		return KindDocx, nil
	case ".txt", ".md", ".csv", ".json", ".log", ".tsv":
		return KindText, nil
	}
	return "", ErrUnsupported
}

// Result is extracted text.
type Result struct {
	Kind Kind
	Text string
	// Truncated means text was cut at maxText bytes or the page cap.
	Truncated bool
	// Note explains an empty or partial result, for example a PDF with no
	// text layer. It is moose's own wording, never file content.
	Note string
}

// Extractor runs bounded extractions. It is safe for concurrent use. Share one
// per process, because the concurrency bound is per Extractor.
type Extractor struct {
	lim Limits
	sem chan struct{}
	// beforeParse is a test hook that runs inside the parse goroutine.
	beforeParse func(ctx context.Context)
}

// New returns an Extractor with lim. Zero fields take the defaults.
func New(lim Limits) *Extractor {
	d := DefaultLimits()
	if lim.MaxDownload <= 0 {
		lim.MaxDownload = d.MaxDownload
	}
	if lim.MaxDecompressed <= 0 {
		lim.MaxDecompressed = d.MaxDecompressed
	}
	if lim.MaxEntries <= 0 {
		lim.MaxEntries = d.MaxEntries
	}
	if lim.MaxPages <= 0 {
		lim.MaxPages = d.MaxPages
	}
	if lim.Deadline <= 0 {
		lim.Deadline = d.Deadline
	}
	if lim.Concurrency <= 0 {
		lim.Concurrency = d.Concurrency
	}
	return &Extractor{lim: lim, sem: make(chan struct{}, lim.Concurrency)}
}

var (
	defaultOnce sync.Once
	defaultExt  *Extractor
)

// Default returns the process-wide Extractor with the deployed limits.
func Default() *Extractor {
	defaultOnce.Do(func() { defaultExt = New(DefaultLimits()) })
	return defaultExt
}

// Limits returns the limits in force.
func (e *Extractor) Limits() Limits { return e.lim }

// Spooled is a download held in a temporary file, so a parser can seek in it
// without the file being in memory. It counts what is written and refuses to
// take more than the cap.
type Spooled struct {
	f        *os.File
	n        int64
	max      int64
	exceeded bool
	closed   atomic.Bool
}

// NewSpool opens an empty spool capped at MaxDownload. Close it when done: that
// deletes the file.
func (e *Extractor) NewSpool() (*Spooled, error) {
	f, err := os.CreateTemp("", "moose-spool-*")
	if err != nil {
		return nil, fmt.Errorf("textextract: spool: %w", err)
	}
	return &Spooled{f: f, max: e.lim.MaxDownload}, nil
}

// Write appends p, or fails with ErrTooLarge once the cap would be passed.
// Nothing past the cap reaches the disk.
func (s *Spooled) Write(p []byte) (int, error) {
	if s.n+int64(len(p)) > s.max {
		s.exceeded = true
		return 0, ErrTooLarge
	}
	n, err := s.f.Write(p)
	s.n += int64(n)
	return n, err
}

// Spool streams r into a new spool. It fails with ErrTooLarge when r holds
// more than MaxDownload bytes, without reading the rest.
func (e *Extractor) Spool(r io.Reader) (*Spooled, error) {
	s, err := e.NewSpool()
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(s, r); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Size is the number of bytes spooled.
func (s *Spooled) Size() int64 { return s.n }

// Exceeded reports that a write was refused for passing the cap.
func (s *Spooled) Exceeded() bool { return s.exceeded }

// Close removes the file. It is safe to call twice.
func (s *Spooled) Close() error {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := s.f.Close()
	_ = os.Remove(s.f.Name())
	return err
}

type outcome struct {
	res Result
	err error
}

// Extract returns the text of src, at most maxText bytes. It waits for a parse
// slot until ctx ends (ErrBusy), then parses in its own goroutine under
// Deadline. A parse that outlives the deadline is abandoned: the caller gets
// ErrDeadline at once, while the slot stays taken until the parser stops (it
// checks the context between pages and tokens), so the concurrency bound
// counts real work and not only callers. Closing src after ErrDeadline is
// safe: the parser's next read fails.
func (e *Extractor) Extract(ctx context.Context, src *Spooled, h Hint, maxText int64) (Result, error) {
	kind, err := Detect(h)
	if err != nil {
		return Result{}, err
	}
	if src == nil || src.closed.Load() {
		return Result{}, errors.New("textextract: no source")
	}
	if maxText <= 0 {
		maxText = 64 << 10
	}
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ErrBusy
	}
	pctx, cancel := context.WithTimeout(ctx, e.lim.Deadline)
	done := make(chan outcome, 1)
	go func() {
		defer func() { <-e.sem }()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: fmt.Errorf("%w: %v", ErrMalformed, r)}
			}
		}()
		if e.beforeParse != nil {
			e.beforeParse(pctx)
		}
		res, err := e.parse(pctx, kind, src, maxText)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-pctx.Done():
		return Result{}, ErrDeadline
	}
}

func (e *Extractor) parse(ctx context.Context, kind Kind, src *Spooled, maxText int64) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, ErrDeadline
	}
	switch kind {
	case KindPDF:
		return e.pdfText(ctx, src, maxText)
	case KindDocx:
		return e.docxText(ctx, src, maxText)
	default:
		return plainText(src, maxText)
	}
}

func plainText(src *Spooled, maxText int64) (Result, error) {
	b, err := io.ReadAll(io.LimitReader(io.NewSectionReader(src.f, 0, src.n), maxText+1))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	r := Result{Kind: KindText}
	r.Text, r.Truncated = clipText(string(b), maxText)
	return r, nil
}

// clipText cuts s to at most max bytes on a character boundary and reports
// whether it was cut. Invalid UTF-8 is dropped so the text is always valid.
func clipText(s string, max int64) (string, bool) {
	cut := false
	if int64(len(s)) > max {
		s, cut = s[:max], true
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.ToValidUTF8(s, ""), cut
}
