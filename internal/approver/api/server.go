package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
)

// Prefix is where Handler is mounted.
const Prefix = "/approve/api/"

// HMAC headers (decision 29). The MAC covers the method, the request URI, the
// timestamp and the sha256 of the body; a timestamp more than MaxSkew from the
// server clock is refused.
const (
	HeaderTimestamp = "X-Moose-Timestamp"
	HeaderSignature = "X-Moose-Signature"
	MaxSkew         = 5 * time.Minute
	maxBody         = 1 << 20
	// maxPreview keeps a sealed preview under DynamoDB's 400 KB item limit.
	maxPreview = 350_000
)

// Sign returns the hex HMAC-SHA256 for a request.
func Sign(secret []byte, method, requestURI string, ts int64, body []byte) string {
	bh := sha256.Sum256(body)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(method + "\n" + requestURI + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(bh[:])))
	return hex.EncodeToString(m.Sum(nil))
}

// StatusResponse is the answer to a status or consume call. Receipt is present
// only when Status is approved or consumed. An item approved through a batch
// has no receipt of its own: it returns the batch's receipt with BatchID, and
// the verifier checks that against the batch request.
type StatusResponse struct {
	RequestID     string           `json:"request_id"`
	Kind          string           `json:"kind"`
	Status        string           `json:"status"`
	PayloadSHA256 string           `json:"payload_sha256"`
	ExpiresAt     time.Time        `json:"expires_at"`
	BatchID       string           `json:"batch_id,omitempty"`
	Receipt       *receipt.Receipt `json:"receipt,omitempty"`
	// Payload is returned for a batch, whose approved item list may be
	// shorter than the one it was created with.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Server serves the service API.
type Server struct {
	Store   *store.Store
	Owner   string
	BaseURL string // public origin, for approval URLs
	Secret  []byte
	Now     func() time.Time
	Log     *slog.Logger
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler serves /approve/api/*. It returns 401 for a missing or bad MAC or a
// timestamp outside the skew window.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Prefix+"requests", s.create)
	mux.HandleFunc("GET "+Prefix+"requests/{id}", s.status)
	mux.HandleFunc("POST "+Prefix+"requests/{id}/consume", s.consume)
	return s.authenticate(mux)
}

type apiError struct {
	Code    string `json:"error"`
	Message string `json:"message,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, apiError{errCode, msg})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			fail(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
			return
		}
		ts, err := strconv.ParseInt(r.Header.Get(HeaderTimestamp), 10, 64)
		if err != nil || len(s.Secret) == 0 {
			fail(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		if d := s.now().Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
			fail(w, http.StatusUnauthorized, "unauthorized", "timestamp outside the allowed window")
			return
		}
		want := Sign(s.Secret, r.Method, r.URL.RequestURI(), ts, body)
		if !hmac.Equal([]byte(want), []byte(r.Header.Get(HeaderSignature))) {
			fail(w, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) approvalURL(id string) string {
	return strings.TrimRight(s.BaseURL, "/") + "/approve/r/" + id
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in CreateBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "body is not valid JSON")
		return
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > 200 {
		fail(w, http.StatusBadRequest, "bad_request", "idempotency_key is required (at most 200 characters)")
		return
	}
	p, err := ParsePayload(in.Kind, in.Payload)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "invalid_payload", err.Error())
		return
	}
	if p.Owner != s.Owner {
		fail(w, http.StatusUnprocessableEntity, "invalid_payload", "owner does not match this approver")
		return
	}
	var pv Preview
	if len(in.Preview) > 0 && string(in.Preview) != "null" {
		if err := json.Unmarshal(in.Preview, &pv); err != nil {
			fail(w, http.StatusUnprocessableEntity, "invalid_preview", "preview does not fit the preview schema")
			return
		}
	}
	if err := checkPreview(p, pv); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.code, err.msg)
		return
	}
	if p.Batch != nil {
		if err := s.checkBatch(r, p); err != nil {
			fail(w, http.StatusUnprocessableEntity, err.code, err.msg)
			return
		}
	}
	rawPreview, _ := json.Marshal(pv)
	if len(rawPreview) > maxPreview {
		fail(w, http.StatusUnprocessableEntity, "preview_too_large", "the preview is too large to store")
		return
	}
	now := s.now()
	req := store.Request{
		ID: store.NewRequestID(), Owner: p.Owner, Kind: p.Kind, IdempotencyKey: in.IdempotencyKey,
		AccountID: p.AccountID, FromAddress: p.From, PayloadSHA256: p.SHA256, Payload: p.Canonical,
		Created: now.UTC(), Expires: now.Add(store.RequestLifetime).UTC(),
	}
	got, created, err := s.Store.CreateRequest(r.Context(), req, rawPreview)
	switch {
	case errors.Is(err, store.ErrIdempotencyReuse):
		fail(w, http.StatusConflict, "idempotency_conflict", "that idempotency_key was used with a different payload")
		return
	case err != nil:
		s.logf("create request", err)
		fail(w, http.StatusInternalServerError, "internal", "")
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, CreateResponse{RequestID: got.ID, PayloadSHA256: got.OrigPayloadSHA256, ApprovalURL: s.approvalURL(got.ID), ExpiresAt: got.Expires})
}

type check struct{ code, msg string }

// checkPreview cross-checks the preview against the signed payload, so what the
// page shows cannot disagree with what the receipt covers.
func checkPreview(p Parsed, pv Preview) *check {
	if !FromKinds(p.Kind) {
		return nil
	}
	if pv.AccountID != p.AccountID || pv.FromAddress != p.From {
		return &check{"preview_mismatch", "preview account_id and from_address must equal the payload's"}
	}
	if strings.TrimSpace(pv.Nickname) == "" {
		return &check{"invalid_preview", "preview nickname is required"}
	}
	if SendKinds(p.Kind) && (pv.AccountStatus != "ok" || pv.SendEnabled == nil || !*pv.SendEnabled) {
		return &check{"account_not_ready", "the account must be status ok with send enabled"}
	}
	if InviteKinds(p.Kind) && (pv.AccountStatus != "ok" || pv.InviteEnabled == nil || !*pv.InviteEnabled || pv.CalendarGranted == nil || !*pv.CalendarGranted) {
		return &check{"account_not_ready", "the account must be status ok with invites enabled and calendar granted"}
	}
	if p.Kind == KindAccountInviteEnable && (pv.CalendarGranted == nil || !*pv.CalendarGranted) {
		return &check{"account_not_ready", "the account must have granted calendar before invites can be enabled"}
	}
	if pv.AccountStatus != "" && pv.AccountStatus != "ok" {
		return &check{"account_not_ready", "the account must be status ok"}
	}
	if p.Kind == KindEmailSend {
		m := pv.Message
		if m == nil {
			return &check{"invalid_preview", "an email-send preview needs the message"}
		}
		if !strings.EqualFold(strings.TrimSpace(m.From), p.From) {
			return &check{"preview_mismatch", "preview From header must equal from_address"}
		}
		if !sameSet(m.Addresses(), p.Email.Recipients) {
			return &check{"preview_mismatch", "preview recipients must equal the payload's recipients"}
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	norm := func(l []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range l {
			m[strings.ToLower(strings.TrimSpace(x))] = true
		}
		return m
	}
	ma, mb := norm(a), norm(b)
	if len(ma) != len(mb) {
		return false
	}
	for k := range ma {
		if !mb[k] {
			return false
		}
	}
	return true
}

// checkBatch requires every item to be a pending email-send request of the
// batch's own account: a batch holds sends from one account only (R8).
func (s *Server) checkBatch(r *http.Request, p Parsed) *check {
	for _, it := range p.Batch.Items {
		c, err := s.Store.Fresh(r.Context(), it.RequestID, s.now())
		if err != nil {
			return &check{"invalid_batch", "batch item " + it.RequestID + " is unknown"}
		}
		switch {
		case c.Owner != p.Owner || c.Kind != KindEmailSend:
			return &check{"invalid_batch", "batch item " + it.RequestID + " is not an email-send request"}
		case c.AccountID != p.AccountID || c.FromAddress != p.From:
			return &check{"cross_account_batch", "a batch holds sends from one account only; " + it.RequestID + " is from another"}
		case c.Status != store.StatusPending:
			return &check{"invalid_batch", "batch item " + it.RequestID + " is " + c.Status}
		case c.PayloadSHA256 != it.PayloadSHA256:
			return &check{"invalid_batch", "batch item " + it.RequestID + " has a different payload_sha256"}
		}
	}
	return nil
}

func (s *Server) logf(msg string, err error) {
	if s.Log != nil {
		s.Log.Error("approver api: "+msg, "err", err)
	}
}

func (s *Server) respond(r store.Request, req *http.Request) StatusResponse {
	out := StatusResponse{RequestID: r.ID, Kind: r.Kind, Status: r.Status, PayloadSHA256: r.PayloadSHA256, ExpiresAt: r.Expires, BatchID: r.BatchID}
	if r.Status != store.StatusApproved && r.Status != store.StatusConsumed {
		return out
	}
	out.Receipt = r.Receipt
	if r.Receipt == nil && r.BatchID != "" {
		if b, err := s.Store.GetRequest(req.Context(), r.BatchID); err == nil {
			out.Receipt = b.Receipt
		}
	}
	if r.Kind == KindEmailSendBatch {
		out.Payload = r.Payload
	}
	return out
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	req, err := s.Store.Fresh(r.Context(), r.PathValue("id"), s.now())
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "not_found", "")
		return
	}
	if err != nil {
		s.logf("status", err)
		fail(w, http.StatusInternalServerError, "internal", "")
		return
	}
	writeJSON(w, http.StatusOK, s.respond(req, r))
}

func (s *Server) consume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Claimant string `json:"claimant"`
	}
	if body, _ := io.ReadAll(r.Body); len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			fail(w, http.StatusBadRequest, "bad_request", "body is not valid JSON")
			return
		}
	}
	id := r.PathValue("id")
	if _, err := s.Store.Fresh(r.Context(), id, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, http.StatusNotFound, "not_found", "")
			return
		}
		fail(w, http.StatusInternalServerError, "internal", "")
		return
	}
	req, err := s.Store.Consume(r.Context(), id, in.Claimant, s.now())
	switch {
	case errors.Is(err, store.ErrAlreadyConsumed):
		fail(w, http.StatusConflict, "already_consumed", "")
	case errors.Is(err, store.ErrInvalidTransition):
		fail(w, http.StatusConflict, "not_approved", "request is "+req.Status)
	case errors.Is(err, store.ErrExpired):
		fail(w, http.StatusGone, "expired", "the approval has lapsed")
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, "not_found", "")
	case err != nil:
		s.logf("consume", err)
		fail(w, http.StatusInternalServerError, "internal", "")
	default:
		writeJSON(w, http.StatusOK, s.respond(req, r))
	}
}
