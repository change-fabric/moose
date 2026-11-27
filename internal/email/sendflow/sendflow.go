// Package sendflow is moose-email's approved-send engine: it snapshots a draft,
// asks the approver for a passkey approval on the exact bytes, and later sends
// those stored bytes once, and only once, after the approval's receipt verifies.
//
// A send request runs in this order (Phase 14):
//
//	request  normalize to one plain-text part, read the draft's RFC822 (at most
//	         256 KB), seal the bytes with the account's envelope, parse the
//	         preview from those same bytes, check from_address, send_enabled,
//	         soft flags, the hourly cap and the kill switch, then create the
//	         approver request keyed by sha256(account, draft, rfc822 sha).
//	dispatch verify the receipt against the stored request, recheck the kill
//	         switch and the account, claim the send ledger row and an hourly cap
//	         slot in one transaction, consume the approval, and, only when
//	         email.send.live is true, call messages.send with the stored bytes.
//
// The ledger row moves claimed, dispatching, then one of sent, failed, unknown
// or would_send, each step a conditional write on the previous state, so a
// retry, a concurrent call or a crash can never send twice.
package sendflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
)

// Approver is the part of the approver's service API the engine uses. The HMAC
// client in internal/approver/api implements it.
type Approver interface {
	Create(ctx context.Context, in api.CreateBody) (resp api.CreateResponse, created bool, err error)
	Status(ctx context.Context, id string) (api.StatusResponse, error)
	Consume(ctx context.Context, id, claimant string) (api.StatusResponse, error)
}

// Providers builds the mailbox provider for a stored account.
type Providers interface {
	Provider(ctx context.Context, a store.Account) (provider.Provider, error)
}

// Defaults for the timing knobs on Service.
const (
	DefaultDispatchTimeout = 20 * time.Second
	DefaultStaleAfter      = 90 * time.Second
	DefaultBatchBudget     = 18 * time.Second
	maxSenderLookups       = 10
	toolSendStatus         = "email_send_status"
	manyRecipients         = 10
	maxRequestAttempts     = 5
)

// Service is the send engine. Its zero fields take defaults except Store,
// Seal, Approver and Providers, which are required.
type Service struct {
	Store     store.Store
	Seal      envelope.Sealer
	Approver  Approver
	Providers Providers
	// ReceiptKey is the pinned PEM public key receipts are verified against.
	ReceiptKey []byte
	// RPID is the relying party id every receipt must carry.
	RPID string
	// Live is email.send.live. While false, a fully approved send ends in
	// would_send and nothing leaves moose.
	Live bool
	Now  func() time.Time
	Log  *slog.Logger
	// DispatchTimeout bounds the provider call. A ledger row left in
	// dispatching for StaleAfter is judged to belong to a crashed call and
	// becomes unknown.
	DispatchTimeout time.Duration
	StaleAfter      time.Duration
	// BatchBudget is how long one status call spends dispatching a batch's
	// items before it returns and leaves the rest for the next call.
	BatchBudget time.Duration
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) dispatchTimeout() time.Duration {
	if s.DispatchTimeout > 0 {
		return s.DispatchTimeout
	}
	return DefaultDispatchTimeout
}

func (s *Service) staleAfter() time.Duration {
	if s.StaleAfter > 0 {
		return s.StaleAfter
	}
	return DefaultStaleAfter
}

func (s *Service) batchBudget() time.Duration {
	if s.BatchBudget > 0 {
		return s.BatchBudget
	}
	return DefaultBatchBudget
}

func (s *Service) logf(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn("sendflow: "+msg, args...)
	}
}

// Error is a refusal with a stable code and a message written for the caller.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Error codes.
const (
	CodeInvalid         = "invalid_input"
	CodeFromMismatch    = "from_mismatch"
	CodeSendDisabled    = "send_disabled"
	CodeAccountNotReady = "account_unavailable"
	CodeKillSwitch      = "kill_switch"
	CodeRateLimited     = "rate_limited"
	CodeDraftNotFound   = "draft_not_found"
	CodeDraftTooLarge   = "draft_too_large"
	CodeNotSendable     = "draft_not_sendable"
	CodeBlockedRecip    = "recipient_blocked"
	CodeApprover        = "approver_unavailable"
	CodeNotFound        = "not_found"
	CodeInternal        = "internal"
	CodeReceipt         = "receipt_invalid"
	CodeProvider        = "provider_error"
)

// Item is one draft's send request.
type Item struct {
	RequestID string `json:"request_id"`
	DraftID   string `json:"draft_id"`
	// Status is pending (waiting for the passkey approval) or, when the same
	// draft bytes were requested before, the earlier request's state.
	Status      string         `json:"status"`
	ApprovalURL string         `json:"approval_url,omitempty"`
	ExpiresAt   *time.Time     `json:"expires_at,omitempty"`
	Subject     string         `json:"subject"`
	Recipients  []string       `json:"recipients"`
	SoftFlags   []api.SoftFlag `json:"soft_flags"`
	NewContacts []string       `json:"new_recipients,omitempty"`
}

// RequestIn is what RequestSend needs. DraftIDs are provider draft ids of
// Account's own mailbox.
type RequestIn struct {
	Account     store.Account
	FromAddress string
	DraftIDs    []string
}

// RequestOut is the answer to RequestSend. A batch has its own request and
// approval URL, and Items lists the per-message requests inside it.
type RequestOut struct {
	Status      string `json:"status"`
	RequestID   string `json:"request_id"`
	ApprovalURL string `json:"approval_url,omitempty"`
	Batch       bool   `json:"batch"`
	Items       []Item `json:"items"`
}

// CheckSendable refuses a send for an account that is not ready: the from
// address must match the account exactly, the account must be ok and have send
// enabled, and the kill switch must be off. It is used at request time and
// again at dispatch.
func (s *Service) CheckSendable(ctx context.Context, a store.Account, from string) *Error {
	if !FromMatches(a, from) {
		return errf(CodeFromMismatch, "from_address %q is not the address of account %s (%s); restate the account's exact address", from, a.ID, a.Address)
	}
	return s.blocked(ctx, a)
}

// FromMatches reports whether from is exactly the account's address. Case is
// ignored, because mailbox addresses are not case sensitive in practice; a
// nickname, display name or any other text never matches.
func FromMatches(a store.Account, from string) bool {
	return strings.TrimSpace(from) != "" && strings.EqualFold(strings.TrimSpace(from), a.Address)
}

func (s *Service) blocked(ctx context.Context, a store.Account) *Error {
	switch {
	case a.Status != store.StatusOK:
		return errf(CodeAccountNotReady, "%s is %s; it cannot send until it is reconnected", a.Address, strings.ReplaceAll(a.Status, "_", " "))
	case !a.SendEnabled:
		return errf(CodeSendDisabled, "sending is not enabled for %s; call email_account_enable_send and approve it with your passkey first", a.Address)
	}
	ks, err := s.Store.KillSwitch(ctx)
	if err != nil {
		return errf(CodeInternal, "could not read the send kill switch")
	}
	if ks.On {
		msg := "the send kill switch is on, so nothing is sent"
		if ks.Reason != "" {
			msg += " (" + ks.Reason + ")"
		}
		return errf(CodeKillSwitch, "%s", msg)
	}
	return nil
}

// RequestSend creates the approver request for one draft, or one batch of
// drafts from the same account.
func (s *Service) RequestSend(ctx context.Context, in RequestIn) (RequestOut, error) {
	a := in.Account
	if len(in.DraftIDs) == 0 {
		return RequestOut{}, errf(CodeInvalid, "name the draft to send")
	}
	if len(in.DraftIDs) > api.MaxBatchItems {
		return RequestOut{}, errf(CodeInvalid, "a batch holds at most %d drafts", api.MaxBatchItems)
	}
	if e := s.CheckSendable(ctx, a, in.FromAddress); e != nil {
		return RequestOut{}, e
	}
	if n, err := s.Store.HourlyCount(ctx, a.ID, s.now()); err != nil {
		return RequestOut{}, errf(CodeInternal, "could not read the send counter")
	} else if n >= a.HourlyCap {
		return RequestOut{}, errf(CodeRateLimited, "%s has reached its hourly send cap of %d", a.Address, a.HourlyCap)
	}
	p, err := s.Providers.Provider(ctx, a)
	if err != nil {
		return RequestOut{}, errf(CodeProvider, "could not open the mailbox for %s", a.Address)
	}
	seen := map[string]bool{}
	var items []Item
	for _, id := range in.DraftIDs {
		if seen[id] {
			return RequestOut{}, errf(CodeInvalid, "draft %s is listed twice", id)
		}
		seen[id] = true
		it, err := s.requestOne(ctx, a, p, id)
		if err != nil {
			return RequestOut{}, err
		}
		items = append(items, it)
	}
	if len(items) == 1 {
		return RequestOut{Status: items[0].Status, RequestID: items[0].RequestID, ApprovalURL: items[0].ApprovalURL, Items: items}, nil
	}
	return s.requestBatch(ctx, a, items)
}

// requestOne runs steps 1 to 8 of email_request_send for one draft.
func (s *Service) requestOne(ctx context.Context, a store.Account, p provider.Provider, draftID string) (Item, error) {
	raw, err := p.GetDraftRaw(ctx, draftID)
	switch {
	case errors.Is(err, provider.ErrDraftTooLarge):
		return Item{}, errf(CodeDraftTooLarge, "draft %s is larger than %d KB; shorten it", draftID, provider.MaxDraftRaw>>10)
	case errors.Is(err, provider.ErrNotFound):
		return Item{}, errf(CodeDraftNotFound, "draft %s does not exist in %s", draftID, a.Address)
	case errors.Is(err, provider.ErrAuth):
		return Item{}, errf("needs_reauth", "%s needs to be reconnected on the moose accounts page", a.Address)
	case err != nil:
		return Item{}, errf(CodeProvider, "the draft could not be read from the mailbox")
	}
	if len(raw) > mailmsg.MaxRaw {
		return Item{}, errf(CodeDraftTooLarge, "draft %s is larger than %d KB; shorten it", draftID, mailmsg.MaxRaw>>10)
	}
	parsed, err := mailmsg.Parse(raw)
	if err != nil {
		var pr *mailmsg.Problem
		if errors.As(err, &pr) {
			return Item{}, errf(CodeNotSendable, "%s", pr.Msg)
		}
		return Item{}, errf(CodeNotSendable, "the draft could not be read")
	}
	if !strings.EqualFold(parsed.From.Address, a.Address) {
		return Item{}, errf(CodeFromMismatch, "the draft's From header (%s) is not the account's address (%s); update the draft", parsed.From.Address, a.Address)
	}
	sum := sha256.Sum256(raw)
	rawSHA := hex.EncodeToString(sum[:])
	meta, err := s.Store.GetDraftMeta(ctx, a.ID, draftID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Item{}, errf(CodeInternal, "could not read the draft notes")
	}

	recips := parsed.Recipients()
	flags, trustChanges, err := s.policy(ctx, a, p, parsed, recips, meta)
	if err != nil {
		return Item{}, err
	}
	payload := api.EmailSendPayload{
		Owner: a.Owner, AccountID: a.ID, FromAddress: a.Address, DraftID: draftID, RFC822SHA256: rawSHA,
		Recipients: recips, SoftFlags: flags, TrustChanges: trustChanges,
	}
	canon, err := canonicalOf(payload)
	if err != nil {
		return Item{}, errf(CodeInternal, "could not encode the request")
	}
	wantSHA := api.SHA256(canon)
	preview, err := s.preview(ctx, a, parsed, "Send this message")
	if err != nil {
		return Item{}, err
	}
	rawPreview, _ := json.Marshal(preview)

	for attempt := 0; attempt < maxRequestAttempts; attempt++ {
		key := idemKey(a.ID, draftID, rawSHA, attempt)
		base := Item{DraftID: draftID, Subject: parsed.Subject, Recipients: recips, SoftFlags: flags}
		// The same draft bytes may have been requested before. Reuse that
		// request while it is alive, whatever the recipients' trust has become
		// since; a denied or expired one needs a fresh key.
		if prior, err := s.Store.FindApprovalByKey(ctx, key); err == nil {
			state, dead := s.earlier(ctx, prior)
			if dead {
				continue
			}
			base.RequestID, base.Status = prior.ID, state
			if state == "pending" || state == "approved" {
				base.ApprovalURL = prior.ApprovalURL
				exp := prior.Expires
				base.ExpiresAt = &exp
			}
			return base, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return Item{}, errf(CodeInternal, "could not look up earlier requests")
		}
		resp, created, err := s.Approver.Create(ctx, api.CreateBody{IdempotencyKey: key, Kind: api.KindEmailSend, Payload: canon, Preview: rawPreview})
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == "idempotency_conflict" {
			continue // the approver holds this key for a request moose never recorded
		}
		if err != nil {
			s.logf("create request", "err", err.Error())
			return Item{}, errf(CodeApprover, "the approval service could not create the request: %s", describe(err))
		}
		if resp.PayloadSHA256 != wantSHA {
			return Item{}, errf(CodeInternal, "the approver hashed a different payload than moose sent")
		}
		item := base
		item.RequestID, item.Status, item.ApprovalURL, item.ExpiresAt = resp.RequestID, "pending", resp.ApprovalURL, &resp.ExpiresAt
		for _, c := range trustChanges {
			item.NewContacts = append(item.NewContacts, c.Address)
		}
		snapshot, err := s.Seal.Seal(ctx, raw, envelope.Context{Owner: a.Owner, AccountID: a.ID})
		if err != nil {
			return Item{}, errf(CodeInternal, "could not seal the message snapshot")
		}
		_, _, err = s.Store.CreateApproval(ctx, store.Approval{
			ID: resp.RequestID, Owner: a.Owner, Kind: store.KindEmailSend, AccountID: a.ID, IdempotencyKey: key,
			PayloadSHA256: wantSHA, Payload: canon, ApprovalURL: resp.ApprovalURL, DraftID: draftID, RFC822SHA256: rawSHA,
			Snapshot: snapshot, ThreadID: meta.ThreadID, Status: store.ApprovalPending, Created: s.now(), Expires: resp.ExpiresAt,
		}, s.now())
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return Item{}, errf(CodeInternal, "could not record the request")
		}
		if created {
			s.audit(ctx, a, "email_request_send", resp.RequestID, recips, rawSHA, "requested")
		}
		return item, nil
	}
	return Item{}, errf(CodeInternal, "too many earlier requests for this draft were denied or expired; edit the draft and try again")
}

// earlier classifies a request moose already holds for this key: its current
// state, and whether it is dead (denied or expired) so a new request is needed.
func (s *Service) earlier(ctx context.Context, prior store.Approval) (state string, dead bool) {
	if row, err := s.Store.GetSend(ctx, prior.ID); err == nil {
		return row.State, false
	}
	switch prior.Status {
	case store.ApprovalDenied, store.ApprovalExpired:
		return prior.Status, true
	}
	st, err := s.Approver.Status(ctx, prior.ID)
	if err != nil {
		return "pending", false
	}
	switch st.Status {
	case "denied", "expired":
		return st.Status, true
	case "pending":
		if !s.now().Before(prior.Expires) {
			return "expired", true
		}
	}
	return st.Status, false
}

func idemKey(accountID, draftID, rawSHA string, attempt int) string {
	s := accountID + "\x00" + draftID + "\x00" + rawSHA
	if attempt > 0 {
		s += fmt.Sprintf("\x00attempt-%d", attempt)
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func describe(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Message != "" {
		return ae.Message
	}
	return "it did not answer"
}

func canonicalOf(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return api.Canonical(b)
}

// policy evaluates the recipient rules: blocked recipients are refused, and new
// recipients, external domains, bcc, large recipient lists and a reply from a
// different account become soft flags the approver shows. Every new recipient
// also becomes a trust change enrolled by the approval.
func (s *Service) policy(ctx context.Context, a store.Account, p provider.Provider, m mailmsg.Parsed, recips []string, meta store.DraftMeta) ([]api.SoftFlag, []api.TrustChange, error) {
	flags := []api.SoftFlag{}
	changes := []api.TrustChange{}
	own := domainOf(a.Address)
	bcc := map[string]bool{}
	for _, b := range m.Bcc {
		bcc[strings.ToLower(b.Address)] = true
	}
	lookups := 0
	for _, addr := range recips {
		if strings.EqualFold(addr, a.Address) {
			continue
		}
		trust, err := s.trustOf(ctx, a, p, addr, &lookups)
		if err != nil {
			return nil, nil, errf(CodeInternal, "could not read the contact list")
		}
		switch trust {
		case store.TrustBlocked:
			return nil, nil, errf(CodeBlockedRecip, "%s is blocked for %s; remove them from the draft", addr, a.Address)
		case store.TrustNew:
			flags = append(flags, api.SoftFlag{Flag: "new_recipient", Address: addr})
			changes = append(changes, api.TrustChange{Address: addr, To: "trusted"})
		}
		if domainOf(addr) != own {
			flags = append(flags, api.SoftFlag{Flag: "external_domain", Address: addr})
		}
		if bcc[addr] {
			flags = append(flags, api.SoftFlag{Flag: "bcc", Address: addr})
		}
	}
	if len(recips) > manyRecipients {
		flags = append(flags, api.SoftFlag{Flag: "many_recipients"})
	}
	if meta.ReplyAccountID != "" && meta.ReplyAccountID != a.ID {
		flags = append(flags, api.SoftFlag{Flag: "reply_account_differs"})
	}
	return flags, changes, nil
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// trustOf is the recipient's trust in this account (contacts are per account).
// An address with no contact row is known when the account has written to it
// before (one Sent search, at most maxSenderLookups per request), else new.
func (s *Service) trustOf(ctx context.Context, a store.Account, p provider.Provider, addr string, lookups *int) (string, error) {
	c, err := s.Store.GetContact(ctx, a.ID, addr)
	if err == nil {
		return c.Trust, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if p != nil && *lookups < maxSenderLookups {
		*lookups++
		res, err := p.Search(ctx, provider.Query{To: addr, Mailbox: provider.MailboxSent, Limit: 1})
		if err == nil && len(res.Messages) > 0 {
			return store.TrustKnown, nil
		}
	}
	return store.TrustNew, nil
}

// preview builds the approver preview. Every message field is parsed from the
// stored bytes; the banner fields come from the account.
func (s *Service) preview(ctx context.Context, a store.Account, m mailmsg.Parsed, action string) (api.Preview, error) {
	pv, err := s.accountPreview(ctx, a, action)
	if err != nil {
		return pv, err
	}
	pv.Message = &api.Message{
		From: strings.ToLower(m.From.Address), Subject: m.Subject, Body: m.Body,
		To: addrList(m.To), Cc: addrList(m.Cc), Bcc: addrList(m.Bcc),
	}
	return pv, nil
}

// accountPreview fills the banner fields: the account, its colour, the other
// accounts for the look-alike warning and the last send.
func (s *Service) accountPreview(ctx context.Context, a store.Account, action string) (api.Preview, error) {
	nick := a.Nickname
	if strings.TrimSpace(nick) == "" {
		nick = a.Address
	}
	enabled := a.SendEnabled
	pv := api.Preview{
		Action: action, AccountID: a.ID, Nickname: nick, FromAddress: a.Address, DisplayName: a.DisplayName,
		ColorIndex: a.ColorIndex, AccountStatus: a.Status, SendEnabled: &enabled,
	}
	accts, err := s.Store.ListAccounts(ctx, a.Owner)
	if err != nil {
		return pv, errf(CodeInternal, "could not list accounts")
	}
	for _, o := range accts {
		if o.ID == a.ID {
			continue
		}
		on := o.Nickname
		if strings.TrimSpace(on) == "" {
			on = o.Address
		}
		pv.Siblings = append(pv.Siblings, api.Sibling{AccountID: o.ID, Nickname: on, Address: o.Address, DisplayName: o.DisplayName})
	}
	if evs, err := s.Store.ListAudit(ctx, a.Owner, s.now().Add(-30*24*time.Hour), 200); err == nil {
		for _, ev := range evs {
			if ev.AccountID == a.ID && ev.Tool == toolSendStatus && ev.Detail == store.SendSent {
				pv.LastSent = &api.LastSent{At: ev.At, Recipients: len(ev.Recipients)}
				break
			}
		}
	}
	return pv, nil
}

func addrList(l []mail.Address) []string {
	var out []string
	for _, a := range l {
		out = append(out, strings.ToLower(a.Address))
	}
	return out
}

// requestBatch groups per-message requests from one account into one
// email-send-batch approval: one banner, one tap, one passkey (R8).
func (s *Service) requestBatch(ctx context.Context, a store.Account, items []Item) (RequestOut, error) {
	var live []Item
	for _, it := range items {
		if it.Status == "pending" {
			live = append(live, it)
		}
	}
	if len(live) == 0 {
		return RequestOut{}, errf(CodeInvalid, "none of those drafts is waiting for approval; check their requests with email_send_status")
	}
	bp := api.BatchPayload{Owner: a.Owner, AccountID: a.ID, FromAddress: a.Address, Items: make([]api.BatchItem, 0, len(live))}
	ids := make([]string, 0, len(live))
	for _, it := range live {
		ap, err := s.Store.GetApproval(ctx, it.RequestID)
		if err != nil {
			return RequestOut{}, errf(CodeInternal, "could not read request %s", it.RequestID)
		}
		bp.Items = append(bp.Items, api.BatchItem{RequestID: it.RequestID, PayloadSHA256: ap.PayloadSHA256})
		ids = append(ids, it.RequestID)
	}
	sort.Strings(ids)
	canon, err := canonicalOf(bp)
	if err != nil {
		return RequestOut{}, errf(CodeInternal, "could not encode the batch")
	}
	pv, err := s.accountPreview(ctx, a, "Send these messages")
	if err != nil {
		return RequestOut{}, err
	}
	rawPreview, _ := json.Marshal(pv)
	h := sha256.Sum256([]byte("batch\x00" + a.ID + "\x00" + strings.Join(ids, "\x00")))
	key := hex.EncodeToString(h[:])
	resp, created, err := s.Approver.Create(ctx, api.CreateBody{IdempotencyKey: key, Kind: api.KindEmailSendBatch, Payload: canon, Preview: rawPreview})
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == "cross_account_batch" {
			return RequestOut{}, errf("cross_account_batch", "a batch holds sends from one account only")
		}
		return RequestOut{}, errf(CodeApprover, "the approval service could not create the batch: %s", describe(err))
	}
	if resp.PayloadSHA256 != api.SHA256(canon) {
		return RequestOut{}, errf(CodeInternal, "the approver hashed a different payload than moose sent")
	}
	_, _, err = s.Store.CreateApproval(ctx, store.Approval{
		ID: resp.RequestID, Owner: a.Owner, Kind: store.KindEmailSendBatch, AccountID: a.ID, IdempotencyKey: key,
		PayloadSHA256: api.SHA256(canon), Payload: canon, ApprovalURL: resp.ApprovalURL, Status: store.ApprovalPending,
		Created: s.now(), Expires: resp.ExpiresAt,
	}, s.now())
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return RequestOut{}, errf(CodeInternal, "could not record the batch")
	}
	if created {
		s.audit(ctx, a, "email_request_send", resp.RequestID, nil, "", fmt.Sprintf("batch of %d", len(live)))
	}
	out := RequestOut{Status: "pending", RequestID: resp.RequestID, ApprovalURL: resp.ApprovalURL, Batch: true, Items: items}
	return out, nil
}

func (s *Service) audit(ctx context.Context, a store.Account, tool, requestID string, recips []string, bodySHA, detail string) {
	err := s.Store.AppendAudit(ctx, store.AuditEvent{
		At: s.now(), Owner: a.Owner, RequestID: requestID, AccountID: a.ID, Tool: tool,
		Recipients: recips, BodySHA256: bodySHA, ApprovalID: requestID, Detail: detail,
	})
	if err != nil {
		s.logf("audit", "err", err.Error())
	}
}
