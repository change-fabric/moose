package pages

import (
	"reflect"
	"testing"

	"github.com/change-fabric/moose/internal/approver/api"
)

func TestLevenshtein(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"abc", "abc", 0}, {"abc", "abd", 1}, {"abc", "ab", 1}, {"kitten", "sitting", 3},
		{"alice@example.com", "alice@exarnple.com", 2}, {"a", "", 1},
	} {
		if got := levenshtein([]rune(tc.a), []rune(tc.b)); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSkeletonEquatesConfusables(t *testing.T) {
	same := [][2]string{
		{"paypal.com", "paypa1.com"},    // 1 for l
		{"paypal.com", "pаypal.com"},    // Cyrillic a
		{"example.com", "exarnple.com"}, // rn for m
		{"wow.com", "vvovv.com"},        // vv for w
		{"Alice@Example.COM", "alice@example.com"},
		{"gоogle.com", "google.com"}, // Cyrillic o
		{"café@x.com", "cafe@x.com"}, // accent
	}
	for _, p := range same {
		if Skeleton(p[0]) != Skeleton(p[1]) {
			t.Errorf("Skeleton(%q) = %q, Skeleton(%q) = %q; want equal", p[0], Skeleton(p[0]), p[1], Skeleton(p[1]))
		}
	}
	if Skeleton("alice@example.com") == Skeleton("bob@example.com") {
		t.Error("different local parts share a skeleton")
	}
}

func TestLookAlikesRules(t *testing.T) {
	self := Account{Nickname: "Work", Address: "alice@example.com", DisplayName: "Alice Doe"}
	sibs := []api.Sibling{
		{Nickname: "within2", Address: "alice@exampel.com", DisplayName: "A"},      // 2 edits
		{Nickname: "three", Address: "alice@exmpl.org", DisplayName: "B"},          // 4+ edits, not confusable
		{Nickname: "same", Address: "alice@example.com", DisplayName: "Alice Doe"}, // the account itself
		{Nickname: "case", Address: "ALICE@example.com ", DisplayName: "Z"},        // the account itself, other case
		{Nickname: "name", Address: "someone@else.example", DisplayName: " alice  DOE "},
		{Nickname: "cyr", Address: "аlice@example.com", DisplayName: "C"},
	}
	got := LookAlikes(self, sibs)
	var names []string
	for _, w := range got {
		names = append(names, w.Nickname)
	}
	if want := []string{"cyr", "within2", "name"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("warnings for %v, want %v (nearest first; the account itself and distant ones skipped)", names, want)
	}
	if !reflect.DeepEqual(got[0].Reasons, []string{"address within 2 edits", "confusable characters"}) {
		t.Errorf("cyr reasons = %v", got[0].Reasons)
	}
	if !reflect.DeepEqual(got[2].Reasons, []string{"same display name"}) {
		t.Errorf("name reasons = %v", got[2].Reasons)
	}
	// An empty display name never matches another empty one.
	if got := LookAlikes(Account{Address: "a@x.example"}, []api.Sibling{{Nickname: "n", Address: "zzzzzzzzzz@y.example"}}); len(got) != 0 {
		t.Errorf("empty display names matched: %v", got)
	}
}

func TestDiffSegmentsMarkTheDifferingCharacters(t *testing.T) {
	segs := func(s ...string) []Segment {
		var out []Segment
		for i := 0; i+1 < len(s); i += 2 {
			out = append(out, Segment{Text: s[i], Diff: s[i+1] == "!"})
		}
		return out
	}
	for _, tc := range []struct {
		a, b         string
		wantA, wantB []Segment
	}{
		{"alice@x.com", "alice@x.com", segs("alice@x.com", "="), segs("alice@x.com", "=")},
		{"alice@example.com", "аlice@example.com", segs("a", "!", "lice@example.com", "="), segs("а", "!", "lice@example.com", "=")},
		{"alice@example.com", "alice@exarnple.com", segs("alice@exa", "=", "m", "!", "ple.com", "="), segs("alice@exa", "=", "rn", "!", "ple.com", "=")},
		{"abc", "abcd", segs("abc", "="), segs("abc", "=", "d", "!")},
	} {
		a, b := diffSegments(tc.a, tc.b)
		if !reflect.DeepEqual(a, tc.wantA) || !reflect.DeepEqual(b, tc.wantB) {
			t.Errorf("diff(%q,%q) = %v / %v, want %v / %v", tc.a, tc.b, a, b, tc.wantA, tc.wantB)
		}
	}
}

func joinDiff(s []Segment) string {
	out := ""
	for _, x := range s {
		if x.Diff {
			out += x.Text
		}
	}
	return out
}

func TestDiffSegmentsSubstitution(t *testing.T) {
	a, b := diffSegments("alice@x.com", "alise@x.com")
	if joinDiff(a) != "c" || joinDiff(b) != "s" {
		t.Fatalf("marked %q / %q, want c / s", joinDiff(a), joinDiff(b))
	}
}
