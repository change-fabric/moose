// Package api is the HMAC-protected service API moose-email uses to create
// approval requests, poll their status and consume them, with the payload and
// preview types both services share.
//
// Payloads are canonical JSON (keys sorted, SetEscapeHTML(false), no
// whitespace); the receipt's payload_sha256 is the sha256 of those bytes, which
// the approver computes itself, so the two sides agree on exactly what was
// approved.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	estore "github.com/change-fabric/moose/internal/accounts/store"
)

// Kinds, shared with moose-email's store.
const (
	KindAccountSendEnable = estore.KindAccountSendEnable
	KindEmailSend         = estore.KindEmailSend
	KindEmailSendBatch    = estore.KindEmailSendBatch
	KindContactTrustBatch = estore.KindContactTrustBatch
	KindDashboardSession  = estore.KindDashboardSession
)

// MaxBatchItems bounds one batch approval.
const MaxBatchItems = 25

// FromKinds are the kinds whose page leads with the FROM banner and needs the
// recorded "Send from this account" tap before an assertion is accepted.
func FromKinds(kind string) bool {
	return kind == KindEmailSend || kind == KindEmailSendBatch || kind == KindAccountSendEnable
}

// SendKinds are the kinds that send mail and therefore need a ready account.
func SendKinds(kind string) bool { return kind == KindEmailSend || kind == KindEmailSendBatch }

// Canonical re-encodes raw JSON canonically: object keys sorted, compact, no
// HTML escaping, numbers kept as written.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("payload is not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("payload has trailing data")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SHA256 is the hex sha256 of canonical payload bytes.
func SHA256(canonical []byte) string {
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:])
}

// Payload shapes. Each is decoded strictly: unknown keys are rejected.

// TrustChange promotes an address to trusted.
type TrustChange struct {
	Address string `json:"address"`
	To      string `json:"to"`
}

// SoftFlag is a recipient policy flag shown next to the recipients.
type SoftFlag struct {
	Flag    string `json:"flag"`
	Address string `json:"address,omitempty"`
}

// SendEnablePayload is the account-send-enable payload.
type SendEnablePayload struct {
	Owner       string `json:"owner"`
	AccountID   string `json:"account_id"`
	FromAddress string `json:"from_address"`
}

// EmailSendPayload is the email-send payload.
type EmailSendPayload struct {
	Owner        string        `json:"owner"`
	AccountID    string        `json:"account_id"`
	FromAddress  string        `json:"from_address"`
	DraftID      string        `json:"draft_id"`
	RFC822SHA256 string        `json:"rfc822_sha256"`
	Recipients   []string      `json:"recipients"`
	SoftFlags    []SoftFlag    `json:"soft_flags"`
	TrustChanges []TrustChange `json:"trust_changes"`
}

// ContactTrustPayload is the contact-trust-batch payload.
type ContactTrustPayload struct {
	Owner     string        `json:"owner"`
	AccountID string        `json:"account_id"`
	Changes   []TrustChange `json:"changes"`
}

// DashboardSessionPayload is the dashboard-session payload.
type DashboardSessionPayload struct {
	Owner           string `json:"owner"`
	ClientID        string `json:"client_id"`
	ClientName      string `json:"client_name"`
	RedirectURIHost string `json:"redirect_uri_host"`
	TxnHash         string `json:"txn_hash"`
}

// BatchItem names one email-send request inside a batch.
type BatchItem struct {
	RequestID     string `json:"request_id"`
	PayloadSHA256 string `json:"payload_sha256"`
}

// BatchPayload is the email-send-batch payload. The receipt covers Items.
type BatchPayload struct {
	Owner       string      `json:"owner"`
	AccountID   string      `json:"account_id"`
	FromAddress string      `json:"from_address"`
	Items       []BatchItem `json:"items"`
}

// Parsed is a validated payload.
type Parsed struct {
	Kind      string
	Owner     string
	AccountID string
	From      string
	Canonical []byte
	SHA256    string

	SendEnable *SendEnablePayload
	Email      *EmailSendPayload
	Trust      *ContactTrustPayload
	Dashboard  *DashboardSessionPayload
	Batch      *BatchPayload
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func strict[T any](canonical []byte) (*T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func need(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		if strings.TrimSpace(pairs[i+1]) == "" {
			return fmt.Errorf("%s is required", pairs[i])
		}
	}
	return nil
}

func checkTrust(changes []TrustChange) error {
	for _, c := range changes {
		if strings.TrimSpace(c.Address) == "" || c.To != "trusted" {
			return errors.New(`trust change needs an address and to: "trusted"`)
		}
	}
	return nil
}

// ParsePayload canonicalizes raw and validates it for kind.
func ParsePayload(kind string, raw []byte) (Parsed, error) {
	canon, err := Canonical(raw)
	if err != nil {
		return Parsed{}, err
	}
	p := Parsed{Kind: kind, Canonical: canon, SHA256: SHA256(canon)}
	switch kind {
	case KindAccountSendEnable:
		v, err := strict[SendEnablePayload](canon)
		if err != nil {
			return Parsed{}, err
		}
		if err := need("owner", v.Owner, "account_id", v.AccountID, "from_address", v.FromAddress); err != nil {
			return Parsed{}, err
		}
		p.SendEnable, p.Owner, p.AccountID, p.From = v, v.Owner, v.AccountID, v.FromAddress
	case KindEmailSend:
		v, err := strict[EmailSendPayload](canon)
		if err != nil {
			return Parsed{}, err
		}
		if err := need("owner", v.Owner, "account_id", v.AccountID, "from_address", v.FromAddress, "draft_id", v.DraftID); err != nil {
			return Parsed{}, err
		}
		if !sha256Hex.MatchString(v.RFC822SHA256) {
			return Parsed{}, errors.New("rfc822_sha256 must be a hex sha256")
		}
		if len(v.Recipients) == 0 {
			return Parsed{}, errors.New("recipients is required")
		}
		if v.SoftFlags == nil || v.TrustChanges == nil {
			return Parsed{}, errors.New("soft_flags and trust_changes are required (use [])")
		}
		if err := checkTrust(v.TrustChanges); err != nil {
			return Parsed{}, err
		}
		p.Email, p.Owner, p.AccountID, p.From = v, v.Owner, v.AccountID, v.FromAddress
	case KindContactTrustBatch:
		v, err := strict[ContactTrustPayload](canon)
		if err != nil {
			return Parsed{}, err
		}
		if err := need("owner", v.Owner, "account_id", v.AccountID); err != nil {
			return Parsed{}, err
		}
		if len(v.Changes) == 0 {
			return Parsed{}, errors.New("changes is required")
		}
		if err := checkTrust(v.Changes); err != nil {
			return Parsed{}, err
		}
		p.Trust, p.Owner, p.AccountID = v, v.Owner, v.AccountID
	case KindDashboardSession:
		v, err := strict[DashboardSessionPayload](canon)
		if err != nil {
			return Parsed{}, err
		}
		if err := need("owner", v.Owner, "client_id", v.ClientID, "client_name", v.ClientName, "redirect_uri_host", v.RedirectURIHost, "txn_hash", v.TxnHash); err != nil {
			return Parsed{}, err
		}
		p.Dashboard, p.Owner = v, v.Owner
	case KindEmailSendBatch:
		v, err := strict[BatchPayload](canon)
		if err != nil {
			return Parsed{}, err
		}
		if err := need("owner", v.Owner, "account_id", v.AccountID, "from_address", v.FromAddress); err != nil {
			return Parsed{}, err
		}
		if n := len(v.Items); n == 0 || n > MaxBatchItems {
			return Parsed{}, fmt.Errorf("a batch holds 1 to %d items", MaxBatchItems)
		}
		seen := map[string]bool{}
		for _, it := range v.Items {
			if it.RequestID == "" || !sha256Hex.MatchString(it.PayloadSHA256) || seen[it.RequestID] {
				return Parsed{}, errors.New("batch items need distinct request_ids and hex payload_sha256 values")
			}
			seen[it.RequestID] = true
		}
		p.Batch, p.Owner, p.AccountID, p.From = v, v.Owner, v.AccountID, v.FromAddress
	default:
		return Parsed{}, fmt.Errorf("unknown kind %q", kind)
	}
	return p, nil
}

// Preview is what moose-email asks the approver to show. It is stored sealed
// and deleted when the request is denied, expires or is consumed. The banner
// fields are cosmetic: the address and account id shown come from the signed
// payload, and Create refuses a preview that disagrees with it.
type Preview struct {
	Action      string `json:"action,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	Nickname    string `json:"nickname,omitempty"`
	FromAddress string `json:"from_address,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	ColorIndex  int    `json:"color_index,omitempty"`
	// AccountStatus and SendEnabled are moose-email's view when it created
	// the request; a send request for an account that is not ok and enabled
	// is refused (R11). Dispatch rechecks them again.
	AccountStatus string `json:"account_status,omitempty"`
	SendEnabled   *bool  `json:"send_enabled,omitempty"`
	// Siblings are the owner's other accounts, for the look-alike warning.
	Siblings []Sibling `json:"siblings,omitempty"`
	LastSent *LastSent `json:"last_sent,omitempty"`
	Message  *Message  `json:"message,omitempty"`
	// SemanticMatch is true when the provider re-serialises the message on send
	// (Microsoft Graph), so the guarantee is that the sent content matches the
	// fields shown, not that the bytes are identical. The approver says so.
	SemanticMatch bool `json:"semantic_match,omitempty"`
}

// Sibling is another connected account, as much as the look-alike check needs.
type Sibling struct {
	AccountID   string `json:"account_id,omitempty"`
	Nickname    string `json:"nickname"`
	Address     string `json:"address"`
	DisplayName string `json:"display_name,omitempty"`
}

// LastSent is metadata about the account's most recent send.
type LastSent struct {
	At         time.Time `json:"at"`
	Recipients int       `json:"recipients"`
}

// Message is the exact message to be sent, parsed from the stored RFC822 bytes.
type Message struct {
	From    string   `json:"from"`
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
}

// Addresses returns every recipient address in the message, lowercased.
func (m Message) Addresses() []string {
	var out []string
	for _, l := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, a := range l {
			out = append(out, strings.ToLower(strings.TrimSpace(a)))
		}
	}
	return out
}

// Wire types.

// CreateBody is the body of POST /approve/api/requests.
type CreateBody struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	Preview        json.RawMessage `json:"preview"`
}

// CreateResponse is the answer to a create.
type CreateResponse struct {
	RequestID     string    `json:"request_id"`
	PayloadSHA256 string    `json:"payload_sha256"`
	ApprovalURL   string    `json:"approval_url"`
	ExpiresAt     time.Time `json:"expires_at"`
}
