package textextract

import (
	"context"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

const noTextNote = "no extractable text (scanned or image-only PDF)"

// pdfText walks pages itself instead of calling Reader.GetPlainText, which
// buffers every page before returning. It stops once more than maxText bytes
// are collected or MaxPages pages are read, and checks ctx between pages. The
// parser panics on some malformed files; Extract recovers that as ErrMalformed.
func (e *Extractor) pdfText(ctx context.Context, src *Spooled, maxText int64) (Result, error) {
	r, err := pdf.NewReader(src.f, src.n)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	total := r.NumPage()
	pages := min(total, e.lim.MaxPages)
	res := Result{Kind: KindPDF}
	var sb strings.Builder
	for i := 1; i <= pages && int64(sb.Len()) <= maxText; i++ {
		if ctx.Err() != nil {
			return Result{}, ErrDeadline
		}
		t, err := r.Page(i).GetPlainText(nil)
		if err != nil {
			return Result{}, fmt.Errorf("%w: page %d: %v", ErrMalformed, i, err)
		}
		sb.WriteString(t)
	}
	res.Text, res.Truncated = clipText(sb.String(), maxText)
	if total > e.lim.MaxPages {
		res.Truncated = true
		res.Note = fmt.Sprintf("only the first %d of %d pages were read", e.lim.MaxPages, total)
	}
	if strings.TrimSpace(res.Text) == "" {
		res.Text = ""
		if res.Note == "" {
			res.Note = noTextNote
		}
	}
	return res, nil
}
