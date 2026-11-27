# moose hosted pages design system

## Problem and users

One owner, usually on a phone, decides whether moose may send a specific email, from a specific account, right now. The same owner signs in to the dashboard and connects Gmail accounts from a laptop. Every page is a short decision: read, check the sending account, press one button. The pages are the approver (send, batch send, enable send, contact trust, dashboard sign-in, queue, result pages), the accounts page at /connect/google and the dashboard sign-in at /connect/login.

## Principles

1. Remove before adding: a rule, token or element stays only if a decision on the page needs it. [proposal]
2. One primary action per page, filled in the accent; everything else is an outline button or a link. [evidence: internal/approver/pages/views.go#page]
3. The FROM banner is the loudest thing on a send page, and nothing else borrows its colours. [evidence: internal/approver/pages/views.go#banner]
4. The three surfaces read as one product: one stylesheet base, one set of modules. [evidence: internal/webui/tokens.go#Base]
5. Danger is shown by the error token only, never by a new colour. [evidence: internal/webui/tokens.css#root]

## Tokens

| Role | Token | Light | Dark |
|---|---|---|---|
| Page | `--bg` | paper 96% with ink | ink [evidence: internal/webui/tokens.css#root] |
| Card | `--card` | paper | ink 90% with paper [evidence: internal/webui/tokens.css#root] |
| Text | `--fg` | ink | paper [evidence: internal/webui/tokens.css#root] |
| Secondary text | `--muted` | ink 74% | paper 72% [evidence: internal/webui/tokens.css#root] |
| Hairline | `--line` | ink 18% | paper 20% [evidence: internal/webui/tokens.css#root] |
| Control edge | `--field` | ink 55% | paper 50% [evidence: internal/webui/tokens.css#root] |
| Action and link | `--accent` | blue | blue-soft [evidence: internal/webui/tokens.css#root] |
| Danger | `--bad` | error | error 45% with paper [evidence: internal/webui/tokens.css#root] |

```css
--ink:#1c1b19;--paper:#f8f6f0;--blue:#0b5cad;--blue-soft:#9cc3f2;--error:#a4261b;
--font:system-ui,-apple-system,"Segoe UI",sans-serif;--mono:ui-monospace,Menlo,Consolas,monospace;
--radius:.5rem;--target:2.75rem}
```

Four authored colours plus `--error`. The account palette in internal/email/palette is data, not theme: it colours only the FROM banner and the account swatch. [evidence: internal/email/palette/palette.go#At]
Two literals stay outside the tokens on purpose: the QR code is black on white so phones can scan it, and swatch patterns overlay translucent black. [evidence: internal/webui/tokens.go#Tokens]

## Typography

Two stacks: `--font` for everything, `--mono` for addresses, hashes, codes and message bodies. [proposal]
One ratio, 1.2: .833rem (meta, chips, hashes), 1rem (body, message body), 1.2rem (h2), 1.44rem (h1), 1.728rem (banner nickname at its widest). [proposal]
Line height 1.5 for text and 1.25 for headings and buttons. [proposal]

## Spacing and layout

One unit, .25rem; gaps in use are .5rem (inside a row), 1rem (between modules and card padding) and 1.5rem (page top and section breaks). [proposal]
One column, max 40rem, 1rem side gutter, on every page; nothing changes shape between 320px and desktop except that flex rows wrap and the banner stacks its monogram above the name under 30rem. [proposal]
One radius, `--radius` (.5rem), for cards, buttons, inputs, swatches and the banner; chips are pills. [proposal]

## Themes

Dark mode reassigns roles among the same five colours (ink becomes the page, paper the text, blue-soft the accent) under `prefers-color-scheme`; no literal is added. [evidence: internal/webui/tokens.css#dark]

## Accessibility commitments

WCAG 2.2 AA on every page, checked by `make a11y-approver` (axe-core at 1280px and 320px, plus a no horizontal scroll check) over every hosted page. [evidence: Makefile#a11y-approver]
Targets are at least `--target` (44px), above the 24px minimum. [proposal]
A 3px accent focus ring on every focusable control. [evidence: internal/webui/tokens.go#Base]
Colour is never the only signal: the banner names its colour in text, the swatch has a text label, and look-alike warnings use text and a highlight. [evidence: internal/approver/pages/views.go#banner]

Contrast pairs from `color_check.rb`: [evidence: internal/webui/tokens.css#root]

```
[light] --accent-ink on --accent: 6.17:1 (pass)
[light] --body-text on --background: 15.93:1 (pass)
[light] --subtle-text on --background: 7.01:1 (pass)
[light] --link-text on --background: 6.17:1 (pass)
[light] --danger-text on --background: 6.77:1 (pass)
[light] --field-edge-ink on --background: 3.78:1 (non-text, 3:1 needed)
[light] --warn-bg-text on --warn-bg: 5.84:1 (pass)
[dark] --accent-ink on --accent: 9.43:1 (pass)
[dark] --body-text on --background: 12.04:1 (pass)
[dark] --subtle-text on --background: 6.61:1 (pass)
[dark] --link-text on --background: 7.13:1 (pass)
[dark] --danger-text on --background: 5.37:1 (pass)
[dark] --field-edge-ink on --background: 3.7:1 (non-text, 3:1 needed)
[dark] --warn-bg-text on --warn-bg: 6.31:1 (pass)
```

## Components

Four modules, used at every scale, all in `webui.Base`: page (`main` with an h1), card (`.card`), row (`dl` pairs or `ul.plain` items) and action row (`.actions`). A card holds rows and at most one action row; a page holds cards and one action row. [proposal]
Button: outline by default, `.primary` fills with the accent, `.danger` outlines in `--bad`; the last confirm of a destructive action is filled with `--bad`. Disabled is 55% opacity, used only next to text saying why. [evidence: internal/webui/tokens.go#Base]
Chip: a pill with a 1px current-colour edge; the approver tints flag chips with `--warn-bg`. [evidence: internal/approver/pages/views.go#baseCSS]
Exceptions with a reason: the FROM banner (2px ink edge, palette fill, pattern strip, colour name text, screen-reader label, R7) and the look-alike warning (2px error edge) are louder than a card so they cannot be skimmed past. [evidence: internal/approver/pages/views.go#banner]

## Do and do not

Do put a page's one primary action last, after what it acts on. [proposal]
Do write state as a sentence in a card ("Nothing is waiting for approval."), not an icon. [evidence: internal/approver/pages/views.go#queue]
Do write an error as what happened, then what to do next, with one link or button back. [evidence: internal/email/connect/page.go#errTmpl]
Do not add a colour, font size, radius or gap that is not listed here; snap to the nearest step. [proposal]
Do not restyle a module per page; if a page needs a new look, change the module for all pages. [proposal]

## Open questions

The sign-in page column could be narrower than 40rem around the QR code; kept at 40rem for one column width until a phone test says otherwise.
