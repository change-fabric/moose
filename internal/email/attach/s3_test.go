package attach_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/change-fabric/moose/internal/email/attach"
)

// s3Stub is the slice of the S3 API the store uses, enough to tell what was
// created, completed, aborted and deleted.
type s3Stub struct {
	mu       sync.Mutex
	creates  int
	parts    int
	complete int
	aborts   int
	puts     []string
	deletes  []string
	sse      string
	list     string
}

func (s *s3Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	io.Copy(io.Discard, r.Body)
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.creates++
		s.sse = r.Header.Get("x-amz-server-side-encryption")
		fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		s.parts++
		w.Header().Set("ETag", `"etag`+q.Get("partNumber")+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		s.complete++
		fmt.Fprint(w, `<CompleteMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><ETag>"x"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.aborts++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		s.puts = append(s.puts, r.URL.Path)
		s.sse = r.Header.Get("x-amz-server-side-encryption")
		w.Header().Set("ETag", `"e"`)
	case r.Method == http.MethodDelete:
		s.deletes = append(s.deletes, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, s.list)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
	}
}

func newS3(t *testing.T) (*attach.S3, *s3Stub) {
	t.Helper()
	stub := &s3Stub{}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	c := s3.New(s3.Options{
		Region:                     "us-east-1",
		BaseEndpoint:               aws.String(srv.URL),
		UsePathStyle:               true,
		Credentials:                credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return attach.NewS3(c, "bucket", "arn:aws:kms:us-east-1:1:key/k"), stub
}

// failingReader serves n bytes of 'x' and then fails.
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection reset")
	}
	k := min(len(p), f.n)
	for i := 0; i < k; i++ {
		p[i] = 'x'
	}
	f.n -= k
	return k, nil
}

func TestS3PutSmallObjectIsOneEncryptedPut(t *testing.T) {
	st, stub := newS3(t)
	if err := st.Put(context.Background(), "att/abc", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if len(stub.puts) != 1 || stub.puts[0] != "/bucket/att/abc" || stub.sse != "aws:kms" || stub.creates != 0 {
		t.Fatalf("%+v", stub)
	}
}

func TestS3FailedMultipartUploadIsAborted(t *testing.T) {
	st, stub := newS3(t)
	// 6 MiB then a dropped connection: one 5 MiB part goes up, then the stream fails.
	err := st.Put(context.Background(), "att/abc", &failingReader{n: 6 << 20})
	if err == nil {
		t.Fatal("want an error")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.creates != 1 || stub.parts < 1 {
		t.Fatalf("test did not reach a multipart upload: %+v", stub)
	}
	if stub.aborts != 1 {
		t.Fatalf("AbortMultipartUpload calls = %d, want 1", stub.aborts)
	}
	if stub.complete != 0 {
		t.Fatal("a failed upload was completed")
	}
	if stub.sse != "aws:kms" {
		t.Fatalf("multipart upload was not requested with SSE-KMS (%q)", stub.sse)
	}
	// The delete that follows is the belt to the abort's braces.
	if len(stub.deletes) != 1 || stub.deletes[0] != "/bucket/att/abc" {
		t.Fatalf("deletes = %v", stub.deletes)
	}
}

func TestS3PutSurvivesACancelledContextForCleanup(t *testing.T) {
	st, stub := newS3(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.Put(ctx, "att/abc", strings.NewReader("x")); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.deletes) != 1 {
		t.Fatalf("cleanup delete did not run after cancellation: %v", stub.deletes)
	}
}

func TestS3PresignForcesDownloadAndExpiry(t *testing.T) {
	st, _ := newS3(t)
	raw, err := st.PresignGet(context.Background(), "att/abc", attach.LinkTTL, "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/bucket/att/abc" || q.Get("X-Amz-Expires") != "900" || q.Get("X-Amz-Signature") == "" {
		t.Fatalf("url %s", raw)
	}
	if q.Get("response-content-type") != "application/octet-stream" || q.Get("response-content-disposition") != `attachment; filename="report.pdf"` {
		t.Fatalf("download is not forced: %s", raw)
	}
}

func TestS3ListAndDelete(t *testing.T) {
	st, stub := newS3(t)
	stub.list = `<ListBucketResult><IsTruncated>false</IsTruncated>` +
		`<Contents><Key>att/one</Key><LastModified>2026-11-25T10:00:00.000Z</LastModified></Contents>` +
		`<Contents><Key>att/two</Key><LastModified>2026-11-25T11:30:00.000Z</LastModified></Contents></ListBucketResult>`
	var got []attach.Object
	if err := st.List(context.Background(), attach.Prefix, func(o attach.Object) error { got = append(got, o); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "att/one" || !got[1].Modified.Equal(time.Date(2026, 11, 25, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("%+v", got)
	}
	if err := st.Delete(context.Background(), "att/one"); err != nil {
		t.Fatal(err)
	}
	if len(stub.deletes) != 1 {
		t.Fatalf("deletes = %v", stub.deletes)
	}
}
