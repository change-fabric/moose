package textextract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const docxBody = "word/document.xml"

// docxText reads the text of word/document.xml. The archive is checked before
// it is opened: the entry count comes from the end-of-central-directory
// record, so a directory of a million entries is refused before the zip reader
// allocates for it. Inflation is counted as bytes are read, never trusted from
// the declared sizes, and stops with ErrLimit at MaxDecompressed.
func (e *Extractor) docxText(ctx context.Context, src *Spooled, maxText int64) (Result, error) {
	if err := e.checkEntryCount(src); err != nil {
		return Result{}, err
	}
	zr, err := zip.NewReader(src.f, src.n)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if len(zr.File) > e.lim.MaxEntries {
		return Result{}, fmt.Errorf("%w: %d entries", ErrLimit, len(zr.File))
	}
	var declared uint64
	var body *zip.File
	for _, f := range zr.File {
		declared += f.UncompressedSize64
		if f.Name == docxBody {
			body = f
		}
	}
	if declared > uint64(e.lim.MaxDecompressed) {
		return Result{}, fmt.Errorf("%w: archive declares %d bytes", ErrLimit, declared)
	}
	if body == nil {
		return Result{}, fmt.Errorf("%w: no %s", ErrMalformed, docxBody)
	}
	rc, err := body.Open()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	defer rc.Close()
	return parseDocxXML(ctx, &capReader{r: rc, left: e.lim.MaxDecompressed}, maxText)
}

// checkEntryCount reads the zip's end-of-central-directory record. A zip64
// archive (count field 0xFFFF) is refused: no real document needs 65535 entries.
func (e *Extractor) checkEntryCount(src *Spooled) error {
	const tail = 22 + 65535
	start := max(src.n-tail, 0)
	buf := make([]byte, src.n-start)
	if _, err := src.f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	i := bytes.LastIndex(buf, []byte{0x50, 0x4b, 0x05, 0x06})
	if i < 0 || len(buf)-i < 22 {
		return fmt.Errorf("%w: not a zip archive", ErrMalformed)
	}
	n := int(binary.LittleEndian.Uint16(buf[i+10:]))
	if n == 0xFFFF || n > e.lim.MaxEntries {
		return fmt.Errorf("%w: archive holds %d or more entries", ErrLimit, n)
	}
	return nil
}

// capReader fails with ErrLimit once more than left bytes would be read.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, ErrLimit
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func parseDocxXML(ctx context.Context, r io.Reader, maxText int64) (Result, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var sb strings.Builder
	inText := false
	for n := 0; ; n++ {
		if n%256 == 0 && ctx.Err() != nil {
			return Result{}, ErrDeadline
		}
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, ErrLimit) {
				return Result{}, fmt.Errorf("%w: document.xml inflates past the cap", ErrLimit)
			}
			return Result{}, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				sb.WriteByte('\t')
			case "br", "cr":
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
		if int64(sb.Len()) > maxText {
			break
		}
	}
	res := Result{Kind: KindDocx}
	res.Text, res.Truncated = clipText(sb.String(), maxText)
	if strings.TrimSpace(res.Text) == "" {
		res.Text, res.Note = "", "the document has no text"
	}
	return res, nil
}
