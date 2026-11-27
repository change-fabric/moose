package graph_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/graph"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

func approved(t *testing.T) []byte {
	t.Helper()
	raw, err := mailmsg.Build(mailmsg.Message{
		From: "Me <me@example.com>", To: []string{"you@example.com"}, Cc: []string{"cc@example.com"}, Bcc: []string{"bcc@example.com"},
		Subject: "Quarterly plan", Body: "Line one\n\nLine two\n",
	}, time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCreateDraftPostsJSONAndReturnsTheImmutableID(t *testing.T) {
	c, s := newGraph(t)
	d, err := c.CreateDraft(context.Background(), provider.DraftInput{Raw: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	if d.ID == "" || d.MessageID == "" || d.ThreadID == "" {
		t.Fatalf("draft = %+v", d)
	}
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0] != "POST /v1.0/me/messages" {
		t.Fatalf("requests = %v, want one POST /me/messages (the stub rejects a request without the ImmutableId Prefer header)", reqs)
	}
}

func TestDraftSnapshotIsAMailmsgRebuildWithPinnedDateAndNoMessageID(t *testing.T) {
	c, s := newGraph(t)
	d, err := c.CreateDraft(context.Background(), provider.DraftInput{Raw: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.GetDraftRaw(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("message-id:")) {
		t.Errorf("the canonical rebuild must not carry a Message-ID:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("X-MS-")) || bytes.Contains(raw, []byte("Thread-Index")) {
		t.Errorf("Exchange headers must not leak into the snapshot:\n%s", raw)
	}
	// Date is lastModifiedDateTime, so it moves with an edit and with nothing else.
	want, err := mailmsg.Build(mailmsg.Message{
		From: "Me <me@example.com>", To: []string{"you@example.com"}, Cc: []string{"cc@example.com"}, Bcc: []string{"bcc@example.com"},
		Subject: "Quarterly plan", Body: "Line one\n\nLine two\n",
	}, s.msg(d.ID).modifiedAt())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Errorf("snapshot differs from the mailmsg.Build of the draft's fields\nwant %q\n got %q", want, raw)
	}
	p, err := mailmsg.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Recipients(), ","); got != "bcc@example.com,cc@example.com,you@example.com" {
		t.Errorf("recipients = %s", got)
	}
}

func TestUpdateDraftPatchesAndKeepsTheID(t *testing.T) {
	c, s := newGraph(t)
	ctx := context.Background()
	d, err := c.CreateDraft(ctx, provider.DraftInput{Raw: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := c.GetDraftRaw(ctx, d.ID)
	next, err := mailmsg.Build(mailmsg.Message{From: "me@example.com", To: []string{"new@example.com"}, Subject: "Changed", Body: "new"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	up, err := c.UpdateDraft(ctx, d.ID, provider.DraftInput{Raw: next})
	if err != nil || up.ID != d.ID {
		t.Fatalf("update = %+v, %v", up, err)
	}
	after, _ := c.GetDraftRaw(ctx, d.ID)
	if bytes.Equal(before, after) {
		t.Fatal("the PATCH did not change the draft")
	}
	var patched bool
	for _, r := range s.Requests() {
		patched = patched || strings.HasPrefix(r, "PATCH /v1.0/me/messages/")
	}
	if !patched {
		t.Errorf("no PATCH in %v", s.Requests())
	}
	// A PATCH replaces content: the old Cc and Bcc must be gone.
	p, _ := mailmsg.Parse(after)
	if len(p.Cc) != 0 || len(p.Bcc) != 0 || len(p.To) != 1 {
		t.Errorf("update merged instead of replacing: %+v", p)
	}
}

func TestDeleteDraftUsesPermanentDeleteAndNeverTouchesANonDraft(t *testing.T) {
	c, s := newGraph(t)
	ctx := context.Background()
	d, err := c.CreateDraft(ctx, provider.DraftInput{Raw: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDraft(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	var perm bool
	for _, r := range s.Requests() {
		perm = perm || strings.HasSuffix(r, "/permanentDelete")
	}
	if !perm {
		t.Errorf("no permanentDelete in %v", s.Requests())
	}
	inbox := seed(t, s, provider.MailboxInbox, "Received", "hello", time.Now(), true)
	n := len(s.Requests())
	if err := c.DeleteDraft(ctx, inbox.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("DeleteDraft of a received message err = %v, want ErrNotFound", err)
	}
	if _, err := c.UpdateDraft(ctx, inbox.ID, provider.DraftInput{Raw: approved(t)}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("UpdateDraft of a received message err = %v, want ErrNotFound", err)
	}
	if _, err := c.GetDraftRaw(ctx, inbox.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("GetDraftRaw of a received message err = %v, want ErrNotFound", err)
	}
	for _, r := range s.Requests()[n:] {
		if strings.HasPrefix(r, "POST") || strings.HasPrefix(r, "PATCH") {
			t.Errorf("a non-draft id caused a write: %s", r)
		}
	}
	if s.msg(inbox.ID) == nil {
		t.Fatal("the received message was deleted")
	}
}

func TestStageIssueAndLookupSentCorrelateByTheStagingID(t *testing.T) {
	c, s := newGraph(t)
	ctx := context.Background()
	st, err := c.StageDraft(ctx, approved(t))
	if err != nil || st.ID == "" || st.InternetMessageID == "" {
		t.Fatalf("staged = %+v, %v", st, err)
	}
	cp, err := c.LookupSent(ctx, st.ID)
	if err != nil || cp.State != provider.SentPending {
		t.Fatalf("before the send: %+v, %v; a draft still visible is pending, never a failure", cp, err)
	}
	if err := c.IssueSend(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	cp, err = c.LookupSent(ctx, st.ID)
	if err != nil || cp.State != provider.SentFound || cp.ID != st.ID || cp.InternetMessageID != st.InternetMessageID || len(cp.Raw) == 0 {
		t.Fatalf("after the send: %+v, %v", cp, err)
	}
	want, _ := mailmsg.Parse(approved(t))
	got, err := mailmsg.Parse(cp.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if diff := mailmsg.SemanticDiff(want, got); diff != "" {
		t.Errorf("sent copy differs: %s", diff)
	}
	if _, err := c.LookupSent(ctx, "AAMkNoSuch="); err != nil {
		t.Errorf("a missing id is SentMissing, not an error: %v", err)
	} else if cp, _ := c.LookupSent(ctx, "AAMkNoSuch="); cp.State != provider.SentMissing {
		t.Errorf("state = %v, want SentMissing", cp.State)
	}
	// Two identical messages sent back to back correlate independently.
	a, _ := c.StageDraft(ctx, approved(t))
	b, _ := c.StageDraft(ctx, approved(t))
	if a.ID == b.ID {
		t.Fatal("two staging drafts share an id")
	}
	if err := c.IssueSend(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if ca, _ := c.LookupSent(ctx, a.ID); ca.State != provider.SentPending {
		t.Errorf("the unsent twin reads %v, want pending", ca.State)
	}
	if cb, _ := c.LookupSent(ctx, b.ID); cb.State != provider.SentFound || cb.ID != b.ID {
		t.Errorf("the sent twin reads %+v", cb)
	}
	_ = s
}

func TestDeleteStagingNeverDeletesTheSentCopy(t *testing.T) {
	c, s := newGraph(t)
	ctx := context.Background()
	st, _ := c.StageDraft(ctx, approved(t))
	if err := c.IssueSend(ctx, st.ID); err != nil {
		t.Fatal(err)
	}
	n := len(s.Requests())
	if err := c.DeleteStaging(ctx, st.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for a message that has moved to Sent Items", err)
	}
	if s.msg(st.ID) == nil {
		t.Fatal("the Sent Items copy was deleted")
	}
	for _, r := range s.Requests()[n:] {
		if strings.Contains(r, "permanentDelete") {
			t.Errorf("permanentDelete was issued for a sent message: %s", r)
		}
	}
	// A staging draft that was never sent is deleted.
	un, _ := c.StageDraft(ctx, approved(t))
	if err := c.DeleteStaging(ctx, un.ID); err != nil {
		t.Fatal(err)
	}
	if s.msg(un.ID) != nil {
		t.Fatal("the unsent staging draft is still there")
	}
}

// Only the enumerated answers prove a rejection; every other answer to a send
// leaves the outcome unknown.
func TestOnlyEnumeratedAnswersProveASendWasRejected(t *testing.T) {
	cases := []struct {
		status   int
		code     string
		rejected bool
	}{
		{400, "ErrorInvalidRecipients", true},
		{400, "ErrorInvalidRequest", true},
		{403, "ErrorAccessDenied", true},
		{403, "ErrorSendAsDenied", true},
		{403, "ErrorSubmissionQuotaExceeded", true},
		{413, "ErrorMessageSizeExceeded", true},
		{422, "ErrorQuotaExceeded", true},
		{400, "ErrorSomethingElse", false},
		{403, "ErrorItemNotFound", false},
		{404, "ErrorItemNotFound", false},
		{401, "InvalidAuthenticationToken", false},
		{429, "ApplicationThrottled", false},
		{500, "ErrorInternalServerError", false},
		{503, "ErrorServerBusy", false},
		{504, "ErrorTimeout", false},
		{422, "ErrorSomethingElse", false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"code":"` + tc.code + `","message":"x"}}`))
		}))
		g := graphapi.New(nil)
		g.BaseURL = srv.URL
		err := graph.New(g).IssueSend(context.Background(), "AAMkStage=")
		srv.Close()
		if err == nil {
			t.Fatalf("%d %s: no error", tc.status, tc.code)
		}
		if got := errors.Is(err, provider.ErrSendRejected); got != tc.rejected {
			t.Errorf("%d %s: ErrSendRejected = %v, want %v (err %v)", tc.status, tc.code, got, tc.rejected, err)
		}
		if tc.rejected && !errors.Is(err, provider.ErrRejected) {
			t.Errorf("%d %s: a proven rejection must also be ErrRejected", tc.status, tc.code)
		}
	}
}

func TestAHTMLOrAttachmentDraftIsNotSendableAsText(t *testing.T) {
	c, s := newGraph(t)
	ctx := context.Background()
	d, err := c.CreateDraft(ctx, provider.DraftInput{Raw: approved(t)})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.find(d.ID).hasAtt = true
	s.mu.Unlock()
	_, err = c.GetDraftRaw(ctx, d.ID)
	if !mailmsg.IsProblem(err) {
		t.Fatalf("a draft with an attachment err = %v, want a mailmsg.Problem", err)
	}
	s.mu.Lock()
	m := s.find(d.ID)
	m.hasAtt, m.htmlBody = false, true
	s.mu.Unlock()
	if _, err = c.GetDraftRaw(ctx, d.ID); !mailmsg.IsProblem(err) {
		t.Fatalf("a draft whose body Graph returns as HTML err = %v, want a mailmsg.Problem", err)
	}
}
