// Package flowtest is an in-memory stand-in for moose-approver's service API,
// for tests of the send engine and the tools. It signs real receipts with a
// local P-256 key, so the engine verifies them exactly as it would in
// production, and it mimics the approver's idempotent create, claimant-bound
// consume and batch approval with per-item drops.
package flowtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
)

// RPID is the relying party id the fake signs into receipts.
const RPID = "moose.example.test"

type req struct {
	id       string
	kind     string
	key      string
	payload  []byte
	sha      string
	status   string
	created  time.Time
	expires  time.Time
	approved time.Time
	rcpt     *receipt.Receipt
	batchID  string
	claimant string
	preview  []byte
}

// Approver is the fake. The zero value is not usable; call New.
type Approver struct {
	mu     sync.Mutex
	signer *receipt.LocalSigner
	reqs   map[string]*req
	byKey  map[string]string
	n      int
	// Now is the fake's clock. Tests move it to expire requests.
	Now func() time.Time
	// Down makes every call fail, as an unreachable approver would.
	Down bool
	// Creates counts accepted create calls that made a new request.
	Creates int
	// ConsumeCalls counts consume calls.
	ConsumeCalls int
}

// New returns a fake with a fresh signing key.
func New() *Approver {
	s, err := receipt.GenerateLocalSigner()
	if err != nil {
		panic(err)
	}
	return &Approver{signer: s, reqs: map[string]*req{}, byKey: map[string]string{}, Now: time.Now}
}

// PublicKeyPEM is the key receipts verify against.
func (a *Approver) PublicKeyPEM() []byte {
	pem, err := a.signer.PublicKeyPEM()
	if err != nil {
		panic(err)
	}
	return pem
}

var errDown = &api.Error{Status: http.StatusBadGateway, Code: "down", Message: "approver unreachable"}

// Create implements sendflow.Approver.
func (a *Approver) Create(_ context.Context, in api.CreateBody) (api.CreateResponse, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Down {
		return api.CreateResponse{}, false, errDown
	}
	p, err := api.ParsePayload(in.Kind, in.Payload)
	if err != nil {
		return api.CreateResponse{}, false, &api.Error{Status: http.StatusUnprocessableEntity, Code: "invalid_payload", Message: err.Error()}
	}
	if id, ok := a.byKey[in.IdempotencyKey]; ok {
		r := a.reqs[id]
		if r.sha != p.SHA256 && r.batchID == "" {
			return api.CreateResponse{}, false, &api.Error{Status: http.StatusConflict, Code: "idempotency_conflict"}
		}
		return a.resp(r), false, nil
	}
	if p.Batch != nil {
		for _, it := range p.Batch.Items {
			c, ok := a.reqs[it.RequestID]
			if !ok || c.kind != api.KindEmailSend {
				return api.CreateResponse{}, false, &api.Error{Status: http.StatusUnprocessableEntity, Code: "invalid_batch", Message: "unknown item " + it.RequestID}
			}
			cp, _ := api.ParsePayload(api.KindEmailSend, c.payload)
			if cp.AccountID != p.AccountID {
				return api.CreateResponse{}, false, &api.Error{Status: http.StatusUnprocessableEntity, Code: "cross_account_batch", Message: "a batch holds sends from one account only"}
			}
		}
	}
	a.n++
	now := a.Now().UTC()
	r := &req{id: fmt.Sprintf("req-%d", a.n), kind: in.Kind, key: in.IdempotencyKey, payload: p.Canonical, sha: p.SHA256,
		status: "pending", created: now, expires: now.Add(15 * time.Minute), preview: in.Preview}
	a.reqs[r.id] = r
	a.byKey[in.IdempotencyKey] = r.id
	a.Creates++
	return a.resp(r), true, nil
}

func (a *Approver) resp(r *req) api.CreateResponse {
	return api.CreateResponse{RequestID: r.id, PayloadSHA256: r.sha, ApprovalURL: "https://" + RPID + "/approve/r/" + r.id, ExpiresAt: r.expires}
}

// fresh applies expiry to a pending request.
func (a *Approver) fresh(r *req) {
	if r.status == "pending" && !a.Now().Before(r.expires) {
		r.status = "expired"
	}
}

func (a *Approver) status(r *req) api.StatusResponse {
	a.fresh(r)
	out := api.StatusResponse{RequestID: r.id, Kind: r.kind, Status: r.status, PayloadSHA256: r.sha, ExpiresAt: r.expires, BatchID: r.batchID}
	if r.status != "approved" && r.status != "consumed" {
		return out
	}
	out.Receipt = r.rcpt
	if r.rcpt == nil && r.batchID != "" {
		out.Receipt = a.reqs[r.batchID].rcpt
	}
	if r.kind == api.KindEmailSendBatch {
		out.Payload = json.RawMessage(r.payload)
	}
	return out
}

// Status implements sendflow.Approver.
func (a *Approver) Status(_ context.Context, id string) (api.StatusResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Down {
		return api.StatusResponse{}, errDown
	}
	r, ok := a.reqs[id]
	if !ok {
		return api.StatusResponse{}, &api.Error{Status: http.StatusNotFound, Code: "not_found"}
	}
	return a.status(r), nil
}

// Consume implements sendflow.Approver with the approver's claimant rule: the
// same claimant may consume again, another claimant gets ErrAlreadyConsumed.
func (a *Approver) Consume(_ context.Context, id, claimant string) (api.StatusResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ConsumeCalls++
	if a.Down {
		return api.StatusResponse{}, errDown
	}
	r, ok := a.reqs[id]
	if !ok {
		return api.StatusResponse{}, &api.Error{Status: http.StatusNotFound, Code: "not_found"}
	}
	a.fresh(r)
	switch r.status {
	case "consumed":
		if claimant != "" && r.claimant == claimant {
			return a.status(r), nil
		}
		return api.StatusResponse{}, api.ErrAlreadyConsumed
	case "approved":
		r.status, r.claimant = "consumed", claimant
		return a.status(r), nil
	case "expired":
		return api.StatusResponse{}, &api.Error{Status: http.StatusGone, Code: "expired"}
	}
	return api.StatusResponse{}, &api.Error{Status: http.StatusConflict, Code: "not_approved", Message: "request is " + r.status}
}

func (a *Approver) sign(r *req, kind, sha string) *receipt.Receipt {
	now := a.Now().UTC()
	rc, err := receipt.Sign(context.Background(), a.signer, receipt.Body{
		RequestID: r.id, Kind: kind, PayloadSHA256: sha, ApprovedAt: now, ExpiresAt: now.Add(15 * time.Minute), RPID: RPID,
	})
	if err != nil {
		panic(err)
	}
	return &rc
}

// Approve approves a pending request, as a passkey tap would, and returns its
// receipt.
func (a *Approver) Approve(id string) receipt.Receipt {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.must(id)
	a.fresh(r)
	if r.status != "pending" {
		panic("flowtest: approve " + id + " in status " + r.status)
	}
	r.status, r.approved = "approved", a.Now().UTC()
	r.rcpt = a.sign(r, r.kind, r.sha)
	return *r.rcpt
}

// ApproveBatch approves a batch and the items it keeps; dropped items are
// denied. The batch payload and hash are rewritten to the kept list, and the
// receipt covers that list.
func (a *Approver) ApproveBatch(id string, drop ...string) receipt.Receipt {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.must(id)
	p, err := api.ParsePayload(api.KindEmailSendBatch, b.payload)
	if err != nil || p.Batch == nil {
		panic("flowtest: " + id + " is not a batch")
	}
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	kept := p.Batch.Items[:0:0]
	for _, it := range p.Batch.Items {
		item := a.must(it.RequestID)
		if dropped[it.RequestID] {
			item.status, item.batchID = "denied", id
			continue
		}
		item.status, item.batchID, item.approved = "approved", id, a.Now().UTC()
		kept = append(kept, it)
	}
	p.Batch.Items = kept
	raw, _ := json.Marshal(p.Batch)
	canon, _ := api.Canonical(raw)
	b.payload, b.sha = canon, api.SHA256(canon)
	b.status, b.approved = "approved", a.Now().UTC()
	b.rcpt = a.sign(b, api.KindEmailSendBatch, b.sha)
	return *b.rcpt
}

// Deny denies a pending request.
func (a *Approver) Deny(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.must(id).status = "denied"
}

// Tamper replaces the receipt returned for id, to test receipt substitution.
func (a *Approver) Tamper(id string, rc receipt.Receipt) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.must(id).rcpt = &rc
}

// Receipt returns the receipt held for id.
func (a *Approver) Receipt(id string) receipt.Receipt {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.must(id)
	if r.rcpt == nil {
		panic("flowtest: no receipt for " + id)
	}
	return *r.rcpt
}

// SignFor signs a receipt for any request id, kind and payload hash, the way a
// stolen or misdirected receipt would look.
func (a *Approver) SignFor(id, kind, sha string, approvedAt, expiresAt time.Time) receipt.Receipt {
	rc, err := receipt.Sign(context.Background(), a.signer, receipt.Body{
		RequestID: id, Kind: kind, PayloadSHA256: sha, ApprovedAt: approvedAt, ExpiresAt: expiresAt, RPID: RPID,
	})
	if err != nil {
		panic(err)
	}
	return rc
}

// Request reports a request's kind, payload hash and status.
func (a *Approver) Request(id string) (kind, sha, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.must(id)
	a.fresh(r)
	return r.kind, r.sha, r.status
}

// Preview returns the preview the engine asked the approver to show for id.
func (a *Approver) Preview(id string) api.Preview {
	a.mu.Lock()
	defer a.mu.Unlock()
	var pv api.Preview
	if err := json.Unmarshal(a.must(id).preview, &pv); err != nil {
		panic(err)
	}
	return pv
}

// Claimant reports who consumed a request.
func (a *Approver) Claimant(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.must(id).claimant
}

func (a *Approver) must(id string) *req {
	r, ok := a.reqs[id]
	if !ok {
		panic("flowtest: unknown request " + id)
	}
	return r
}
