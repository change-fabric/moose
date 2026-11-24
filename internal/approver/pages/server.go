// Package pages serves moose-approver's browser side: the server-rendered
// approval page for each request, the passkey enrollment page and the approval
// queue.
//
// A page shows the exact thing being approved. For mail that is the headers,
// recipients with soft flags, the plain-text body and the RFC822 sha256,
// rendered from the sealed preview; the account address in the FROM banner
// comes from the signed payload. Sending kinds put the banner first and make
// the owner tap "Send from <nickname> (<address>)" before the passkey prompt:
// the tap is a conditional write on the request row, and an assertion for such
// a request is refused until it is recorded.
package pages

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/approver/webauthn"
	"github.com/change-fabric/moose/internal/email/palette"
)

// Server serves the approval pages.
type Server struct {
	Store    *store.Store
	WebAuthn *webauthn.Service
	Signer   receipt.Signer
	RPID     string
	// Origin is the exact origin the pages are served from; it is checked on
	// every state-changing request.
	Origin string
	Owner  string
	// Secret signs the queue session cookie.
	Secret []byte
	// InsecureCookies drops the Secure flag for http://localhost in dev.
	InsecureCookies bool
	Now             func() time.Time
	Log             *slog.Logger

	css string
}

const (
	queueCookie = "moose_approver_queue"
	queueTTL    = 15 * time.Minute
	maxBody     = 1 << 16
)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler serves /approve/r/, /approve/register/ and /approve/queue.
func (s *Server) Handler() http.Handler {
	s.css = css()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /approve/r/{id}", s.page)
	mux.HandleFunc("POST /approve/r/{id}/from", s.confirmFrom)
	mux.HandleFunc("POST /approve/r/{id}/begin", s.begin)
	mux.HandleFunc("POST /approve/r/{id}/finish", s.finish)
	mux.HandleFunc("GET /approve/register/{token}", s.registerPage)
	mux.HandleFunc("POST /approve/register/{token}/begin", s.registerBegin)
	mux.HandleFunc("POST /approve/register/{token}/finish", s.registerFinish)
	mux.HandleFunc("GET /approve/queue", s.queue)
	mux.HandleFunc("POST /approve/queue/login/begin", s.queueLoginBegin)
	mux.HandleFunc("POST /approve/queue/login/finish", s.queueLoginFinish)
	return mux
}

// ---- plumbing ----

func (s *Server) logf(msg string, err error) {
	if s.Log != nil {
		s.Log.Error("approver pages: "+msg, "err", err)
	}
}

func nonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func secureHeaders(w http.ResponseWriter, n string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+n+"'; style-src 'nonce-"+n+"'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func (s *Server) writePage(w http.ResponseWriter, status int, v pageView) {
	n := nonce()
	v.Nonce, v.CSS = n, template.CSS(s.css)
	body, err := render(v, script)
	if err != nil {
		s.logf("render", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	secureHeaders(w, n)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, code, msg string) {
	jsonOut(w, status, map[string]string{"error": code, "message": msg})
}

// sameOrigin rejects a state-changing request that did not come from this
// origin's own pages.
func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != s.Origin {
		jsonErr(w, http.StatusForbidden, "bad_origin", "request did not come from this site")
		return false
	}
	return true
}

func stateHeading(status string) (heading, msg string) {
	switch status {
	case store.StatusApproved:
		return "Approved", "This request was approved. You can close this page."
	case store.StatusDenied:
		return "Denied", "This request was denied. Nothing was sent or changed."
	case store.StatusExpired:
		return "Expired", "This request expired before it was decided. Ask for a new one."
	case store.StatusConsumed:
		return "Already used", "This approval has been used. It cannot be used again."
	}
	return "Not available", "This request cannot be decided."
}

// ---- approval page ----

func softFlagText(f api.SoftFlag) string {
	label := map[string]string{
		"new_recipient": "New recipient", "external_domain": "External domain",
		"bcc": "Bcc recipient", "many_recipients": "More than 10 recipients",
	}[f.Flag]
	if label == "" {
		label = f.Flag
	}
	if f.Address != "" {
		return label + ": " + f.Address
	}
	return label
}

func sendViewOf(pv api.Preview, p api.EmailSendPayload) *sendView {
	v := &sendView{SHA: p.RFC822SHA256, Trust: p.TrustChanges}
	if m := pv.Message; m != nil {
		v.From, v.Subject, v.Body, v.To, v.Cc, v.Bcc = m.From, m.Subject, m.Body, m.To, m.Cc, m.Bcc
	}
	for _, f := range p.SoftFlags {
		v.Flags = append(v.Flags, softFlagText(f))
	}
	return v
}

func (s *Server) loadPreview(r *http.Request, req store.Request) (api.Preview, error) {
	raw, err := s.Store.Preview(r.Context(), req)
	if err != nil {
		return api.Preview{}, err
	}
	var pv api.Preview
	return pv, json.Unmarshal(raw, &pv)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	req, err := s.Store.Fresh(r.Context(), r.PathValue("id"), s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.writePage(w, http.StatusNotFound, pageView{Title: "Approval not found", Heading: "Not found", Message: "This approval link is not valid."})
		return
	}
	if err != nil {
		s.logf("page", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := pageView{Kind: req.Kind, Title: "Approval request", Heading: "Approval request"}
	if req.Status != store.StatusPending {
		v.Heading, v.Message = stateHeading(req.Status)
		v.Title = v.Heading
		s.writePage(w, http.StatusOK, v)
		return
	}
	pv, err := s.loadPreview(r, req)
	if err != nil {
		v.Heading, v.Message = "Preview unavailable", "The preview for this request is no longer available, so it cannot be approved. Ask for a new request."
		s.writePage(w, http.StatusOK, v)
		return
	}
	parsed, err := api.ParsePayload(req.Kind, req.Payload)
	if err != nil {
		s.logf("stored payload", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v.Pending = true
	v.Expires = req.Expires.UTC().Format("15:04:05 UTC")
	v.NeedsFrom = api.FromKinds(req.Kind)
	v.Confirmed = req.FromConfirmedAt != nil
	if v.NeedsFrom {
		b := newBanner(pv, req.FromAddress, s.now())
		v.Banner = &b
	}
	switch req.Kind {
	case api.KindEmailSend:
		v.Title, v.Heading, v.Body = "Approve email", "Approve this email", "send"
		v.Send = sendViewOf(pv, *parsed.Email)
		v.Send.RequestID = req.ID
	case api.KindAccountSendEnable:
		v.Title, v.Heading, v.Body = "Enable sending", "Enable sending from this account", "enable"
	case api.KindContactTrustBatch:
		v.Title, v.Heading, v.Body = "Trust contacts", "Trust these addresses", "trust"
		v.Trust = parsed.Trust.Changes
		v.Account = pv.Nickname
		if v.Account == "" {
			v.Account = parsed.Trust.AccountID
		}
	case api.KindDashboardSession:
		v.Title, v.Heading, v.Body = "Sign in", "Sign in to moose", "dashboard"
		v.Dashboard = parsed.Dashboard
	case api.KindEmailSendBatch:
		v.Title, v.Heading, v.Body = "Approve batch", fmt.Sprintf("Approve %d emails from one account", len(parsed.Batch.Items)), "batch"
		for _, it := range parsed.Batch.Items {
			child, err := s.Store.Fresh(r.Context(), it.RequestID, s.now())
			cpv, perr := api.Preview{}, error(nil)
			if err == nil && child.Status == store.StatusPending {
				cpv, perr = s.loadPreview(r, child)
			}
			if err != nil || child.Status != store.StatusPending || perr != nil {
				v.Pending, v.Banner, v.Body = false, nil, ""
				v.Heading, v.Message = "Batch not available", "One of the messages in this batch is no longer pending, so the batch cannot be approved. Ask for a new request."
				break
			}
			cp, _ := api.ParsePayload(api.KindEmailSend, child.Payload)
			v.Items = append(v.Items, batchItemView{ID: child.ID, Send: sendViewOf(cpv, *cp.Email)})
		}
	}
	s.writePage(w, http.StatusOK, v)
}

// ---- FROM tap ----

func (s *Server) confirmFrom(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	req, err := s.Store.Fresh(r.Context(), r.PathValue("id"), s.now())
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "not_found", "")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if !api.FromKinds(req.Kind) {
		jsonErr(w, http.StatusBadRequest, "no_sender", "this request has no sending account")
		return
	}
	if req.Status != store.StatusPending {
		jsonErr(w, http.StatusConflict, "not_pending", "request is "+req.Status)
		return
	}
	got, err := s.Store.ConfirmFrom(r.Context(), req.ID, s.now())
	if err != nil {
		jsonErr(w, http.StatusConflict, "not_pending", err.Error())
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"confirmed_at": got.FromConfirmedAt})
}

// ---- assertion ceremony ----

type batchPlan struct {
	kept, dropped []string
	payload       []byte
	sha           string
}

// planBatch splits the batch's items by the drop list and builds the payload
// the receipt will cover. Every drop must name an item of the batch.
func planBatch(req store.Request, drops []string) (batchPlan, error) {
	parsed, err := api.ParsePayload(api.KindEmailSendBatch, req.Payload)
	if err != nil {
		return batchPlan{}, err
	}
	drop := map[string]bool{}
	for _, d := range drops {
		drop[d] = true
	}
	final := *parsed.Batch
	final.Items = nil
	var plan batchPlan
	for _, it := range parsed.Batch.Items {
		if drop[it.RequestID] {
			plan.dropped = append(plan.dropped, it.RequestID)
			delete(drop, it.RequestID)
			continue
		}
		plan.kept = append(plan.kept, it.RequestID)
		final.Items = append(final.Items, it)
	}
	if len(drop) > 0 {
		return batchPlan{}, errors.New("drop names an item that is not in the batch")
	}
	raw, err := json.Marshal(final)
	if err != nil {
		return batchPlan{}, err
	}
	canon, err := api.Canonical(raw)
	if err != nil {
		return batchPlan{}, err
	}
	plan.payload, plan.sha = canon, api.SHA256(canon)
	return plan, nil
}

// digestFor is the payload hash an assertion for this decision is bound to.
func digestFor(req store.Request, action string, drops []string) (string, error) {
	if action == webauthn.PurposeApprove && req.Kind == api.KindEmailSendBatch {
		plan, err := planBatch(req, drops)
		if err != nil {
			return "", err
		}
		if len(plan.kept) == 0 {
			return "", errors.New("a batch needs at least one message; deny it instead")
		}
		return plan.sha, nil
	}
	return req.PayloadSHA256, nil
}

func (s *Server) begin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	var in struct {
		Action string   `json:"action"`
		Drop   []string `json:"drop"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil || (in.Action != webauthn.PurposeApprove && in.Action != webauthn.PurposeDeny) {
		jsonErr(w, http.StatusBadRequest, "bad_request", "action must be approve or deny")
		return
	}
	req, err := s.Store.Fresh(r.Context(), r.PathValue("id"), s.now())
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "not_found", "")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if req.Status != store.StatusPending {
		jsonErr(w, http.StatusConflict, "not_pending", "request is "+req.Status)
		return
	}
	if in.Action == webauthn.PurposeApprove {
		if ok, _ := s.Store.HasPreview(r.Context(), req.ID); !ok {
			jsonErr(w, http.StatusConflict, "preview_unavailable", "the preview is gone, so this cannot be approved")
			return
		}
		if api.FromKinds(req.Kind) && req.FromConfirmedAt == nil {
			jsonErr(w, http.StatusConflict, "from_not_confirmed", "tap the Send from button first")
			return
		}
	}
	if in.Action == webauthn.PurposeDeny {
		in.Drop = nil
	}
	digest, err := digestFor(req, in.Action, in.Drop)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad_drop", err.Error())
		return
	}
	assertion, err := s.WebAuthn.BeginAssertion(r.Context(), req.Owner, webauthn.Binding{
		Purpose: in.Action, RequestID: req.ID, PayloadSHA256: digest, Drops: in.Drop,
	})
	if errors.Is(err, webauthn.ErrNoCredential) {
		jsonErr(w, http.StatusConflict, "no_passkey", "no passkey is enrolled; run moose-approver enroll")
		return
	}
	if err != nil {
		s.logf("begin", err)
		jsonErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	jsonOut(w, http.StatusOK, assertion)
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	req, err := s.Store.Fresh(r.Context(), r.PathValue("id"), s.now())
	if errors.Is(err, store.ErrNotFound) {
		jsonErr(w, http.StatusNotFound, "not_found", "")
		return
	}
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	if req.Status != store.StatusPending {
		jsonErr(w, http.StatusConflict, "not_pending", "request is "+req.Status)
		return
	}
	c, err := s.WebAuthn.FinishAssertion(r.Context(), req.Owner, body, func(c store.Challenge) error {
		if c.RequestID != req.ID {
			return errors.New("challenge was issued for another request")
		}
		if c.Purpose != webauthn.PurposeApprove && c.Purpose != webauthn.PurposeDeny {
			return errors.New("challenge was issued for another purpose")
		}
		want, err := digestFor(req, c.Purpose, c.Drops)
		if err != nil {
			return err
		}
		if c.PayloadSHA256 != want {
			return errors.New("challenge was issued for another payload")
		}
		return nil
	})
	if err != nil {
		code := http.StatusForbidden
		if !errors.Is(err, webauthn.ErrChallenge) && !errors.Is(err, webauthn.ErrBinding) && !errors.Is(err, webauthn.ErrUserVerification) && !strings.Contains(err.Error(), "assertion rejected") {
			s.logf("finish", err)
			code = http.StatusInternalServerError
		}
		jsonErr(w, code, "assertion_rejected", "the passkey assertion was not accepted")
		return
	}
	now := s.now()
	if c.Purpose == webauthn.PurposeDeny {
		if _, err := s.Store.Deny(r.Context(), req.ID, now); err != nil {
			jsonErr(w, http.StatusConflict, "not_pending", err.Error())
			return
		}
		jsonOut(w, http.StatusOK, map[string]string{"status": store.StatusDenied})
		return
	}
	sign := func(rq store.Request) (receipt.Receipt, error) {
		return receipt.Sign(r.Context(), s.Signer, receipt.Body{
			RequestID: rq.ID, Kind: rq.Kind, PayloadSHA256: rq.PayloadSHA256,
			ApprovedAt: now, ExpiresAt: now.Add(store.ReceiptLifetime), RPID: s.RPID,
		})
	}
	if req.Kind == api.KindEmailSendBatch {
		plan, err := planBatch(req, c.Drops)
		if err == nil {
			_, err = s.Store.ApproveBatch(r.Context(), req.ID, now, plan.kept, plan.dropped,
				func(store.Request) ([]byte, string, error) { return plan.payload, plan.sha, nil }, sign)
		}
		s.approved(w, err)
		return
	}
	_, err = s.Store.Approve(r.Context(), req.ID, now, api.FromKinds(req.Kind), sign)
	s.approved(w, err)
}

func (s *Server) approved(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		jsonOut(w, http.StatusOK, map[string]string{"status": store.StatusApproved})
	case errors.Is(err, store.ErrNeedsFromConfirm):
		jsonErr(w, http.StatusConflict, "from_not_confirmed", "the sending account was not confirmed")
	case errors.Is(err, store.ErrInvalidTransition), errors.Is(err, store.ErrExpired):
		jsonErr(w, http.StatusConflict, "not_pending", err.Error())
	default:
		s.logf("approve", err)
		jsonErr(w, http.StatusInternalServerError, "internal", "")
	}
}

// ---- passkey enrollment ----

func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	t, err := s.Store.PeekEnrollToken(r.Context(), store.Hash(r.PathValue("token")), s.now())
	if err != nil {
		s.writePage(w, http.StatusNotFound, pageView{Title: "Link not valid", Heading: "Link not valid", Message: "This enrollment link is not valid, was already used, or has expired. Run moose-approver enroll for a new one."})
		return
	}
	s.writePage(w, http.StatusOK, pageView{Title: "Create passkey", Heading: "Create your passkey", Body: "register", Account: t.Owner})
}

func (s *Server) registerBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	creation, err := s.WebAuthn.BeginRegistration(r.Context(), r.PathValue("token"))
	if errors.Is(err, webauthn.ErrEnrollToken) {
		jsonErr(w, http.StatusForbidden, "bad_token", "this enrollment link is not valid, was used or has expired")
		return
	}
	if err != nil {
		s.logf("register begin", err)
		jsonErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	jsonOut(w, http.StatusOK, creation)
}

func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	_, err := s.WebAuthn.FinishRegistration(r.Context(), r.PathValue("token"), body)
	switch {
	case err == nil:
		jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, webauthn.ErrEnrollToken), errors.Is(err, webauthn.ErrChallenge), errors.Is(err, webauthn.ErrBinding), errors.Is(err, webauthn.ErrUserVerification):
		jsonErr(w, http.StatusForbidden, "registration_rejected", "the passkey was not accepted")
	case strings.Contains(err.Error(), "attestation rejected") || strings.Contains(err.Error(), "malformed"):
		jsonErr(w, http.StatusBadRequest, "registration_rejected", "the passkey was not accepted")
	default:
		s.logf("register finish", err)
		jsonErr(w, http.StatusInternalServerError, "internal", "")
	}
}

// ---- queue ----

var queueLoginDigest = func() string {
	h := sha256.Sum256([]byte("moose-approver-queue-login"))
	return hex.EncodeToString(h[:])
}()

func (s *Server) queueMAC(payload string) string {
	m := hmac.New(sha256.New, s.Secret)
	m.Write([]byte("queue-session\n" + payload))
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) queueAuthed(r *http.Request) bool {
	c, err := r.Cookie(queueCookie)
	if err != nil || len(s.Secret) == 0 {
		return false
	}
	payload, mac, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(s.queueMAC(payload))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	owner, exp, ok := strings.Cut(string(raw), "|")
	if !ok || owner != s.Owner {
		return false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && s.now().Before(time.Unix(unix, 0))
}

func (s *Server) queueLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	assertion, err := s.WebAuthn.BeginAssertion(r.Context(), s.Owner, webauthn.Binding{
		Purpose: webauthn.PurposeQueueLogin, RequestID: "queue", PayloadSHA256: queueLoginDigest,
	})
	if errors.Is(err, webauthn.ErrNoCredential) {
		jsonErr(w, http.StatusConflict, "no_passkey", "no passkey is enrolled; run moose-approver enroll")
		return
	}
	if err != nil {
		s.logf("queue login begin", err)
		jsonErr(w, http.StatusInternalServerError, "internal", "")
		return
	}
	jsonOut(w, http.StatusOK, assertion)
}

func (s *Server) queueLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
	_, err := s.WebAuthn.FinishAssertion(r.Context(), s.Owner, body, func(c store.Challenge) error {
		if c.Purpose != webauthn.PurposeQueueLogin || c.RequestID != "queue" || c.PayloadSHA256 != queueLoginDigest {
			return errors.New("challenge was not issued for the queue sign-in")
		}
		return nil
	})
	if err != nil {
		jsonErr(w, http.StatusForbidden, "assertion_rejected", "the passkey assertion was not accepted")
		return
	}
	exp := s.now().Add(queueTTL)
	payload := base64.RawURLEncoding.EncodeToString([]byte(s.Owner + "|" + strconv.FormatInt(exp.Unix(), 10)))
	http.SetCookie(w, &http.Cookie{
		Name: queueCookie, Value: payload + "." + s.queueMAC(payload), Path: "/approve", Expires: exp,
		HttpOnly: true, Secure: !s.InsecureCookies, SameSite: http.SameSiteStrictMode,
	})
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

var kindLabels = map[string]string{
	api.KindEmailSend: "Email", api.KindAccountSendEnable: "Enable sending", api.KindEmailSendBatch: "Email batch",
	api.KindContactTrustBatch: "Trust contacts", api.KindDashboardSession: "Dashboard sign-in",
}

// queue lists pending approvals grouped by account (R8). It needs a passkey
// sign-in first, because the list links to every pending approval.
func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	if !s.queueAuthed(r) {
		s.writePage(w, http.StatusUnauthorized, pageView{Title: "Approval queue", Heading: "Approval queue", Body: "login"})
		return
	}
	open, err := s.Store.ListOpen(r.Context(), s.Owner)
	if err != nil {
		s.logf("queue", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.now()
	groups := map[string]*queueGroup{}
	var order []string
	for _, req := range open {
		if req.Status != store.StatusPending || !now.Before(req.Expires) {
			continue
		}
		key := req.AccountID
		g := groups[key]
		if g == nil {
			g = &queueGroup{N: len(order), Title: "Not tied to an account"}
			if key != "" {
				g.HasAccount, g.Title = true, key
				if pv, err := s.loadPreview(r, req); err == nil {
					g.Title, g.Address = pv.Nickname, req.FromAddress
					if g.Address == "" {
						g.Address = pv.FromAddress
					}
					g.Swatch, g.ColorName = indexOf(pv.ColorIndex), palette.At(pv.ColorIndex).Name
				}
			}
			groups[key] = g
			order = append(order, key)
		}
		detail := ""
		if p, err := api.ParsePayload(req.Kind, req.Payload); err == nil {
			switch {
			case p.Email != nil:
				detail = fmt.Sprintf("%d recipients", len(p.Email.Recipients))
			case p.Batch != nil:
				detail = fmt.Sprintf("%d messages", len(p.Batch.Items))
			case p.Trust != nil:
				detail = fmt.Sprintf("%d addresses", len(p.Trust.Changes))
			case p.Dashboard != nil:
				detail = p.Dashboard.ClientName
			}
		}
		g.Rows = append(g.Rows, queueRow{
			Label: kindLabels[req.Kind], Detail: detail, URL: "/approve/r/" + req.ID, Age: since(now, req.Created) + " old",
		})
	}
	sort.SliceStable(order, func(i, j int) bool { return (order[i] == "") != (order[j] == "") && order[j] == "" })
	v := pageView{Title: "Approval queue", Heading: "Approval queue", Body: "queue"}
	for n, k := range order {
		groups[k].N = n
		v.Groups = append(v.Groups, *groups[k])
	}
	s.writePage(w, http.StatusOK, v)
}
