// Package webui holds the tokens and base styles shared by every page moose
// serves (the accounts page, the dashboard sign-in page and the approver), so
// the three read as one product. DESIGN.md beside this file is the rulebook.
package webui

import _ "embed"

// Tokens authors four colours plus --error, two font stacks, one radius and
// one target size. Page roles are reassignments or color-mixes of the
// colours, and dark mode swaps roles rather than adding colours. Two literals
// stay outside on purpose: the sign-in QR code is black on white so phones
// can scan it, and the account swatch patterns overlay translucent black. The
// --*-text, --*-ink and --background names are read by cf:color's
// color_check.rb, which pairs each foreground with the surface it sits on.
//
//go:embed tokens.css
var Tokens string

// Base is every module the pages share: the page column, headings, the card,
// the definition list, the button, the chip and the action row. Sizes come
// from one type ratio (1.2: .833, 1, 1.2, 1.44rem) and one spacing unit
// (.25rem; gaps of .5, 1 and 1.5rem). Pages add only what is theirs.
const Base = `*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:1rem/1.5 var(--font)}
main{max-width:40rem;margin:0 auto;padding:1.5rem 1rem 3rem}
h1{font-size:1.44rem;line-height:1.25;margin:0 0 1rem}
h2{font-size:1.2rem;line-height:1.25;margin:1.5rem 0 .5rem}
a{color:var(--accent)}
.card{background:var(--card);border:1px solid var(--line);border-radius:var(--radius);padding:1rem;margin:1rem 0}
.card>:first-child{margin-top:0}
.card>:last-child{margin-bottom:0}
.note{color:var(--muted)}
.mono{font-family:var(--mono);overflow-wrap:anywhere}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.25rem 1rem;margin:0}
dt{color:var(--muted)}
dd{margin:0;overflow-wrap:anywhere}
.chip{display:inline-block;border:1px solid currentColor;border-radius:1rem;padding:0 .5rem;font-size:.833rem}
.actions{display:flex;flex-wrap:wrap;gap:.5rem;margin:1rem 0}
button,.btn{display:inline-flex;align-items:center;justify-content:center;min-height:var(--target);min-width:var(--target);padding:.5rem 1rem;font:inherit;font-weight:600;line-height:1.25;text-align:center;text-decoration:none;border-radius:var(--radius);border:1px solid var(--field);background:var(--card);color:var(--fg);cursor:pointer}
button.primary,.btn.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-ink)}
button.danger{border-color:var(--bad);color:var(--bad)}
button:disabled{opacity:.55;cursor:not-allowed}
[hidden]{display:none!important}
button:focus-visible,a:focus-visible,input:focus-visible,summary:focus-visible,textarea:focus-visible,pre:focus-visible{outline:3px solid var(--accent);outline-offset:2px}
@media (prefers-reduced-motion:no-preference){button{transition:background-color .15s,color .15s}}
@media (max-width:30rem){dl{grid-template-columns:1fr}dt{margin-top:.5rem}dt:first-child{margin-top:0}}
`
