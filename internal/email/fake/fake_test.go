package fake_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/provider"
)

func seeded() *fake.Provider {
	p := fake.New()
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	p.AddMessage(provider.Message{ID: "a", ThreadID: "t1", Date: day(1), From: provider.Address{Email: "bob@x.com"}, Subject: "Plan", Mailboxes: []string{"INBOX"}, Unread: true})
	p.AddMessage(provider.Message{ID: "b", ThreadID: "t1", Date: day(2), From: provider.Address{Email: "me@x.com"}, To: []provider.Address{{Email: "bob@x.com"}}, Subject: "Re: Plan", Mailboxes: []string{"SENT"}})
	p.AddMessage(provider.Message{ID: "c", Date: day(3), From: provider.Address{Email: "al@x.com"}, Subject: "Invoice", BodyText: "see attached", Mailboxes: []string{"INBOX"}})
	p.AddAttachment("c", provider.AttachmentMeta{ID: "att", Filename: "i.pdf"}, []byte("pdf"))
	return p
}

func ids(r provider.SearchResult) []string {
	var out []string
	for _, m := range r.Messages {
		out = append(out, m.ID)
	}
	return out
}

func TestSearchFilters(t *testing.T) {
	p := seeded()
	ctx := context.Background()
	tests := []struct {
		name string
		q    provider.Query
		want []string
	}{
		{"all newest first", provider.Query{}, []string{"c", "b", "a"}},
		{"from", provider.Query{From: "bob"}, []string{"a"}},
		{"to", provider.Query{To: "bob"}, []string{"b"}},
		{"unread", provider.Query{Unread: true}, []string{"a"}},
		{"mailbox", provider.Query{Mailbox: "SENT"}, []string{"b"}},
		{"has attachment", provider.Query{HasAttachment: true}, []string{"c"}},
		{"text", provider.Query{Text: "attached"}, []string{"c"}},
		{"after", provider.Query{After: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}, []string{"c", "b"}},
		{"before", provider.Query{Before: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}, []string{"b", "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := p.Search(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			got := ids(r)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v want %v", got, tt.want)
				}
			}
		})
	}
}

func TestSearchPaging(t *testing.T) {
	p := seeded()
	ctx := context.Background()
	r1, _ := p.Search(ctx, provider.Query{Limit: 2})
	if len(r1.Messages) != 2 || r1.NextCursor == "" {
		t.Fatalf("page1 %+v", r1)
	}
	r2, _ := p.Search(ctx, provider.Query{Limit: 2, Cursor: r1.NextCursor})
	if len(r2.Messages) != 1 || r2.NextCursor != "" || r2.Messages[0].ID != "a" {
		t.Fatalf("page2 %+v", r2)
	}
	if _, err := p.Search(ctx, provider.Query{Cursor: "zzz"}); !errors.Is(err, provider.ErrInvalidQuery) {
		t.Errorf("bad cursor err = %v", err)
	}
}

func TestThreadMessageAttachment(t *testing.T) {
	p := seeded()
	ctx := context.Background()
	th, err := p.GetThread(ctx, "t1")
	if err != nil || len(th.Messages) != 2 || th.Messages[0].ID != "a" {
		t.Fatalf("%+v %v", th, err)
	}
	if _, err := p.GetMessage(ctx, "nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	rc, meta, err := p.OpenAttachment(ctx, "c", "att")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	if string(b) != "pdf" || meta.Size != 3 {
		t.Errorf("%q %+v", b, meta)
	}
}

func TestDraftLifecycleAndSend(t *testing.T) {
	p := fake.New()
	ctx := context.Background()
	d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: []byte("v1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpdateDraft(ctx, d.ID, provider.DraftInput{Raw: []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	raw, _ := p.GetDraftRaw(ctx, d.ID)
	if string(raw) != "v2" {
		t.Errorf("raw %q", raw)
	}
	if _, err := p.GetDraftRaw(ctx, "x"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if _, err := p.SendDraft(ctx, []byte("v1"), ""); err != nil {
		t.Fatal(err)
	}
	if s := p.SentMessages(); len(s) != 1 || string(s[0].Raw) != "v1" {
		t.Errorf("sent %+v", s)
	}
}

func TestErrInjected(t *testing.T) {
	p := fake.New()
	p.Err = provider.ErrAuth
	if _, err := p.ListMailboxes(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("err = %v", err)
	}
}
