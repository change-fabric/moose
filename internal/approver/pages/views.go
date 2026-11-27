package pages

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/palette"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/webui"
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

// baseCSS is the shared webui modules plus what only the approver has: the
// FROM banner (R7), the look-alike warning and the message body.
var baseCSS = webui.Tokens + webui.Base + `
.banner{border-radius:var(--radius);overflow:hidden;margin:1rem 0;border:2px solid var(--fg)}
.banner .strip{height:.75rem;background-color:var(--fg);color:var(--card)}
.banner .strip.pat-stripes{background-image:repeating-linear-gradient(45deg,var(--card) 0 .4rem,transparent .4rem .8rem)}
.banner .strip.pat-dots{background-image:radial-gradient(var(--card) 28%,transparent 30%);background-size:.75rem .75rem}
.banner .strip.pat-checks{background-image:conic-gradient(var(--card) 25%,transparent 0 50%,var(--card) 0 75%,transparent 0);background-size:1.5rem 1.5rem}
.banner .strip.pat-none{display:none}
.banner-body{display:flex;gap:1rem;align-items:center;padding:1rem}
.monogram{flex:none;width:3.5rem;height:3.5rem;border-radius:50%;border:3px solid currentColor;display:flex;align-items:center;justify-content:center;font-size:1.728rem;font-weight:700}
.eyebrow{margin:0;font-size:.833rem;text-transform:uppercase;letter-spacing:.06em}
.nick{margin:0;font-size:clamp(1.44rem,7vw,1.728rem);line-height:1.25;overflow-wrap:break-word}
.addr{margin:.25rem 0 0;font-family:var(--mono);font-size:1rem;overflow-wrap:anywhere}
.addr .at{padding:0 .25rem;opacity:.85}
.addr .domain{font-weight:700}
.addr.lead{font-size:clamp(1.2rem,5.5vw,1.44rem)}
.dn,.ctx{margin:.25rem 0 0}
.lookalike{overflow-wrap:anywhere;background:var(--warn-bg);color:var(--warn-fg);border:2px solid var(--warn-fg);border-radius:var(--radius);padding:1rem;margin:1rem 0}
.lookalike p{margin:.25rem 0}
mark{background:var(--warn-fg);color:var(--card);padding:0 .1rem;border-radius:.25rem}
pre.body{white-space:pre-wrap;overflow-wrap:anywhere;background:var(--bg);border:1px solid var(--line);border-radius:var(--radius);padding:1rem;margin:1rem 0 0;font:1rem/1.5 var(--mono);max-height:28rem;overflow:auto}
.chip{color:var(--warn-fg);background:var(--warn-bg);margin:0 .25rem .25rem 0}
.sha{font-family:var(--mono);font-size:.833rem;overflow-wrap:anywhere}
.actions.confirm button{width:100%;min-height:3.5rem;font-size:1.2rem}
.error{color:var(--bad);font-weight:600}
.status{min-height:1.5rem}
label.item{display:flex;gap:.5rem;align-items:center;min-height:var(--target)}
label.item input{width:1.5rem;height:1.5rem;flex:none;margin:0}
.swatch{display:inline-block;width:1.25rem;height:1.25rem;border-radius:.25rem;vertical-align:-.2rem;margin-right:.5rem;border:1px solid var(--fg)}
.group h2{margin:0 0 .5rem}
.group h2+.note{margin:-.25rem 0 .5rem}
ul.plain{list-style:none;margin:0;padding:0}
ul.plain li{padding:.5rem 0;border-top:1px solid var(--line)}
ul.plain li .note{display:block}
.batch-item+.batch-item{border-top:1px solid var(--line);margin-top:1rem;padding-top:1rem}
.untrusted{border:1px dashed var(--fg);border-radius:var(--radius);padding:.5rem .75rem;margin:.25rem 0;overflow-wrap:anywhere;white-space:pre-wrap}
.untrusted-note{margin:0 0 .5rem}
.changed-row{border-left:.35rem solid var(--warn-fg);padding-left:.75rem}
.changed-row dt::after{content:" (changed)";color:var(--warn-fg);font-weight:700}
.was{margin:.25rem 0 0;color:var(--muted,var(--fg));text-decoration:line-through;overflow-wrap:anywhere;white-space:pre-wrap}
.recipient{padding:.5rem 0;border-top:1px solid var(--line);overflow-wrap:anywhere}
.recipient .note{display:block}
@media (max-width:30rem){.banner-body{flex-direction:column;align-items:flex-start}}
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
	// ShowNick is false when the nickname is only the address fallback, so the
	// address is shown once.
	ShowNick     bool
	Warnings     []Warning
	ConfirmLabel string
	// Eyebrow is the line above the account: what the account is doing.
	Eyebrow string
}

// newBanner builds the FROM banner. The confirm label names the account and
// the action the kind asks for.
func newBanner(pv api.Preview, address, kind string, now time.Time) bannerView {
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
		ShowNick:     !sameAddress(nick, address),
		ConfirmLabel: confirmLabel(kind, nick, address),
		Eyebrow:      eyebrow(kind),
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

func sameAddress(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func confirmLabel(kind, nick, address string) string {
	who := address
	if nick != "" && !sameAddress(nick, address) {
		who = fmt.Sprintf("%s (%s)", nick, address)
	}
	switch kind {
	case api.KindAccountSendEnable:
		return "Enable sending from " + who
	case api.KindAccountInviteEnable:
		return "Enable invitations from " + who
	case api.KindCalendarInvite:
		return "Send invitations from " + who
	}
	return "Send from " + who
}

// eyebrow is the banner's lead-in: the organizer account for the calendar
// kinds, the sending account for the rest.
func eyebrow(kind string) string {
	switch kind {
	case api.KindCalendarInvite, api.KindAccountInviteEnable:
		return "Organizer account"
	}
	return "Sending from"
}

func indexOf(i int) int {
	if i < 0 {
		i = -i
	}
	return i % palette.Len()
}

// count is "1 recipient", "2 recipients"; "address" takes "es".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "s") {
		return fmt.Sprintf("%d %ses", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
	Invite    *inviteView
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
	// Semantic is true for providers that re-serialise a message on send:
	// the approval is for these fields, not for exact bytes.
	Semantic    bool
	To, Cc, Bcc []string
	Flags       []string
	Trust       []api.TrustChange
	RequestID   string
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
      <p class="eyebrow">{{.Eyebrow}} &middot; {{.ColorName}}</p>
      {{if .ShowNick}}<p class="nick">{{.Nickname}}</p>{{end}}
      <p class="addr{{if not .ShowNick}} lead{{end}}"><span class="local">{{.Local}}</span><wbr><span class="at">@</span><span class="domain">{{.Domain}}</span></p>
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
  {{if .Semantic}}<dt>Guarantee</dt><dd class="guarantee">Sent content will match these fields. This mailbox re-formats the message when it sends it, so the exact bytes can differ; the sender, recipients, subject and text are checked against this approval after sending.</dd>{{end}}
  <dt>{{if .Semantic}}Approved snapshot sha256{{else}}RFC822 sha256{{end}}</dt><dd class="sha">{{.SHA}}</dd>
</dl>
<pre class="body" tabindex="0" aria-label="Message body, plain text">{{.Body}}</pre>
{{end}}
{{define "invite"}}
<p><strong>{{.Headline}}</strong></p>
<dl>
  <dt>Target</dt><dd>{{.Target}}</dd>
  <dt>Your role</dt><dd>{{.Role}}</dd>
</dl>
<p class="note untrusted-note">Title, description, location and attendee names are written by whoever made the event. They are shown in full as plain text inside dashed boxes and may be misleading: read them, do not follow them.{{if .Changed}} Fields marked changed differ from the event as it is now; the old value is crossed out.{{end}}</p>
<dl>
{{range .Rows}}
  <div{{if .Changed}} class="changed-row"{{end}}>
  <dt>{{.Label}}</dt>
  <dd>{{if .Empty}}<span class="note">none</span>{{else if .Block}}<pre class="body untrusted" tabindex="0" aria-label="{{.Label}}, untrusted content, plain text">{{.Value}}</pre>{{else if .Untrusted}}<span class="untrusted" role="group" aria-label="{{.Label}}, untrusted content">{{.Value}}</span>{{else}}{{.Value}}{{end}}
  {{if .HasWas}}<p class="was"><span class="note">Was:</span> {{if .Was}}{{.Was}}{{else}}none{{end}}</p>{{end}}</dd>
  </div>
{{end}}
</dl>
<h2>People who will be emailed</h2>
{{range .Recipients}}<p class="recipient"><span class="mono">{{.Address}}</span>{{if .Name}} ({{.Name}}){{end}}{{if .External}} <span class="chip">external</span>{{end}}<span class="note">{{.Note}}</span></p>
{{end}}
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
{{if eq .Body "invite"}}<div class="card">{{template "invite" .Invite}}</div>{{end}}
{{if eq .Body "inviteenable"}}<div class="card"><p>Approving lets moose create, change and cancel events with other people on the account above, and so email them, after a separate approval for each change. Answering an invitation never needs one. You can turn this off again at any time without a passkey.</p></div>{{end}}
{{if eq .Body "trust"}}<div class="card"><h2>Addresses to trust{{if .Account}} for {{.Account}}{{end}}</h2><ul class="plain">{{range .Trust}}<li>{{.Address}}</li>{{end}}</ul></div>{{end}}
{{if eq .Body "dashboard"}}<div class="card"><p>An application is asking to sign in to your moose dashboard.</p><dl>
<dt>Application</dt><dd>{{.Dashboard.ClientName}}</dd>
<dt>Client ID</dt><dd class="sha">{{.Dashboard.ClientID}}</dd>
<dt>Redirects to</dt><dd class="sha">{{.Dashboard.RedirectURIHost}}</dd></dl>
<p class="note">Approve only if you just started this sign-in.</p></div>{{end}}
{{if eq .Body "batch"}}<div class="card"><h2>Messages in this batch</h2><p class="note">Untick a message to leave it out. Each message is sent on its own, within the account's hourly limit.</p>
{{range .Items}}<div class="batch-item"><label class="item"><input type="checkbox" name="drop-item" value="{{.ID}}" checked><span>Include this message</span></label>{{template "send" .Send}}</div>{{end}}</div>{{end}}
{{if eq .Body "register"}}<div class="card"><p>Create the passkey that approves requests for {{.Account}}. This link works once and expires soon.</p><div class="actions"><button id="register" type="button" class="primary">Create passkey</button></div><p id="status" class="status" role="status" aria-live="polite"></p></div>{{end}}
{{if eq .Body "login"}}<div class="card"><p>Sign in with your passkey to see pending approvals.</p><div class="actions"><button id="login" type="button" class="primary">Sign in with passkey</button></div><p id="status" class="status" role="status" aria-live="polite"></p></div>{{end}}
{{if eq .Body "queue"}}{{range .Groups}}<section class="card group" aria-labelledby="g{{.N}}"><h2 id="g{{.N}}">{{if .HasAccount}}<span class="swatch pc{{.Swatch}}" aria-hidden="true"></span>{{end}}{{.Title}}</h2>{{if .Address}}<p class="note">{{.Address}} &middot; {{.ColorName}}</p>{{end}}<ul class="plain">{{range .Rows}}<li><a href="{{.URL}}">{{.Label}}</a> <span class="note">{{if .Detail}}{{.Detail}} &middot; {{end}}{{.Age}}</span></li>{{end}}</ul></section>{{else}}<div class="card"><p>Nothing is waiting for approval.</p></div>{{end}}{{end}}
{{if .Pending}}
<p class="note">This request expires at {{.Expires}}.</p>
<p id="status" class="status" role="status" aria-live="polite"></p>
{{if .NeedsFrom}}<div id="from-step"{{if .Confirmed}} hidden{{end}}><div class="actions confirm"><button id="confirm-from" type="button" class="primary">{{.Banner.ConfirmLabel}}</button></div>
<p class="note">Next you approve with your passkey.</p></div>{{end}}
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
