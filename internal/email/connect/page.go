package connect

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/email/gmail"
	"github.com/change-fabric/moose/internal/email/palette"
	"github.com/change-fabric/moose/internal/email/store"
)

// GmailProfile is the production ProfileFunc: it asks Gmail who the token
// belongs to. Extra options (an endpoint, in tests) are passed to the client.
func GmailProfile(extra ...option.ClientOption) ProfileFunc {
	return func(ctx context.Context, ts oauth2.TokenSource) (Profile, error) {
		c, err := gmail.New(ctx, append([]option.ClientOption{option.WithTokenSource(ts)}, extra...)...)
		if err != nil {
			return Profile{}, err
		}
		p, err := c.Profile(ctx)
		if err != nil {
			return Profile{}, err
		}
		return Profile{Address: p.Address, DisplayName: p.DisplayName, Caps: c.Capabilities()}, nil
	}
}

// SessionAuth authenticates requests by the dashboard session cookie the
// approver sets after a passkey login.
type SessionAuth struct {
	Store store.Store
	Now   func() time.Time
}

// Owner returns the owner of a live, unrevoked dashboard session.
func (a SessionAuth) Owner(r *http.Request) (string, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	sess, err := a.Store.GetSession(r.Context(), store.HashSecret(c.Value))
	if err != nil || sess.Kind != store.SessionDashboard || sess.Revoked {
		return "", false
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if !now().Before(sess.Expires) {
		return "", false
	}
	return sess.Owner, true
}

// StaticOwner authenticates every request as one owner. Dev only.
type StaticOwner string

func (o StaticOwner) Owner(*http.Request) (string, bool) { return string(o), o != "" }

type rowView struct {
	ID, Address, DisplayName string
	Initial                            string
	ColorName, Hex, Ink, Pattern       string
	Groups                             string
	Status, StatusLabel                string
	LastSync                           string
	SendEnabled, IsDefault             bool
	NeedsReauth, Disconnected          bool
	CanReconnect, CanEnableSend        bool
	LastError                          string
}

type pageView struct {
	CSRF   string
	Rows   []rowView
	Notice string
	// SendNote explains why Enable send is unavailable, when it is.
	SendNote string
}

var statusLabels = map[string]string{
	store.StatusOK:           "Connected",
	store.StatusNeedsReauth:  "Needs reconnect",
	store.StatusError:        "Error",
	store.StatusDisconnected: "Disconnected",
}

// rows builds the view rows. Accounts that need reauthorization sort first;
// the rest keep connect order.
func rows(accts []store.Account, approverReady bool) []rowView {
	out := make([]rowView, 0, len(accts))
	for _, a := range accts {
		c := palette.At(a.ColorIndex)
		label := statusLabels[a.Status]
		if label == "" {
			label = a.Status
		}
		last := "never"
		if a.LastSyncAt != nil {
			last = a.LastSyncAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		out = append(out, rowView{
			ID: a.ID, Initial: initial(a.Address), Address: a.Address, DisplayName: a.DisplayName,
			ColorName: c.Name, Hex: c.Hex, Ink: c.Ink, Pattern: palette.Pattern(a.ColorIndex),
			Groups: strings.Join(a.Groups, ", "), Status: a.Status, StatusLabel: label, LastSync: last,
			SendEnabled: a.SendEnabled, IsDefault: a.IsDefault,
			NeedsReauth:   a.Status == store.StatusNeedsReauth,
			Disconnected:  a.Status == store.StatusDisconnected,
			CanReconnect:  a.Status == store.StatusNeedsReauth || a.Status == store.StatusDisconnected,
			CanEnableSend: !a.SendEnabled && a.Status == store.StatusOK && approverReady,
			LastError:     a.LastError,
		})
	}
	// accts arrive in connect order and the sort is stable.
	sort.SliceStable(out, func(i, j int) bool { return out[i].NeedsReauth && !out[j].NeedsReauth })
	return out
}

// initial is the monogram: the address's first character, upper-cased.
func initial(text string) string {
	for _, r := range text {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.owner(w, r)
	if !ok {
		return
	}
	accts, err := s.d.Store.ListAccounts(r.Context(), owner)
	if err != nil {
		s.internal(w, "list accounts", err)
		return
	}
	csrf := randomToken(24)
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		csrf = c.Value
	}
	http.SetCookie(w, s.cookie(csrfCookie, csrf, 3600, http.SameSiteStrictMode))
	v := pageView{CSRF: csrf, Rows: rows(accts, s.d.Approver != nil)}
	if s.d.Approver == nil {
		v.SendNote = "Every account is read-only for now. Turning on send with your passkey comes in a later update."
	}
	q := r.URL.Query()
	for key, tmpl := range map[string]string{
		"connected": "Connected %s. It is read-only until you enable send.", "sendoff": "Send is off for %s.",
		"disconnected": "Disconnected %s. Its tokens were deleted.",
	} {
		if id := q.Get(key); id != "" {
			if a, ok := findByID(accts, id); ok {
				v.Notice = strings.Replace(tmpl, "%s", a.Address, 1)
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, v); err != nil {
		s.d.Log.Warn("connect: render page", "err", err.Error())
	}
}

func (s *Server) fail(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = errTmpl.Execute(w, map[string]any{"Title": title, "Message": msg, "Back": PagePath})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.d.Log.Error("connect: "+what, "err", err.Error())
	s.fail(w, http.StatusInternalServerError, "Something went wrong", "The request could not be completed. Nothing was changed that you need to undo; try again.")
}

const css = `
:root{--bg:#fbfaf7;--fg:#1c1b19;--muted:#5d5a52;--card:#fff;--line:#d9d5c9;--accent:#0b5cad;--warn:#8a4b00;--ok:#1d6b3a;--bad:#a4261b}
@media (prefers-color-scheme:dark){:root{--bg:#161614;--fg:#f0eee8;--muted:#a8a498;--card:#201f1c;--line:#3a3833;--accent:#7fb4f0;--warn:#e6a85c;--ok:#74c895;--bad:#f08a80}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:46rem;margin:0 auto;padding:1.5rem 16px 3rem}
h1{font-size:1.5rem;margin:0 0 .25rem}
p.lede{color:var(--muted);margin:0 0 1.25rem}
ul.accts{list-style:none;margin:0;padding:0;display:grid;gap:.75rem}
li.acct{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem}
.head{display:flex;gap:.75rem;align-items:center;flex-wrap:wrap}
.swatch{display:inline-grid;place-items:center;width:2.5rem;height:2.5rem;border-radius:8px;font-weight:700;border:1px solid var(--line)}
.pat-stripes{background-image:repeating-linear-gradient(45deg,rgba(0,0,0,.28) 0 4px,transparent 4px 9px)}
.pat-dots{background-image:radial-gradient(rgba(0,0,0,.35) 2px,transparent 2px);background-size:8px 8px}
.pat-checks{background-image:conic-gradient(rgba(0,0,0,.3) 25%,transparent 0 50%,rgba(0,0,0,.3) 0 75%,transparent 0);background-size:10px 10px}
.nick{font-size:1.15rem;font-weight:650}
.addr{font-family:ui-monospace,Menlo,monospace;word-break:break-all}
.chip{display:inline-block;border:1px solid currentColor;border-radius:999px;padding:0 .6rem;font-size:.85rem}
.chip.ok{color:var(--ok)}.chip.needs_reauth,.chip.error{color:var(--bad)}.chip.disconnected{color:var(--muted)}
dl.meta{display:grid;grid-template-columns:max-content 1fr;gap:.15rem 1rem;margin:.75rem 0;color:var(--muted)}
dl.meta dt{font-weight:600}dl.meta dd{margin:0;color:var(--fg)}
.actions{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center;margin-top:.5rem}
form{margin:0;display:inline-flex;gap:.5rem;align-items:center;flex-wrap:wrap}
button,input[type=text]{min-height:44px;font:inherit;border-radius:8px;border:1px solid var(--line);padding:0 .9rem;background:var(--card);color:var(--fg)}
button{cursor:pointer;font-weight:600}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
@media (prefers-color-scheme:dark){button.primary{color:#0b1b2e}}
button:focus-visible,input:focus-visible,a:focus-visible{outline:3px solid var(--accent);outline-offset:2px}
.notice{border:1px solid var(--ok);color:var(--ok);border-radius:8px;padding:.6rem .9rem;margin-bottom:1rem}
.note{color:var(--muted);border-left:3px solid var(--line);padding:.1rem 0 .1rem .75rem;margin:0 0 1.25rem}
.err{color:var(--bad)}
.add{margin-top:1.5rem;background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem}
.add label{display:block;font-weight:600;margin-bottom:.35rem}
.add form{display:flex;flex-wrap:wrap;gap:.5rem;align-items:stretch;width:100%}
.add input[type=text]{flex:1 1 14rem;min-width:0}
.add .hint{color:var(--muted);font-size:.9rem;margin:.75rem 0 0}
.add ol{color:var(--muted);font-size:.9rem;margin:.25rem 0 0;padding-left:1.25rem}
.empty{border:1px dashed var(--line);border-radius:10px;padding:1rem;color:var(--muted);margin:0}
details.danger-zone{margin-top:.75rem}
details.danger-zone>summary{list-style:none;display:inline-flex;align-items:center;min-height:44px;padding:0 .9rem;border:1px solid var(--bad);border-radius:8px;color:var(--bad);font-weight:600;cursor:pointer}
details.danger-zone>summary::-webkit-details-marker{display:none}
details.danger-zone[open]>summary{background:var(--bad);color:var(--card)}
.confirm{margin-top:.75rem;border:1px solid var(--bad);border-radius:8px;padding:.75rem}
.confirm p{margin:0 0 .5rem;color:var(--bad)}
.confirm form{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center;width:100%}
.confirm label{flex-basis:100%}
.confirm input[type=text]{flex:1 1 14rem;min-width:0}
button.danger{background:var(--bad);border-color:var(--bad);color:var(--card)}
button.danger:disabled{opacity:.45;cursor:not-allowed}
@media (prefers-reduced-motion:no-preference){button{transition:background-color .15s}}
`

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>moose accounts</title><style>` + css + `</style></head>
<body><main>
<h1>Connected accounts</h1>
<p class="lede">The Gmail accounts Claude can reach through this moose connection.</p>
{{if .Notice}}<p class="notice" role="status">{{.Notice}}</p>{{end}}
{{if .SendNote}}<p class="note">{{.SendNote}}</p>{{end}}
{{if .Rows}}<ul class="accts">
{{range .Rows}}<li class="acct"><section aria-labelledby="n-{{.ID}}">
 <div class="head">
  <span class="swatch pat-{{.Pattern}}" style="background-color:{{.Hex}};color:{{.Ink}}" aria-hidden="true">{{.Initial}}</span>
  <div><div class="nick addr" id="n-{{.ID}}">{{.Address}}{{if .IsDefault}} <small>(default)</small>{{end}}</div></div>
  <span class="chip {{.Status}}">{{.StatusLabel}}</span>
 </div>
 <dl class="meta">
  <dt>Colour</dt><dd>{{.ColorName}}</dd>
  {{if .DisplayName}}<dt>Name</dt><dd>{{.DisplayName}}</dd>{{end}}
  {{if .Groups}}<dt>Groups</dt><dd>{{.Groups}}</dd>{{end}}
  <dt>Last sync</dt><dd>{{.LastSync}}</dd>
  <dt>Send</dt><dd>{{if .SendEnabled}}Enabled{{else}}Off (read-only){{end}}</dd>
  {{if .LastError}}<dt>Last error</dt><dd class="err">{{.LastError}}</dd>{{end}}
 </dl>
 <div class="actions">
  {{if .CanReconnect}}<form method="post" action="/connect/google/start"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="account_id" value="{{.ID}}"><button class="primary" type="submit">Reconnect</button></form>{{end}}
  {{if .SendEnabled}}<form method="post" action="/connect/accounts/{{.ID}}/disable-send"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Disable send</button></form>{{end}}
  {{if .CanEnableSend}}<form method="post" action="/connect/accounts/{{.ID}}/enable-send"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Enable send (passkey)</button></form>{{end}}
 </div>
 {{if not .Disconnected}}<details class="danger-zone"><summary>Disconnect</summary>
  <div class="confirm">
  <p>This deletes the stored Google tokens for <b>{{.Address}}</b>. Claude loses access until you connect it again.</p>
  <form method="post" action="/connect/accounts/{{.ID}}/disconnect"><input type="hidden" name="csrf" value="{{$.CSRF}}">
   <label for="c-{{.ID}}">Type <b>{{.Address}}</b> to confirm</label>
   <input type="text" id="c-{{.ID}}" name="confirm" autocomplete="off" spellcheck="false" autocapitalize="off" required>
   <button class="danger" type="submit">Disconnect permanently</button></form>
  </div>
 </details>{{end}}
</section></li>{{end}}
</ul>{{else}}<p class="empty">No accounts connected yet. Connect your first Gmail account below.</p>{{end}}
<section class="add" aria-labelledby="add-h"><h2 id="add-h" style="font-size:1.1rem;margin:0 0 .5rem">Connect a Google account</h2>
<form method="post" action="/connect/google/start"><input type="hidden" name="csrf" value="{{.CSRF}}">
<button class="primary" type="submit">Connect Google account</button></form>
<p class="hint">Google asks for permission to read mail and manage drafts. Because this is your own private app, Google first shows an unverified-app warning:</p>
<ol><li>Click <b>Advanced</b>.</li><li>Click <b>Go to moose.changefabric.org (unsafe)</b>.</li><li>Allow both permissions.</li></ol>
</section>
</main></body></html>`))

var errTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>` + css + `</style></head>
<body><main><h1>{{.Title}}</h1><p role="alert">{{.Message}}</p><p><a href="{{.Back}}">Back to accounts</a></p></main></body></html>`))
