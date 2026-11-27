package approver_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/palette"
	"github.com/change-fabric/moose/internal/approver/api"
)

func TestBannerShowsTheAccountInItsPaletteColour(t *testing.T) {
	h := newHarness(t)
	last := &api.LastSent{At: t0.Add(-3 * time.Hour), Recipients: 2}
	id := h.create(sendBody(work, sendOpts{Last: last})).RequestID
	body := h.get("/approve/r/" + id).Body

	for _, want := range []string{
		`class="banner pc0"`, // first account, first palette colour
		`class="strip pat-none"`,
		`>Work</p>`, // nickname, large
		`<span class="local">alice</span><wbr><span class="at">@</span><span class="domain">example.com</span>`, // local part and domain separated
		`class="dn">Alice Doe`,                               // display name
		`class="monogram" aria-hidden="true">W`,              // monogram
		`Sending from &middot; ` + palette.At(0).Name,        // colour named in text, not by colour alone
		`Last sent from this account 3h ago to 2 recipients`, // metadata-only context
		`>Send from Work (alice@example.com)</button>`,       // the confirm button names the account
		`role="region" aria-label="Sending account: Work, alice@example.com, colour ` + palette.At(0).Name + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("banner lacks %q", want)
		}
	}
	if strings.Contains(body, "Similar to") {
		t.Error("a look-alike warning appeared with no look-alike account")
	}
	// The banner comes before the message it frames, and the page has one h1.
	if strings.Index(body, `role="region"`) > strings.Index(body, `Hello Bob`) {
		t.Error("the banner is not above the message")
	}
	if strings.Count(body, "<h1>") != 1 {
		t.Errorf("%d h1 elements", strings.Count(body, "<h1>"))
	}
}

func TestEnableBannerNamesTheActionAndShowsTheAddressOnce(t *testing.T) {
	h := newHarness(t)
	bare := acct{"acc_bare", "f3muletown@gmail.com", "f3muletown@gmail.com", "", 1}
	body := h.get("/approve/r/" + h.create(enableBody(bare, "en-bare")).RequestID).Body
	if !strings.Contains(body, `>Enable sending from f3muletown@gmail.com</button>`) {
		t.Error("the enable button does not name the action and the address once")
	}
	if strings.Contains(body, `class="nick"`) {
		t.Error("the nickname fallback repeats the address in the banner")
	}
	if !strings.Contains(body, `class="addr lead"`) {
		t.Error("the address is not the lead line when there is no nickname")
	}
	if !strings.Contains(body, `role="region" aria-label="Sending account: f3muletown@gmail.com`) {
		t.Error("the banner region lost its label")
	}

	named := h.get("/approve/r/" + h.create(enableBody(work, "en-work")).RequestID).Body
	if !strings.Contains(named, `>Enable sending from Work (alice@example.com)</button>`) {
		t.Error("the enable button does not name the nickname and address")
	}
	if !strings.Contains(named, `<p class="nick">Work</p>`) {
		t.Error("a real nickname is not shown")
	}
}

func TestEachPaletteColourAndPatternReachesThePage(t *testing.T) {
	h := newHarness(t)
	for _, idx := range []int{0, 5, 11, 12, 13, 25, 36} {
		a := acct{"acc_" + string(rune('a'+idx%26)), "Acct" + string(rune('A'+idx%26)), "x" + string(rune('a'+idx%26)) + "@example.com", "", idx}
		id := h.create(sendBody(a, sendOpts{Draft: "d"})).RequestID
		body := h.get("/approve/r/" + id).Body
		if want := `class="banner pc` + strconv.Itoa(idx%palette.Len()) + `"`; !strings.Contains(body, want) {
			t.Errorf("color_index %d: page lacks %q", idx, want)
		}
		if want := `class="strip pat-` + palette.Pattern(idx) + `"`; !strings.Contains(body, want) {
			t.Errorf("color_index %d: page lacks %q", idx, want)
		}
		// The stylesheet defines the colour class with the palette's ink.
		c := palette.At(idx)
		if want := `.pc` + strconv.Itoa(idx%palette.Len()) + `{background:` + c.Hex + `;color:` + c.Ink + `}`; !strings.Contains(body, want) {
			t.Errorf("color_index %d: stylesheet lacks %q", idx, want)
		}
	}
	// Account 13 onward is told apart by a pattern as well as the colour.
	if palette.Pattern(13) == "none" {
		t.Fatal("account 14 has no pattern overlay")
	}
}

func TestLookAlikeFixtureShowsTheWarningWithHighlights(t *testing.T) {
	h := newHarness(t)
	siblings := []api.Sibling{
		// Cyrillic a (U+0430) in place of the Latin a.
		{AccountID: "s1", Nickname: "Old work", Address: "аlice@example.com", DisplayName: "Someone Else"},
		// rn for m, two edits away.
		{AccountID: "s2", Nickname: "Typo", Address: "alice@exarnple.com", DisplayName: "Other"},
		// Same display name, nothing like the address.
		{AccountID: "s3", Nickname: "Namesake", Address: "totally.different@elsewhere.example", DisplayName: "alice doe"},
		// Not similar at all.
		{AccountID: "s4", Nickname: "Unrelated", Address: "zed@unrelated.example", DisplayName: "Zed"},
	}
	id := h.create(sendBody(work, sendOpts{Siblings: siblings})).RequestID
	body := h.get("/approve/r/" + id).Body

	if n := strings.Count(body, `class="lookalike" role="alert"`); n != 3 {
		t.Fatalf("%d warnings, want 3 (Old work, Typo, Namesake)", n)
	}
	for _, want := range []string{
		"Similar to Old work (",
		"<mark>а</mark>lice@example.com", // theirs: the Cyrillic letter marked
		"<mark>a</mark>lice@example.com", // ours: the Latin letter marked
		"Similar to Typo (",
		"alice@exa<mark>rn</mark>ple.com", // theirs: rn marked
		"alice@exa<mark>m</mark>ple.com",  // ours: m marked
		"confusable characters",
		"address within 2 edits",
		"Similar to Namesake (",
		"same display name",
		"Check the address before you approve.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("look-alike warnings lack %q", want)
		}
	}
	if strings.Contains(body, "Unrelated") {
		t.Error("an unrelated account triggered a warning")
	}
	// Nearest first: the Cyrillic swap (1 edit) before the rn/m swap (2).
	if strings.Index(body, "Similar to Old work") > strings.Index(body, "Similar to Typo") {
		t.Error("warnings are not ordered nearest first")
	}
	// The warnings sit directly under the banner, before the message.
	if strings.Index(body, "Similar to Old work") > strings.Index(body, "Hello Bob") {
		t.Error("the warnings come after the message")
	}
}

func TestPageRespectsAccessibilityRules(t *testing.T) {
	h := newHarness(t)
	id := h.create(sendBody(work, sendOpts{})).RequestID
	body := h.get("/approve/r/" + id).Body
	for _, want := range []string{
		`<html lang="en">`, `<meta name="viewport"`, `<main id="main"`,
		`--target:2.75rem`, `min-height:var(--target);min-width:var(--target)`, // 44px at the default size, in rem so it scales with zoom
		`@media (prefers-reduced-motion:no-preference)`,
		`:focus-visible`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// No fixed pixel widths that break 200% zoom.
	if regexp.MustCompile(`(?m)[^-]width:\d+px`).MatchString(body) {
		t.Error("the stylesheet fixes a pixel width")
	}
	// Transitions only for people who have not asked for reduced motion.
	for _, m := range regexp.MustCompile(`transition:[^;}]*`).FindAllStringIndex(body, -1) {
		before := body[:m[0]]
		if i := strings.LastIndex(before, "@media"); i < 0 || !strings.Contains(before[i:], "prefers-reduced-motion:no-preference") {
			t.Error("a transition is outside the no-preference media query")
		}
	}
}
