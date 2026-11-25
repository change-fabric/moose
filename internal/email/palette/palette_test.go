package palette

import (
	"math"
	"strconv"
	"testing"
)

func lum(hex string) float64 {
	ch := func(s string) float64 {
		v, _ := strconv.ParseUint(s, 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[1:3]) + 0.7152*ch(hex[3:5]) + 0.0722*ch(hex[5:7])
}

func contrast(a, b string) float64 {
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func TestTwelveNamedColoursWithReadableInk(t *testing.T) {
	if Len() != 12 {
		t.Fatalf("palette has %d colours, want 12", Len())
	}
	names, hexes := map[string]bool{}, map[string]bool{}
	for i := range Len() {
		c := At(i)
		if names[c.Name] || hexes[c.Hex] {
			t.Errorf("colour %d (%s %s) duplicates another", i, c.Name, c.Hex)
		}
		names[c.Name], hexes[c.Hex] = true, true
		if r := contrast(c.Hex, c.Ink); r < 4.5 {
			t.Errorf("%s: ink contrast %.2f is below 4.5:1", c.Name, r)
		}
	}
}

func TestPatternOverlayFromThirteenthAccount(t *testing.T) {
	for i := range 12 {
		if Pattern(i) != "none" {
			t.Errorf("account %d pattern = %s, want none", i, Pattern(i))
		}
	}
	if At(12) != At(0) || Pattern(12) == "none" {
		t.Errorf("account 13 = %v/%s, want account 1's colour with an overlay", At(12), Pattern(12))
	}
	if Pattern(24) == Pattern(12) {
		t.Error("third pass reuses the second pass overlay")
	}
}
