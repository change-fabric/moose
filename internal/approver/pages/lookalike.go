package pages

import (
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/change-fabric/moose/internal/approver/api"
)

// Segment is a run of an address, marked when it differs from the look-alike.
type Segment struct {
	Text string
	Diff bool
}

// Warning says another connected account could be mistaken for this one.
type Warning struct {
	Nickname string
	Address  string
	// Reasons are "address within 2 edits", "confusable characters" or
	// "same display name".
	Reasons []string
	// Ours and Theirs are the two addresses with the differing characters marked.
	Ours, Theirs []Segment
}

// Account is the account being approved, as far as the check needs.
type Account struct {
	Nickname, Address, DisplayName string
}

// LookAlikes returns a warning for every other account that is within
// Levenshtein distance 2 of the address, shares its confusable skeleton
// (a curated subset of Unicode TR39: Cyrillic and Greek letters that read as
// Latin, plus rn/m, vv/w and cl/d), or has the same display name. Warnings are
// ordered nearest first.
func LookAlikes(self Account, others []api.Sibling) []Warning {
	selfAddr := strings.ToLower(strings.TrimSpace(self.Address))
	type scored struct {
		w    Warning
		dist int
	}
	var found []scored
	for _, o := range others {
		oAddr := strings.ToLower(strings.TrimSpace(o.Address))
		if oAddr == selfAddr {
			continue
		}
		var reasons []string
		d := levenshtein([]rune(selfAddr), []rune(oAddr))
		if d <= 2 {
			reasons = append(reasons, "address within 2 edits")
		}
		if Skeleton(selfAddr) == Skeleton(oAddr) {
			reasons = append(reasons, "confusable characters")
		}
		if dn := normName(self.DisplayName); dn != "" && dn == normName(o.DisplayName) {
			reasons = append(reasons, "same display name")
		}
		if len(reasons) == 0 {
			continue
		}
		ours, theirs := diffSegments(strings.TrimSpace(self.Address), strings.TrimSpace(o.Address))
		found = append(found, scored{Warning{Nickname: o.Nickname, Address: o.Address, Reasons: reasons, Ours: ours, Theirs: theirs}, d})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].dist < found[j].dist })
	out := make([]Warning, len(found))
	for i, f := range found {
		out[i] = f.w
	}
	return out
}

func normName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(s))), " ")
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// diffSegments aligns a with b by edit distance and marks the runes of each
// that have no counterpart in the other (substituted, inserted or deleted).
func diffSegments(a, b string) (aSegs, bSegs []Segment) {
	ra, rb := []rune(a), []rune(b)
	n, m := len(ra), len(rb)
	d := make([][]int, n+1)
	for i := range d {
		d[i] = make([]int, m+1)
		d[i][0] = i
	}
	for j := 1; j <= m; j++ {
		d[0][j] = j
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
		}
	}
	markA, markB := make([]bool, n), make([]bool, m)
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && ra[i-1] == rb[j-1] && d[i][j] == d[i-1][j-1]:
			i, j = i-1, j-1
		case i > 0 && j > 0 && d[i][j] == d[i-1][j-1]+1:
			markA[i-1], markB[j-1] = true, true
			i, j = i-1, j-1
		case i > 0 && d[i][j] == d[i-1][j]+1:
			markA[i-1] = true
			i--
		default:
			markB[j-1] = true
			j--
		}
	}
	return segments(ra, markA), segments(rb, markB)
}

func segments(rs []rune, mark []bool) []Segment {
	var out []Segment
	for i, r := range rs {
		if len(out) > 0 && out[len(out)-1].Diff == mark[i] {
			out[len(out)-1].Text += string(r)
			continue
		}
		out = append(out, Segment{Text: string(r), Diff: mark[i]})
	}
	return out
}

// confusables maps letters of other scripts that read as Latin to their Latin
// lookalike. It is a curated subset of the TR39 confusables data: the
// characters attackers use for address typosquats.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'с': 'c', 'е': 'e', 'о': 'o', 'р': 'p', 'х': 'x', 'у': 'y', 'і': 'i', 'ј': 'j', 'ѕ': 's',
	'ԁ': 'd', 'һ': 'h', 'ԛ': 'q', 'ԝ': 'w', 'к': 'k', 'м': 'm', 'н': 'h', 'т': 't', 'в': 'b', 'ѡ': 'w',
	// Greek
	'α': 'a', 'ο': 'o', 'ρ': 'p', 'ν': 'v', 'ι': 'i', 'κ': 'k', 'τ': 't', 'ε': 'e', 'υ': 'u', 'χ': 'x', 'β': 'b',
	// Latin look-alikes
	'ı': 'i', 'ɡ': 'g', 'ɑ': 'a', 'ⅼ': 'l', 'ℓ': 'l', 'ǀ': 'l', '|': 'l', '1': 'l', '0': 'o',
}

// Skeleton reduces s to a form in which visually confusable strings are equal.
func Skeleton(s string) string {
	var b strings.Builder
	for _, r := range norm.NFKD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if m, ok := confusables[r]; ok {
			r = m
		}
		switch r {
		case 'm':
			b.WriteString("rn")
		case 'w':
			b.WriteString("vv")
		case 'd':
			b.WriteString("cl")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
