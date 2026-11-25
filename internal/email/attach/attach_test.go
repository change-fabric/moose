package attach_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/attach"
	"github.com/change-fabric/moose/internal/email/attach/attachtest"
	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/textextract"
)

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func docx(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	io.WriteString(w, `<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>`+text+`</w:t></w:r></w:p></w:body></w:document>`)
	zw.Close()
	return buf.Bytes()
}

type fixture struct {
	t   *testing.T
	p   *fake.Provider
	st  *attachtest.Mem
	svc *attach.Service
	n   int
}

func newFixture(t *testing.T, lim textextract.Limits) *fixture {
	f := &fixture{t: t, p: fake.New()}
	f.st = attachtest.NewMem(func() time.Time { return t0 })
	f.svc = &attach.Service{
		Store: f.st, Extract: textextract.New(lim), Now: func() time.Time { return t0 },
		NewID: func() string { f.n++; return fmt.Sprintf("id%d", f.n) },
	}
	return f
}

func (f *fixture) add(name, mime string, data []byte) {
	f.p.AddMessage(provider.Message{ID: "m1", From: provider.Address{Email: "a@x.com"}, Date: t0})
	f.p.AddAttachment("m1", provider.AttachmentMeta{ID: "a1", Filename: name, MIMEType: mime}, data)
}

func TestFetchStreamsToStoreThenExtracts(t *testing.T) {
	f := newFixture(t, textextract.Limits{})
	raw := docx(t, "Quarterly numbers: 42")
	f.add("q.docx", "application/octet-stream", raw)

	res, err := f.svc.Fetch(context.Background(), f.p, "m1", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != attach.StatusExtracted || !strings.Contains(res.Text, "Quarterly numbers: 42") {
		t.Fatalf("result %+v", res)
	}
	if keys := f.st.Keys(); len(keys) != 1 || keys[0] != "att/id1" {
		t.Fatalf("keys = %v", keys)
	}
	if got, _ := f.st.Object("att/id1"); !bytes.Equal(got, raw) {
		t.Fatal("the stored object is not the original attachment")
	}
	if !strings.Contains(res.URL, "att/id1") || !strings.Contains(res.URL, "15m0s") {
		t.Fatalf("url %q", res.URL)
	}
	if !res.Expires.Equal(t0.Add(15 * time.Minute)) {
		t.Fatalf("expires %v", res.Expires)
	}
	if f.st.LastDownloadName != "q.docx" {
		t.Fatalf("download name %q", f.st.LastDownloadName)
	}
}

func TestExtractedTextIsNeverStored(t *testing.T) {
	f := newFixture(t, textextract.Limits{})
	const marker = "MARKER-the-extracted-text-must-not-be-stored"
	raw := docx(t, marker)
	if bytes.Contains(raw, []byte(marker)) {
		t.Fatal("test precondition: the marker is visible in the compressed docx")
	}
	f.add("a.docx", "application/octet-stream", raw)
	res, err := f.svc.Fetch(context.Background(), f.p, "m1", "a1")
	if err != nil || !strings.Contains(res.Text, marker) {
		t.Fatalf("%+v, %v", res, err)
	}
	if f.st.Contains(marker) {
		t.Fatal("extracted text reached the store")
	}
	if f.st.Puts() != 1 {
		t.Fatalf("%d writes to the store, want only the original file", f.st.Puts())
	}
}

// brokenReader serves n bytes and then fails, like a connection that drops.
type brokenReader struct {
	n   int
	err error
}

func (b *brokenReader) Read(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, b.err
	}
	k := min(len(p), b.n)
	for i := 0; i < k; i++ {
		p[i] = 'x'
	}
	b.n -= k
	return k, nil
}
func (b *brokenReader) Close() error { return nil }

// wrapped lets a test replace what OpenAttachment returns.
type wrapped struct {
	provider.Provider
	open func() (io.ReadCloser, provider.AttachmentMeta, error)
}

func (w wrapped) OpenAttachment(context.Context, string, string) (io.ReadCloser, provider.AttachmentMeta, error) {
	return w.open()
}

func TestPartialUploadIsCleanedUp(t *testing.T) {
	for name, tc := range map[string]struct {
		prep func(f *fixture) provider.Provider
	}{
		"the source drops mid-stream": {func(f *fixture) provider.Provider {
			return wrapped{f.p, func() (io.ReadCloser, provider.AttachmentMeta, error) {
				return &brokenReader{n: 4096, err: io.ErrUnexpectedEOF}, provider.AttachmentMeta{ID: "a1", Filename: "a.txt", MIMEType: "text/plain", Size: 9000}, nil
			}}
		}},
		"the store fails mid-upload": {func(f *fixture) provider.Provider {
			f.st.PutFailAfter = 100
			return wrapped{f.p, func() (io.ReadCloser, provider.AttachmentMeta, error) {
				return io.NopCloser(strings.NewReader(strings.Repeat("y", 5000))), provider.AttachmentMeta{ID: "a1", Filename: "a.txt", MIMEType: "text/plain", Size: 5000}, nil
			}}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, textextract.Limits{})
			p := tc.prep(f)
			if _, err := f.svc.Fetch(context.Background(), p, "m1", "a1"); err == nil {
				t.Fatal("want an error")
			}
			if keys := f.st.Keys(); len(keys) != 0 {
				t.Fatalf("partial object left behind: %v", keys)
			}
			if d := f.st.Deleted(); len(d) != 1 || d[0] != "att/id1" {
				t.Fatalf("deleted = %v", d)
			}
		})
	}
}

func TestPartialUploadIsCleanedUpEvenWhenTheRequestWasCancelled(t *testing.T) {
	f := newFixture(t, textextract.Limits{})
	f.st.PutFailAfter = 10
	f.add("a.txt", "text/plain", []byte(strings.Repeat("z", 100)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.svc.Fetch(ctx, f.p, "m1", "a1"); err == nil {
		t.Fatal("want an error")
	}
	if len(f.st.Keys()) != 0 {
		t.Fatal("object left behind after a cancelled request")
	}
}

func TestOversizeDownloadStopsMidStreamAndCleansUp(t *testing.T) {
	f := newFixture(t, textextract.Limits{MaxDownload: 1000})
	// The provider claims 10 bytes but streams far more: the spool cap, not the claim, decides.
	p := wrapped{f.p, func() (io.ReadCloser, provider.AttachmentMeta, error) {
		return &brokenReader{n: 1 << 20, err: io.EOF}, provider.AttachmentMeta{ID: "a1", Filename: "a.txt", MIMEType: "text/plain", Size: 10}, nil
	}}
	_, err := f.svc.Fetch(context.Background(), p, "m1", "a1")
	if !errors.Is(err, attach.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if len(f.st.Keys()) != 0 {
		t.Fatal("object left behind")
	}
	// A declared size over the cap is refused before anything is stored.
	big := wrapped{f.p, func() (io.ReadCloser, provider.AttachmentMeta, error) {
		return io.NopCloser(strings.NewReader("x")), provider.AttachmentMeta{ID: "a1", Size: 5000}, nil
	}}
	if _, err := f.svc.Fetch(context.Background(), big, "m1", "a1"); !errors.Is(err, attach.ErrTooLarge) || f.st.Puts() != 1 {
		t.Fatalf("declared oversize: err=%v puts=%d (only the first call may have stored anything)", err, f.st.Puts())
	}
}

func TestPresignFailureDeletesTheObject(t *testing.T) {
	f := newFixture(t, textextract.Limits{})
	f.st.PresignErr = errors.New("no signer")
	f.add("a.txt", "text/plain", []byte("hello"))
	if _, err := f.svc.Fetch(context.Background(), f.p, "m1", "a1"); err == nil {
		t.Fatal("want an error")
	}
	if len(f.st.Keys()) != 0 {
		t.Fatal("object left behind after a presign failure")
	}
}

func TestUnreadableFilesStillGetALink(t *testing.T) {
	f := newFixture(t, textextract.Limits{})
	f.add("pic.png", "image/png", []byte("\x89PNG...."))
	res, err := f.svc.Fetch(context.Background(), f.p, "m1", "a1")
	if err != nil || res.Status != attach.StatusUnsupported || res.Text != "" || res.URL == "" {
		t.Fatalf("%+v, %v", res, err)
	}

	f = newFixture(t, textextract.Limits{})
	f.add("bad.pdf", "application/pdf", []byte("not really a pdf"))
	res, err = f.svc.Fetch(context.Background(), f.p, "m1", "a1")
	if err != nil || res.Status != attach.StatusUnreadable || res.URL == "" {
		t.Fatalf("%+v, %v", res, err)
	}

	f = newFixture(t, textextract.Limits{MaxEntries: 3})
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 10; i++ {
		zw.Create(fmt.Sprintf("f%d", i))
	}
	zw.Close()
	f.add("many.docx", "application/octet-stream", buf.Bytes())
	res, err = f.svc.Fetch(context.Background(), f.p, "m1", "a1")
	if err != nil || res.Status != attach.StatusTooComplex || res.URL == "" {
		t.Fatalf("%+v, %v", res, err)
	}
}

func TestMissingAttachment(t *testing.T) {
	f := newFixture(t, textextract.Limits{})
	f.add("a.txt", "text/plain", []byte("x"))
	if _, err := f.svc.Fetch(context.Background(), f.p, "m1", "nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if f.st.Puts() != 0 {
		t.Fatal("stored something for a missing attachment")
	}
}
