// Package palette is the curated set of account colours (R7). Accounts are
// assigned a colour_index in connect order and keep it across reconnects. The
// approver's FROM banner and the connect page both read it, so the same account
// looks the same everywhere.
//
// Account 13 onward reuses a colour with a different pattern overlay, so the
// pair (colour, pattern) stays distinct without a hue to rely on. The colour
// vision deficiency check on this table (every pair at CIE76 deltaE 20 or more
// under normal vision, protanopia, deutanopia and tritanopia) is cvd_test.go.
package palette

// Color is one palette entry.
type Color struct {
	Name string
	// Hex is the swatch colour. Ink is the text colour with at least 4.5:1
	// contrast against Hex.
	Hex, Ink string
}

// Patterns are the overlays for accounts past the first full pass of colours.
var Patterns = []string{"none", "stripes", "dots", "checks"}

var colors = []Color{
	{"Blue", "#1010f8", "#ffffff"},
	{"Crimson", "#a00000", "#ffffff"},
	{"Yellow", "#f0e800", "#1a1a1a"},
	{"Jade", "#48a078", "#1a1a1a"},
	{"Violet", "#6000a0", "#ffffff"},
	{"Orange", "#e07828", "#1a1a1a"},
	{"Cyan", "#60f8f8", "#1a1a1a"},
	{"Orchid", "#a04890", "#ffffff"},
	{"Navy", "#100840", "#ffffff"},
	{"Lime", "#b8f098", "#1a1a1a"},
	{"Maroon", "#500010", "#ffffff"},
	{"Lavender", "#c088f8", "#1a1a1a"},
}

// Len is the number of distinct colours.
func Len() int { return len(colors) }

// At returns the colour for a colour_index, wrapping after Len().
func At(index int) Color {
	if index < 0 {
		index = -index
	}
	return colors[index%len(colors)]
}

// Pattern returns the overlay for a colour_index: "none" for the first pass,
// then stripes, dots and checks for each later pass.
func Pattern(index int) string {
	if index < 0 {
		index = -index
	}
	return Patterns[(index/len(colors))%len(Patterns)]
}
