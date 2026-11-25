// Package webui holds the colour tokens shared by every page moose serves
// (the accounts page, the OAuth sign-in page and the approver), so the three
// read as one product.
package webui

// Tokens authors four colours plus --error. Page roles are reassignments or
// color-mixes of those, and dark mode swaps roles rather than adding colours.
// Two literals stay outside on purpose: the sign-in QR code is black on white
// so phones can scan it, and the account swatch patterns overlay translucent
// black. The --*-text, --*-ink and --background names are read by cf:color's
// color_check.rb, which pairs each foreground with the surface it sits on.
const Tokens = `
:root{color-scheme:light dark;
--ink:#1c1b19;--paper:#f8f6f0;--blue:#0b5cad;--blue-soft:#9cc3f2;--error:#a4261b;
--bg:color-mix(in srgb,var(--paper) 96%,var(--ink));
--card:var(--paper);
--fg:var(--ink);
--muted:color-mix(in srgb,var(--ink) 74%,var(--paper));
--line:color-mix(in srgb,var(--ink) 18%,var(--paper));
--field:color-mix(in srgb,var(--ink) 55%,var(--paper));
--accent:var(--blue);--accent-ink:var(--paper);
--ok:var(--blue);--bad:var(--error);
--warn-fg:var(--error);--warn-bg:color-mix(in srgb,var(--error) 9%,var(--paper));
--background:var(--card);
--body-text:var(--fg);--subtle-text:var(--muted);--link-text:var(--accent);--danger-text:var(--bad);--field-edge-ink:var(--field);--warn-bg-text:var(--warn-fg)}
@media (prefers-color-scheme:dark){:root{
--bg:var(--ink);
--card:color-mix(in srgb,var(--ink) 90%,var(--paper));
--fg:var(--paper);
--muted:color-mix(in srgb,var(--paper) 72%,var(--ink));
--line:color-mix(in srgb,var(--paper) 20%,var(--ink));
--field:color-mix(in srgb,var(--paper) 50%,var(--ink));
--accent:var(--blue-soft);--accent-ink:var(--ink);
--ok:var(--blue-soft);--bad:color-mix(in srgb,var(--error) 45%,var(--paper));
--warn-fg:var(--bad);--warn-bg:color-mix(in srgb,var(--error) 22%,var(--ink))}}
`

// Base is the reset, focus ring and primary button every page shares.
const Base = `*{box-sizing:border-box}
button:focus-visible,a:focus-visible,input:focus-visible{outline:3px solid var(--accent);outline-offset:2px}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-ink)}
`
