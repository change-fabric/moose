// Package htmltext turns an HTML mail body into readable plain text.
//
// The output is still untrusted data. Conversion only removes what a reader
// would not see (scripts, styles, hidden elements) and what a model does not
// need (markup), so a hidden-text trick in the HTML does not reach the model
// more easily than it would reach a person.
package htmltext

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MaxInput is the most HTML Convert parses. Longer input is cut first, so a
// huge message cannot cost unbounded memory.
const MaxInput = 1 << 20

// Convert renders h as plain text: block elements become line breaks, list
// items get "- ", links show their target, images show their alt text, and
// runs of blank lines collapse. Input over MaxInput bytes is truncated.
func Convert(h string) string {
	if len(h) > MaxInput {
		h = strings.ToValidUTF8(h[:MaxInput], "")
	}
	doc, err := html.Parse(strings.NewReader(h))
	if err != nil {
		return ""
	}
	var w writer
	w.walk(doc)
	return w.String()
}

type writer struct {
	b        strings.Builder
	pending  bool // a space is owed before the next word
	breaks   int  // line breaks written at the end so far (capped at 2)
	depthPre int
}

func (w *writer) String() string { return strings.TrimSpace(w.b.String()) }

func (w *writer) text(s string) {
	if w.depthPre > 0 {
		w.b.WriteString(s)
		w.breaks = 0
		return
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == 0xa0 {
			w.pending = w.b.Len() > 0 && w.breaks == 0
			continue
		}
		if unicode.IsControl(r) || invisible(r) {
			continue // invisible characters carry no reading value
		}
		if w.pending {
			w.b.WriteByte(' ')
			w.pending = false
		}
		w.b.WriteRune(r)
		w.breaks = 0
	}
}

func (w *writer) brk(n int) {
	if w.b.Len() == 0 {
		return
	}
	for w.breaks < n {
		w.b.WriteByte('\n')
		w.breaks++
	}
	w.pending = false
}

var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Template: true,
	atom.Noscript: true, atom.Svg: true, atom.Iframe: true, atom.Object: true, atom.Embed: true,
}

var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Ul: true, atom.Ol: true, atom.Table: true, atom.Tr: true, atom.Blockquote: true, atom.Pre: true,
	atom.Hr: true, atom.Form: true, atom.Address: true, atom.Dl: true, atom.Figure: true,
}

func (w *writer) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.ElementNode:
		if skipped[n.DataAtom] || hidden(n) {
			return
		}
		switch n.DataAtom {
		case atom.Br:
			w.brk(1)
			return
		case atom.Img:
			if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
				w.text("[image: " + alt + "]")
			}
			return
		case atom.Li, atom.Dt, atom.Dd:
			w.brk(1)
			w.b.WriteString("- ")
			w.breaks, w.pending = 0, false
		case atom.Td, atom.Th:
			w.text(" ")
		case atom.Pre:
			w.brk(2)
			w.depthPre++
			defer func() { w.depthPre-- }()
		}
		if blocks[n.DataAtom] && n.DataAtom != atom.Pre {
			w.brk(2)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c)
		}
		if n.DataAtom == atom.A {
			if href := strings.TrimSpace(attr(n, "href")); href != "" && !strings.HasPrefix(strings.ToLower(href), "javascript:") {
				w.text(" (" + href + ")")
			}
		}
		if blocks[n.DataAtom] {
			w.brk(2)
		}
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// hidden reports an element a reader cannot see: the hidden attribute, or an
// inline style that hides it or shrinks it to nothing.
func hidden(n *html.Node) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, "hidden") {
			return true
		}
	}
	style := strings.ToLower(strings.Join(strings.Fields(attr(n, "style")), ""))
	return hiddenStyle.MatchString(style)
}

var hiddenStyle = regexp.MustCompile(`(^|;)(display:none|visibility:hidden|opacity:0(\.0+)?|max-height:0(px)?|font-size:0(px|pt|em|rem|%)?)(;|!|$)`)

// invisible reports zero-width and byte-order characters: they carry no
// reading value and are a way to hide or split words.
func invisible(r rune) bool {
	switch r {
	case 0x200b, 0x200c, 0x200d, 0x2060, 0xfeff:
		return true
	}
	return false
}
