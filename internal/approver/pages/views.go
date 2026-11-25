package pages

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/email/palette"
)

// css is the page stylesheet. It is inlined with a per-response nonce, so the
// content security policy needs no 'unsafe-inline'. The palette classes are
// generated from the palette so the banner and the connect page agree.
func css() string {
	var b strings.Builder
	b.WriteString(baseCSS)
	for i := range palette.Len() {
		c := palette.At(i)
		fmt.Fprintf(&b, ".pc%d{background:%s;color:%s}\n", i, c.Hex, c.Ink)
	}
	return b.String()
}

const baseCSS = `
:root{color-scheme:light;--bg:#f6f5f1;--fg:#1c1b19;--muted:#55524a;--card:#fff;--line:#c9c5b8;--accent:#0b4f99;--warn-bg:#fff3d6;--warn-fg:#5c3b00;--bad:#9b1c12;--ok:#17613a}
*{box-sizing:border-box}
html{font-size:100%}
body{margin:0;background:var(--bg);color:var(--fg);font:1rem/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:46rem;margin:0 auto;padding:1rem}
h1{font-size:1.4rem;margin:.5rem 0 1rem}
h2{font-size:1.15rem;margin:1.25rem 0 .5rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:.5rem;padding:1rem;margin:1rem 0}
.banner{border-radius:.5rem;overflow:hidden;margin:1rem 0;border:2px solid var(--fg)}
.banner .strip{height:.75rem;background-color:var(--fg);color:#fff}
.banner .strip.pat-stripes{background-image:repeating-linear-gradient(45deg,#fff 0 .4rem,transparent .4rem .8rem)}
.banner .strip.pat-dots{background-image:radial-gradient(#fff 28%,transparent 30%);background-size:.75rem .75rem}
.banner .strip.pat-checks{background-image:conic-gradient(#fff 25%,transparent 0 50%,#fff 0 75%,transparent 0);background-size:1.5rem 1.5rem}
.banner .strip.pat-none{display:none}
.banner-body{display:flex;gap:1rem;align-items:center;padding:1rem}
.monogram{flex:none;width:3.5rem;height:3.5rem;border-radius:50%;border:3px solid currentColor;display:flex;align-items:center;justify-content:center;font-size:1.75rem;font-weight:700}
.eyebrow{margin:0;font-size:.875rem;text-transform:uppercase;letter-spacing:.06em}
.nick{margin:0;font-size:2rem;line-height:1.15;overflow-wrap:anywhere}
.addr{margin:.25rem 0 0;font-family:ui-monospace,Menlo,Consolas,monospace;font-size:1.1rem;overflow-wrap:anywhere}
.addr .at{padding:0 .25rem;opacity:.85}
.addr .domain{font-weight:700}
.dn,.ctx{margin:.25rem 0 0}
.lookalike{overflow-wrap:anywhere;background:var(--warn-bg);color:var(--warn-fg);border:2px solid var(--warn-fg);border-radius:.5rem;padding:.75rem 1rem;margin:1rem 0}
.lookalike p{margin:.25rem 0}
.lookalike .mono{font-family:ui-monospace,Menlo,Consolas,monospace;overflow-wrap:anywhere}
mark{background:var(--warn-fg);color:#fff;padding:0 .1rem;border-radius:.15rem}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.25rem 1rem;margin:0}
dt{color:var(--muted)}
dd{margin:0;overflow-wrap:anywhere}
pre.body{white-space:pre-wrap;overflow-wrap:anywhere;background:#f3f1ea;border:1px solid var(--line);border-radius:.375rem;padding:.75rem;margin:.5rem 0;font:.95rem/1.45 ui-monospace,Menlo,Consolas,monospace;max-height:28rem;overflow:auto}
.chip{display:inline-block;border:1px solid var(--warn-fg);color:var(--warn-fg);background:var(--warn-bg);border-radius:1rem;padding:0 .6rem;font-size:.875rem;margin:.1rem .25rem .1rem 0}
.sha{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:.85rem;overflow-wrap:anywhere}
.actions{display:flex;flex-wrap:wrap;gap:.75rem;margin:1rem 0}
button,.btn{min-height:2.75rem;min-width:2.75rem;padding:.5rem 1.1rem;font:inherit;font-weight:600;border-radius:.4rem;border:2px solid var(--fg);background:#fff;color:var(--fg);cursor:pointer;text-decoration:none;display:inline-flex;align-items:center}
button.primary{background:var(--fg);color:#fff}
button.danger{border-color:var(--bad);color:var(--bad)}
button:focus-visible,a:focus-visible,input:focus-visible{outline:3px solid var(--accent);outline-offset:2px}
button[hidden],[hidden]{display:none!important}
button:disabled{opacity:.55;cursor:not-allowed}
.note{color:var(--muted)}
.error{color:var(--bad);font-weight:600}
.status{min-height:1.5rem}
label.item{display:flex;gap:.75rem;align-items:flex-start;min-height:2.75rem;padding:.25rem 0}
label.item input{width:1.5rem;height:1.5rem;flex:none;margin-top:.1rem}
.group h3{margin:0 0 .5rem;font-size:1.05rem}
.swatch{display:inline-block;width:1.25rem;height:1.25rem;border-radius:.25rem;vertical-align:middle;margin-right:.4rem;border:1px solid var(--fg)}
ul.plain{list-style:none;margin:0;padding:0}
ul.plain li{padding:.4rem 0;border-top:1px solid var(--line)}
a{color:var(--accent)}
@media (prefers-reduced-motion:no-preference){button{transition:background-color .15s,color .15s}}
@media (max-width:30rem){.banner-body{flex-direction:column;align-items:flex-start}dl{grid-template-columns:1fr}dt{margin-top:.5rem}}
`

// bannerView is the FROM banner of one account.
type bannerView struct {
	ColorIdx     int
	ColorName    string
	PatternClass string
	Nickname     string
	Local        string
	Domain       string
	DisplayName  string
	Monogram     string
	Region       string
	LastSent     string
	Warnings     []Warning
	ConfirmLabel string
}

func newBanner(pv api.Preview, address string, now time.Time) bannerView {
	col := palette.At(pv.ColorIndex)
	local, domain := address, ""
	if i := strings.LastIndex(address, "@"); i >= 0 {
		local, domain = address[:i], address[i+1:]
	}
	nick := pv.Nickname
	mono := "?"
	if r := []rune(strings.TrimSpace(nick)); len(r) > 0 {
		mono = strings.ToUpper(string(r[0]))
	}
	v := bannerView{
		ColorIdx: indexOf(pv.ColorIndex), ColorName: col.Name, PatternClass: "pat-" + palette.Pattern(pv.ColorIndex),
		Nickname: nick, Local: local, Domain: domain, DisplayName: pv.DisplayName, Monogram: mono,
		Region:       fmt.Sprintf("Sending account: %s, %s, colour %s", nick, address, col.Name),
		Warnings:     LookAlikes(Account{Nickname: nick, Address: address, DisplayName: pv.DisplayName}, pv.Siblings),
		ConfirmLabel: fmt.Sprintf("Send from %s (%s)", nick, address),
	}
	if pv.LastSent != nil && !pv.LastSent.At.IsZero() {
		who := "recipients"
		if pv.LastSent.Recipients == 1 {
			who = "recipient"
		}
		v.LastSent = fmt.Sprintf("Last sent from this account %s ago to %d %s", since(now, pv.LastSent.At), pv.LastSent.Recipients, who)
	}
	return v
}

func indexOf(i int) int {
	if i < 0 {
		i = -i
	}
	return i % palette.Len()
}

func since(now, then time.Time) string {
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// pageView is everything a page template reads.
type pageView struct {
	Title   string
	Nonce   string
	CSS     template.CSS
	Kind    string
	Heading string
	Banner  *bannerView
	// Needs the FROM tap before approving.
	NeedsFrom bool
	Confirmed bool
	Pending   bool
	Message   string // state message for non-pending requests
	Body      string // which body template: send, enable, trust, dashboard, batch, state
	Send      *sendView
	Trust     []api.TrustChange
	Dashboard *api.DashboardSessionPayload
	Items     []batchItemView
	Account   string
	Expires   string
	Notice    string
	Groups    []queueGroup
}

// queueGroup is one account's pending requests on the queue page.
type queueGroup struct {
	N          int
	Title      string
	Address    string
	ColorName  string
	Swatch     int
	HasAccount bool
	Rows       []queueRow
}

type queueRow struct{ Label, Detail, URL, Age string }

type sendView struct {
	From, Subject, Body, SHA string
	To, Cc, Bcc              []string
	Flags                    []string
	Trust                    []api.TrustChange
	RequestID                string
}

type batchItemView struct {
	ID   string
	Send *sendView
}

var funcs = template.FuncMap{"join": strings.Join}

var pageTmpl = template.Must(template.New("page").Funcs(funcs).Parse(`{{define "banner"}}
<div class="banner pc{{.ColorIdx}}" role="region" aria-label="{{.Region}}">
  <div class="strip {{.PatternClass}}" aria-hidden="true"></div>
  <div class="banner-body">
    <div class="monogram" aria-hidden="true">{{.Monogram}}</div>
    <div>
      <p class="eyebrow">Sending from &middot; {{.ColorName}}</p>
      <p class="nick">{{.Nickname}}</p>
      <p class="addr"><span class="local">{{.Local}}</span><span class="at">@</span><span class="domain">{{.Domain}}</span></p>
      {{if .DisplayName}}<p class="dn">{{.DisplayName}}</p>{{end}}
      {{if .LastSent}}<p class="ctx">{{.LastSent}}</p>{{end}}
    </div>
  </div>
</div>
{{range .Warnings}}
<div class="lookalike" role="alert">
  <p><strong>Similar to {{.Nickname}} ({{range .Theirs}}{{if .Diff}}<mark>{{.Text}}</mark>{{else}}{{.Text}}{{end}}{{end}})</strong></p>
  <p>This account: <span class="mono">{{range .Ours}}{{if .Diff}}<mark>{{.Text}}</mark>{{else}}{{.Text}}{{end}}{{end}}</span></p>
  <p>{{join .Reasons ", "}}. Check the address before you approve.</p>
</div>
{{end}}
{{end}}
{{define "send"}}
<dl>
  <dt>From</dt><dd>{{.From}}</dd>
  <dt>To</dt><dd>{{join .To ", "}}</dd>
  {{if .Cc}}<dt>Cc</dt><dd>{{join .Cc ", "}}</dd>{{end}}
  {{if .Bcc}}<dt>Bcc</dt><dd>{{join .Bcc ", "}}</dd>{{end}}
  <dt>Subject</dt><dd>{{.Subject}}</dd>
  {{if .Flags}}<dt>Flags</dt><dd>{{range .Flags}}<span class="chip">{{.}}</span>{{end}}</dd>{{end}}
  {{if .Trust}}<dt>Will be trusted</dt><dd>{{range .Trust}}<span class="chip">{{.Address}}</span>{{end}}</dd>{{end}}
  <dt>RFC822 sha256</dt><dd class="sha">{{.SHA}}</dd>
</dl>
<pre class="body" tabindex="0" aria-label="Message body, plain text">{{.Body}}</pre>
{{end}}
{{define "page"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style nonce="{{.Nonce}}">{{.CSS}}</style>
</head>
<body>
<main id="main" data-kind="{{.Kind}}" data-needs-from="{{.NeedsFrom}}" data-confirmed="{{.Confirmed}}">
<h1>{{.Heading}}</h1>
{{if .Notice}}<p class="note">{{.Notice}}</p>{{end}}
{{if .Banner}}{{template "banner" .Banner}}{{end}}
{{if .Message}}<div class="card"><p>{{.Message}}</p></div>{{end}}
{{if eq .Body "send"}}<div class="card">{{template "send" .Send}}</div>{{end}}
{{if eq .Body "enable"}}<div class="card"><p>Approving lets moose send email from the account above, after a separate approval for each message. You can turn sending off again at any time without a passkey.</p></div>{{end}}
{{if eq .Body "trust"}}<div class="card"><h2>Addresses to trust{{if .Account}} for {{.Account}}{{end}}</h2><ul class="plain">{{range .Trust}}<li>{{.Address}}</li>{{end}}</ul></div>{{end}}
{{if eq .Body "dashboard"}}<div class="card"><p>An application is asking to sign in to your moose dashboard.</p><dl>
<dt>Application</dt><dd>{{.Dashboard.ClientName}}</dd>
<dt>Client ID</dt><dd class="sha">{{.Dashboard.ClientID}}</dd>
<dt>Redirects to</dt><dd class="sha">{{.Dashboard.RedirectURIHost}}</dd></dl>
<p class="note">Approve only if you just started this sign-in.</p></div>{{end}}
{{if eq .Body "batch"}}<div class="card"><h2>Messages in this batch</h2><p class="note">Untick a message to leave it out. Each message is sent on its own, within the account's hourly limit.</p>
{{range .Items}}<div class="group"><label class="item"><input type="checkbox" name="drop-item" value="{{.ID}}" checked aria-label="Include this message"><span>Include this message</span></label>{{template "send" .Send}}</div><hr>{{end}}</div>{{end}}
{{if eq .Body "register"}}<div class="card"><p>Create the passkey that approves requests for {{.Account}}. This link works once and expires soon.</p><div class="actions"><button id="register" type="button" class="primary">Create passkey</button></div><p id="status" class="status" role="status" aria-live="polite"></p></div>{{end}}
{{if eq .Body "login"}}<div class="card"><p>Sign in with your passkey to see pending approvals.</p><div class="actions"><button id="login" type="button" class="primary">Sign in with passkey</button></div><p id="status" class="status" role="status" aria-live="polite"></p></div>{{end}}
{{if eq .Body "queue"}}{{range .Groups}}<section class="card group" aria-labelledby="g{{.N}}"><h2 id="g{{.N}}">{{if .HasAccount}}<span class="swatch pc{{.Swatch}}" aria-hidden="true"></span>{{end}}{{.Title}}{{if .Address}} <span class="note">{{.Address}} &middot; {{.ColorName}}</span>{{end}}</h2><ul class="plain">{{range .Rows}}<li><a href="{{.URL}}">{{.Label}}</a> <span class="note">{{.Detail}} &middot; {{.Age}}</span></li>{{end}}</ul></section>{{else}}<div class="card"><p>Nothing is waiting for approval.</p></div>{{end}}{{end}}
{{if .Pending}}
<p class="note">This request expires at {{.Expires}}.</p>
<p id="status" class="status" role="status" aria-live="polite"></p>
{{if .NeedsFrom}}<div class="actions"><button id="confirm-from" type="button" class="primary"{{if .Confirmed}} hidden{{end}}>{{.Banner.ConfirmLabel}}</button></div>{{end}}
<div class="actions" id="decide"{{if .NeedsFrom}}{{if not .Confirmed}} hidden{{end}}{{end}}>
  <button id="approve" type="button" class="primary">Approve with passkey</button>
  <button id="deny" type="button" class="danger">Deny</button>
</div>
{{end}}
</main>
<script nonce="{{.Nonce}}">{{.JS}}</script>
</body>
</html>{{end}}`))

// The page template reads .JS, so the render wrapper adds it.
type pageData struct {
	pageView
	JS template.JS
}

func render(v pageView, js string) ([]byte, error) {
	var buf bytes.Buffer
	if err := pageTmpl.ExecuteTemplate(&buf, "page", pageData{v, template.JS(js)}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
