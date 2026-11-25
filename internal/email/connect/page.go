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
	"github.com/change-fabric/moose/internal/webui"
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
	ID, Address, DisplayName         string
	Initial                          string
	ColorName, Hex, Ink, Pattern     string
	Groups                           string
	Status, StatusLabel              string
	LastSync                         string
	SendEnabled, IsDefault, Verified bool
	NeedsReauth, Disconnected        bool
	CanReconnect, CanEnableSend      bool
	LastError                        string
}

type pageView struct {
	CSRF   string
	Rows   []rowView
	Notice string
	// SendNote explains why Enable send is unavailable, when it is.
	SendNote string
}

var statusLabels = map[string]string{
	store.StatusOK:           "Access verified",
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
		last := "Not used by Claude yet"
		if a.LastSyncAt != nil {
			last = "Last read " + a.LastSyncAt.UTC().Format("Jan 2, 15:04 UTC")
		}
		out = append(out, rowView{
			ID: a.ID, Initial: initial(a.Address), Address: a.Address, DisplayName: a.DisplayName,
			ColorName: c.Name, Hex: c.Hex, Ink: c.Ink, Pattern: palette.Pattern(a.ColorIndex),
			Groups: strings.Join(a.Groups, ", "), Status: a.Status, StatusLabel: label, LastSync: last,
			SendEnabled: a.SendEnabled, IsDefault: a.IsDefault, Verified: a.Status == store.StatusOK,
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
	s.failWith(w, code, title, msg, PagePath, "Back to accounts")
}

// failWith renders an error page with a single action link.
func (s *Server) failWith(w http.ResponseWriter, code int, title, msg, href, label string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = errTmpl.Execute(w, map[string]any{"Title": title, "Message": msg, "Href": href, "Label": label})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.d.Log.Error("connect: "+what, "err", err.Error())
	s.fail(w, http.StatusInternalServerError, "Something went wrong", "The request could not be completed. Nothing was changed that you need to undo; try again.")
}

const css = webui.Tokens + webui.Base + `
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:46rem;margin:0 auto;padding:1.5rem 16px 3rem}
h1{font-size:1.5rem;margin:0 0 .25rem}
p.lede{color:var(--muted);margin:0 0 1.25rem}
ul.accts{list-style:none;margin:0;padding:0;display:grid;gap:.75rem}
li.acct{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:1rem}
.swatch{display:inline-grid;place-items:center;width:2.5rem;height:2.5rem;border-radius:8px;font-weight:700;border:1px solid var(--line)}
.pat-stripes{background-image:repeating-linear-gradient(45deg,rgba(0,0,0,.28) 0 4px,transparent 4px 9px)}
.pat-dots{background-image:radial-gradient(rgba(0,0,0,.35) 2px,transparent 2px);background-size:8px 8px}
.pat-checks{background-image:conic-gradient(rgba(0,0,0,.3) 25%,transparent 0 50%,rgba(0,0,0,.3) 0 75%,transparent 0);background-size:10px 10px}
.addr{font-family:ui-monospace,Menlo,monospace;word-break:break-all}
.chip{display:inline-block;border:1px solid currentColor;border-radius:999px;padding:0 .6rem;font-size:.85rem}
.chip.ok{color:var(--ok)}.chip.needs_reauth,.chip.error{color:var(--bad)}.chip.disconnected{color:var(--muted)}
form{margin:0;display:inline-flex;gap:.5rem;align-items:center;flex-wrap:wrap}
button,input[type=text]{min-height:44px;font:inherit;border-radius:8px;border:1px solid var(--field);padding:0 .9rem;background:var(--card);color:var(--fg)}
button{cursor:pointer;font-weight:600}
a.btn.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-ink)}
a.btn{display:inline-flex;align-items:center;min-height:44px;padding:0 .9rem;border-radius:8px;border:1px solid var(--field);font-weight:600;text-decoration:none}
.notice{border:1px solid var(--ok);color:var(--ok);border-radius:8px;padding:.6rem .9rem;margin-bottom:1rem}
.note{color:var(--muted);border-left:3px solid var(--line);padding:.1rem 0 .1rem .75rem;margin:0 0 1.25rem}
.err{color:var(--bad)}
.empty{border:1px dashed var(--line);border-radius:10px;padding:1rem;color:var(--muted);margin:0}
.top{display:flex;flex-wrap:wrap;gap:1rem;align-items:flex-end;justify-content:space-between;margin-bottom:.5rem}
.top p.lede{margin:0}
details.help{color:var(--muted);font-size:.9rem;margin:0 0 1.25rem}
details.help>summary{min-height:44px;display:inline-flex;align-items:center;cursor:pointer;color:var(--accent)}
details.help p{margin:.25rem 0 0}
.head{display:flex;gap:.85rem;align-items:flex-start;flex-wrap:wrap}
.ident{display:grid;gap:.3rem;min-width:0;flex:1}
.ident .addr{font-size:1.05rem;font-weight:600}
.status{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center}
.meta{color:var(--muted);font-size:.9rem}
.bar{display:flex;flex-wrap:wrap;gap:.75rem 1rem;align-items:flex-start;justify-content:space-between;margin-top:.9rem;padding-top:.9rem;border-top:1px solid var(--line)}
.group{display:flex;flex-wrap:wrap;gap:.5rem .75rem;align-items:center}
details.danger-zone{margin:0}
details.danger-zone[open]{flex-basis:100%}
details.danger-zone>summary{list-style:none;display:inline-flex;align-items:center;min-height:44px;padding:0 .9rem;border:1px solid var(--bad);border-radius:8px;color:var(--bad);font-weight:600;cursor:pointer}
details.danger-zone>summary::-webkit-details-marker{display:none}
details.danger-zone[open]>summary{color:var(--muted);border-color:var(--line)}
details.danger-zone .opened{display:none}details.danger-zone[open] .opened{display:inline}details.danger-zone[open] .closed{display:none}
.bar form,.bar details{flex:0 0 auto}
@media (max-width:30rem){.bar{display:grid;grid-template-columns:1fr 1fr;gap:.5rem}.bar>.group{display:contents}.bar button,.bar summary{width:100%;justify-content:center}details.danger-zone[open]{grid-column:1/-1}}
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
<header class="top">
 <div><h1>Connected accounts</h1>
 <p class="lede">The Gmail accounts Claude can reach through this moose connection.</p></div>
 <form method="post" action="/connect/google/start"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="primary" type="submit">Connect Google account</button></form>
</header>
<details class="help"><summary>What happens when I connect?</summary>
 <p>Google asks for permission to read mail and manage drafts. Because this is your own private app, it first shows an unverified-app warning: click <b>Advanced</b>, then <b>Go to moose.changefabric.org (unsafe)</b>, then allow both permissions.</p>
</details>
{{if .Notice}}<p class="notice" role="status">{{.Notice}}</p>{{end}}
{{if .SendNote}}<p class="note">{{.SendNote}}</p>{{end}}
{{if .Rows}}<ul class="accts">
{{range .Rows}}<li class="acct"><section aria-labelledby="n-{{.ID}}">
 <div class="head">
  <span class="swatch pat-{{.Pattern}}" style="background-color:{{.Hex}};color:{{.Ink}}" role="img" aria-label="Colour {{.ColorName}}" title="{{.ColorName}}">{{.Initial}}</span>
  <div class="ident">
   <div class="addr" id="n-{{.ID}}">{{.Address}}</div>
   <div class="status"><span class="chip {{.Status}}">{{.StatusLabel}}</span>
    <span class="meta">{{if .Verified}}Gmail read checked at connect · {{end}}{{.LastSync}}</span></div>
   <div class="meta">{{if .IsDefault}}Default · {{end}}{{.ColorName}} · {{if .SendEnabled}}Sending on{{else}}Read-only{{end}}</div>
   {{if .DisplayName}}<div class="meta">{{.DisplayName}}{{if .Groups}} · {{.Groups}}{{end}}</div>{{end}}
   {{if .LastError}}<div class="err">{{.LastError}}</div>{{end}}
  </div>
 </div>
 <div class="bar">
  <div class="group">
   {{if .CanReconnect}}<form method="post" action="/connect/google/start"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="account_id" value="{{.ID}}"><button class="primary" type="submit">Reconnect</button></form>{{end}}
   {{if .SendEnabled}}<form method="post" action="/connect/accounts/{{.ID}}/disable-send"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Turn off sending</button></form>{{end}}
   {{if .CanEnableSend}}<form method="post" action="/connect/accounts/{{.ID}}/enable-send"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit">Enable sending...</button></form>{{end}}
  </div>
  {{if not .Disconnected}}<details class="danger-zone"><summary><span class="closed">Disconnect...</span><span class="opened">Cancel</span></summary>
   <div class="confirm">
   <p>This deletes the stored Google tokens for <b>{{.Address}}</b>. Claude loses access until you connect it again.</p>
   <form method="post" action="/connect/accounts/{{.ID}}/disconnect"><input type="hidden" name="csrf" value="{{$.CSRF}}">
    <label for="c-{{.ID}}">Type <b>{{.Address}}</b> to confirm</label>
    <input type="text" id="c-{{.ID}}" name="confirm" autocomplete="off" spellcheck="false" autocapitalize="off" required>
    <button class="danger" type="submit">Disconnect account</button></form>
   </div>
  </details>{{end}}
 </div>
</section></li>{{end}}
</ul>{{else}}<p class="empty">No accounts connected yet. Use Connect Google account above to add your first one.</p>{{end}}
</main></body></html>`))

var errTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title><style>` + css + `</style></head>
<body><main><h1>{{.Title}}</h1><p role="alert">{{.Message}}</p><p><a class="btn primary" href="{{.Href}}">{{.Label}}</a></p></main></body></html>`))
