// Package palette is the curated set of account colours (R7). Accounts are
// assigned a colour_index in connect order and keep it across reconnects. The
// approver's FROM banner and the connect page both read it, so the same account
// looks the same everywhere.
//
// Account 13 onward reuses a colour with a different pattern overlay, so the
// pair (colour, pattern) stays distinct without a hue to rely on. The colour
// vision deficiency checks on this table belong to the approver phase, which
// owns the banner.
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
	{"Blue", "#0b5cad", "#ffffff"},
	{"Vermilion", "#b8340b", "#ffffff"},
	{"Teal", "#00695c", "#ffffff"},
	{"Amber", "#ffb000", "#1a1a1a"},
	{"Plum", "#7b2d8e", "#ffffff"},
	{"Sky", "#7fc8f8", "#1a1a1a"},
	{"Forest", "#2e6b1f", "#ffffff"},
	{"Rose", "#e8a0bf", "#1a1a1a"},
	{"Slate", "#44546a", "#ffffff"},
	{"Gold", "#c9b200", "#1a1a1a"},
	{"Brown", "#6d4326", "#ffffff"},
	{"Mint", "#8ad9b1", "#1a1a1a"},
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
