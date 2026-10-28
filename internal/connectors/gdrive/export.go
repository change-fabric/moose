package gdrive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ledongthuc/pdf"
	"google.golang.org/api/drive/v3"

	"github.com/change-fabric/moose/internal/core"
)

const (
	firstSheetNote = "first sheet only"
	noPDFTextNote  = "no extractable text (scanned or image-only PDF)"
	// The whole PDF must be in memory to parse its xref table, so the raw
	// download gets its own cap, independent of the returned text cap.
	maxPDFBytes = 32 << 20
)

// Get fetches metadata, then text content by mime type, capped at maxBytes.
func (c *Connector) Get(ctx context.Context, id string, maxBytes int64) (core.Document, error) {
	if maxBytes <= 0 {
		maxBytes = core.DefaultMaxBytes
	}
	var f *drive.File
	err := c.retry(ctx, func() error {
		var err error
		f, err = c.svc.Files.Get(id).SupportsAllDrives(true).Fields(fileFields).Context(ctx).Do()
		return err
	})
	if err != nil {
		return core.Document{}, mapErr(err)
	}
	if f.Trashed {
		return core.Document{}, core.ErrNotFound
	}
	doc := core.Document{DocRef: toRef(f)}

	switch {
	case f.MimeType == "application/vnd.google-apps.document",
		f.MimeType == "application/vnd.google-apps.presentation":
		return c.fill(ctx, doc, core.ContentTypePlain, maxBytes, c.export(ctx, id, "text/plain"))
	case f.MimeType == "application/vnd.google-apps.spreadsheet":
		doc.Snippet = firstSheetNote
		return c.fill(ctx, doc, core.ContentTypeCSV, maxBytes, c.export(ctx, id, "text/csv"))
	case f.MimeType == folderMime:
		return c.folder(ctx, doc, maxBytes)
	case f.MimeType == "application/pdf":
		return c.pdfText(ctx, doc, maxBytes)
	case isText(f.MimeType):
		ct := core.ContentTypePlain
		switch f.MimeType {
		case "text/csv":
			ct = core.ContentTypeCSV
		case "text/markdown":
			ct = core.ContentTypeMarkdown
		}
		return c.fill(ctx, doc, ct, maxBytes, c.download(ctx, id))
	default:
		doc.ContentType = core.ContentTypeMetadataOnly
		return doc, nil
	}
}

func isText(m string) bool {
	return strings.HasPrefix(m, "text/") || m == "application/json"
}

type opener func() (*http.Response, error)

func (c *Connector) export(ctx context.Context, id, mime string) opener {
	return func() (*http.Response, error) {
		return c.svc.Files.Export(id, mime).Context(ctx).Download()
	}
}

func (c *Connector) download(ctx context.Context, id string) opener {
	return func() (*http.Response, error) {
		return c.svc.Files.Get(id).SupportsAllDrives(true).Context(ctx).Download()
	}
}

func (c *Connector) fill(ctx context.Context, doc core.Document, ct string, maxBytes int64, open opener) (core.Document, error) {
	b, truncated, err := c.read(ctx, open, maxBytes)
	if err != nil {
		return core.Document{}, err
	}
	doc.Content = string(b)
	doc.ContentType = ct
	doc.Truncated = truncated
	return doc, nil
}

func (c *Connector) pdfText(ctx context.Context, doc core.Document, maxBytes int64) (core.Document, error) {
	raw, tooBig, err := c.read(ctx, c.download(ctx, doc.ID), maxPDFBytes)
	if err != nil {
		return core.Document{}, err
	}
	doc.ContentType = core.ContentTypeMetadataOnly
	if tooBig {
		doc.Snippet = fmt.Sprintf("PDF larger than %d bytes; text not extracted", maxPDFBytes)
		return doc, nil
	}
	text, err := extractPDF(raw)
	if err != nil || strings.TrimSpace(text) == "" {
		doc.Snippet = noPDFTextNote
		return doc, nil
	}
	if int64(len(text)) > maxBytes {
		text = text[:maxBytes]
		doc.Truncated = true
	}
	doc.Content = text
	doc.ContentType = core.ContentTypePlain
	return doc, nil
}

// extractPDF recovers from panics because the parser panics on some malformed files.
func extractPDF(raw []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parse pdf: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", err
	}
	rd, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(rd)
	return string(b), err
}

// read returns at most limit bytes and whether the body was longer.
func (c *Connector) read(ctx context.Context, open opener, limit int64) ([]byte, bool, error) {
	var resp *http.Response
	err := c.retry(ctx, func() error {
		var err error
		resp, err = open()
		return err
	})
	if err != nil {
		return nil, false, mapErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("%w: read body: %v", ErrUpstreamUnavailable, err)
	}
	if int64(len(b)) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}

func (c *Connector) folder(ctx context.Context, doc core.Document, maxBytes int64) (core.Document, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", escapeQuery(doc.ID))
	res, err := c.list(ctx, q, "", core.MaxLimit)
	if err != nil {
		return core.Document{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Folder: %s\n", doc.Title)
	for _, it := range res.Items {
		fmt.Fprintf(&sb, "- %s\t%s\t%s\n", it.ID, it.MimeType, it.Title)
	}
	if res.NextPageToken != "" {
		sb.WriteString("(more items not shown)\n")
	}
	s := sb.String()
	if int64(len(s)) > maxBytes {
		s = s[:maxBytes]
		doc.Truncated = true
	}
	doc.Content = s
	doc.ContentType = core.ContentTypePlain
	return doc, nil
}
