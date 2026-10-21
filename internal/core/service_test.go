package core_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/connectors/fake"
	"github.com/change-fabric/moose/internal/core"
)

func doc(id, title, content string, mod time.Time) core.Document {
	return core.Document{DocRef: core.DocRef{ID: id, Title: title, Modified: mod}, Content: content, ContentType: core.ContentTypePlain}
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestServiceRedactsContentAndSnippet(t *testing.T) {
	c := fake.New("a", doc("1", "keys", "aws AKIAABCDEFGHIJKLMNOP here", t0))
	s, err := core.NewService([]core.Connector{c}, core.Options{RedactPatterns: []string{`AKIA[0-9A-Z]{16}`}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Get(context.Background(), "a", "1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.Content, "AKIA") || !strings.Contains(d.Content, "[REDACTED]") {
		t.Fatalf("content not redacted: %q", d.Content)
	}
	res, err := s.Search(context.Background(), core.SearchQuery{Text: "keys", Source: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || strings.Contains(res.Items[0].Snippet, "AKIA") {
		t.Fatalf("snippet not redacted: %+v", res.Items)
	}
}

func TestServiceInvalidRedactionPatternNamed(t *testing.T) {
	_, err := core.NewService(nil, core.Options{RedactPatterns: []string{"("}})
	if err == nil || !strings.Contains(err.Error(), `"("`) {
		t.Fatalf("want error naming pattern, got %v", err)
	}
}

func TestServiceFanOutMergesAndSorts(t *testing.T) {
	a := fake.New("a", doc("1", "plan old", "", t0))
	b := fake.New("b", doc("2", "plan new", "", t0.Add(time.Hour)))
	s, err := core.NewService([]core.Connector{a, b}, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), core.SearchQuery{Text: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.Items[0].Source != "b" || res.Items[1].Source != "a" {
		t.Fatalf("unexpected fan-out result: %+v", res.Items)
	}
}

func TestServiceFanOutPartialFailure(t *testing.T) {
	a := fake.New("a", doc("1", "plan", "", t0))
	b := fake.New("b")
	b.SearchErr = errors.New("boom")
	s, _ := core.NewService([]core.Connector{a, b}, core.Options{})
	res, err := s.Search(context.Background(), core.SearchQuery{Text: "plan"})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("want partial result, got %+v %v", res, err)
	}
	a.SearchErr = errors.New("also boom")
	if _, err := s.Search(context.Background(), core.SearchQuery{Text: "plan"}); err == nil {
		t.Fatal("want error when every connector fails")
	}
}

func TestServiceFanOutLimit(t *testing.T) {
	a := fake.New("a", doc("1", "x", "", t0), doc("2", "x", "", t0))
	b := fake.New("b", doc("3", "x", "", t0))
	s, _ := core.NewService([]core.Connector{a, b}, core.Options{})
	res, _ := s.Search(context.Background(), core.SearchQuery{Text: "x", Limit: 2})
	if len(res.Items) != 2 {
		t.Fatalf("want 2, got %d", len(res.Items))
	}
}

func TestServiceCapsLimitAtMax(t *testing.T) {
	var docs []core.Document
	for i := range core.MaxLimit + 50 {
		docs = append(docs, doc(strconv.Itoa(i), "x", "", t0))
	}
	s, _ := core.NewService([]core.Connector{fake.New("a", docs...)}, core.Options{})
	res, _ := s.Search(context.Background(), core.SearchQuery{Text: "x", Limit: 1000})
	if len(res.Items) != core.MaxLimit {
		t.Fatalf("search: want %d, got %d", core.MaxLimit, len(res.Items))
	}
	res, _ = s.List(context.Background(), "a", "", 1000)
	if len(res.Items) != core.MaxLimit {
		t.Fatalf("list: want %d, got %d", core.MaxLimit, len(res.Items))
	}
}

func TestServiceValidationAndErrors(t *testing.T) {
	s, _ := core.NewService([]core.Connector{fake.New("a")}, core.Options{})
	ctx := context.Background()
	if _, err := s.Search(ctx, core.SearchQuery{Text: "  "}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("empty query: %v", err)
	}
	if _, err := s.Search(ctx, core.SearchQuery{Text: strings.Repeat("x", 257)}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("long query: %v", err)
	}
	if _, err := s.Get(ctx, "nope", "1"); !errors.Is(err, core.ErrUnknownSource) {
		t.Fatalf("unknown source: %v", err)
	}
	if _, err := s.Get(ctx, "a", "missing"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing doc: %v", err)
	}
}

func TestServiceGetTruncatesAtMaxBytes(t *testing.T) {
	s, _ := core.NewService([]core.Connector{fake.New("a", doc("1", "t", "0123456789", t0))}, core.Options{MaxBytes: 4})
	d, err := s.Get(context.Background(), "a", "1")
	if err != nil || d.Content != "0123" || !d.Truncated {
		t.Fatalf("got %+v %v", d, err)
	}
}

func TestServiceListSourcesHealth(t *testing.T) {
	a, b := fake.New("a"), fake.New("b")
	b.HealthErr = errors.New("no key")
	s, _ := core.NewService([]core.Connector{b, a}, core.Options{})
	src := s.ListSources(context.Background())
	if len(src) != 2 || src[0].ID != "a" || !src[0].Healthy || src[1].Healthy || !strings.Contains(src[1].Detail, "no key") {
		t.Fatalf("unexpected sources: %+v", src)
	}
}
