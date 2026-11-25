// Package config loads moose.yaml and applies MOOSE_* environment overrides.
// The file holds deployment settings only, never document access rules.
package config

import (
	"bytes"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/change-fabric/moose/internal/drive/core"
)

// Environments and identity modes.
const (
	EnvDev  = "dev"
	EnvProd = "prod"

	IdentityTSNet        = "tsnet"
	IdentityRelayHeaders = "relay-headers"
	IdentityDev          = "dev"
	IdentityPATOnly      = "pat-only"

	StoreSQLite = "sqlite"
	StoreDynamo = "dynamo"

	ProviderGmail = "gmail"
)

// Config is the full runtime configuration. The common fields apply to every
// server. Each server also has its own section, which is nil unless the file
// sets it; a server calls its own validator (ValidateDrive, ValidateEmail or
// ValidateApprover) and refuses to start when its section is missing or bad.
// Load validates only the common part, so one file may carry the sections of
// servers that run elsewhere.
type Config struct {
	Env      string   `yaml:"env"`
	Tailnet  string   `yaml:"tailnet"`
	Hostname string   `yaml:"hostname"`
	Identity Identity `yaml:"identity"`
	Store    Store    `yaml:"store"`

	Drive    *Drive    `yaml:"drive"`
	Email    *Email    `yaml:"email"`
	Approver *Approver `yaml:"approver"`

	// Env-only settings (secrets and runtime wiring).
	TSAuthKey      string `yaml:"-"`
	TSNetStateDir  string `yaml:"-"`
	BootstrapAdmin string `yaml:"-"`
	RelaySecretARN string `yaml:"-"`
	Listen         string `yaml:"-"`
	DevIdentity    string `yaml:"-"`
}

type Identity struct {
	Mode          string   `yaml:"mode"`
	AllowedLogins []string `yaml:"allowed_logins"`
	AllowedTags   []string `yaml:"allowed_tags"`
}

type Store struct {
	Kind        string `yaml:"kind"`
	SQLitePath  string `yaml:"sqlite_path"`
	DynamoTable string `yaml:"dynamo_table"`
}

// Drive configures moose-drive.
type Drive struct {
	Connectors map[string]ConnectorConfig `yaml:"connectors"`
	Redaction  Redaction                  `yaml:"redaction"`

	// Env-only settings.
	GDriveKeyFile      string `yaml:"-"`
	GDriveKeySecretARN string `yaml:"-"`
}

type ConnectorConfig struct {
	Enabled          bool  `yaml:"enabled"`
	MaxDocumentBytes int64 `yaml:"max_document_bytes"`
}

type Redaction struct {
	Patterns []string `yaml:"patterns"`
}

// Email configures moose-email.
type Email struct {
	Provider string    `yaml:"provider"`
	Send     EmailSend `yaml:"send"`
	// BaseURL is the public origin of this deployment (https://moose.example.org).
	// Setting it turns on the Google connect page, whose OAuth redirect URI is
	// BaseURL + /connect/google/callback.
	BaseURL string `yaml:"base_url"`
	// KMSKeyID is the symmetric KMS key that wraps the data keys sealing
	// refresh tokens and stored message snapshots.
	KMSKeyID string `yaml:"kms_key_id"`

	// OAuth settings. Setting any of Owner, RPID or ReceiptPublicKey turns on
	// the OAuth 2.1 authorization server and puts /mcp behind it; then all of
	// them, the service secret and BaseURL are required.
	//
	// Owner is the single tenant owner every token is issued to.
	Owner string `yaml:"owner"`
	// RPID is the WebAuthn relying party id every receipt must carry.
	RPID string `yaml:"rp_id"`
	// ReceiptPublicKey is the PEM public half of the receipt signing key,
	// pinned here so moose-email verifies receipts offline.
	ReceiptPublicKey string `yaml:"receipt_public_key"`
	// ApproverURL is where moose-approver's service API lives; it defaults to BaseURL.
	ApproverURL string `yaml:"approver_url"`

	// Env-only settings (secrets).
	// ApproverSecretARN names the Secrets Manager secret holding the HMAC key
	// shared with moose-approver; ApproverSecret is the dev alternative.
	ApproverSecretARN string `yaml:"-"`
	ApproverSecret    string `yaml:"-"`
	// DevKey is a base64 32-byte key that stands in for KMS in dev (MOOSE_EMAIL_DEV_KEY).
	DevKey string `yaml:"-"`
	// GoogleSecretARN names the Secrets Manager secret holding the tenant's
	// Google OAuth client as JSON {"client_id": ..., "client_secret": ...}.
	GoogleSecretARN string `yaml:"-"`
	// GoogleClientID and GoogleClientSecret are the dev alternative to the secret.
	GoogleClientID     string `yaml:"-"`
	GoogleClientSecret string `yaml:"-"`
}

// EmailSend gates live sending. It stays false until an end-to-end approval
// has been verified.
type EmailSend struct {
	Live bool `yaml:"live"`
}

// Approver configures moose-approver.
type Approver struct {
	RPID   string `yaml:"rp_id"`
	Origin string `yaml:"origin"`
	// Owner is the single tenant owner whose passkeys approve requests and
	// whose requests the service accepts.
	Owner string `yaml:"owner"`
	// SigningKeyID is the asymmetric ECC_NIST_P256 KMS key that signs receipts.
	SigningKeyID string `yaml:"signing_key_id"`
	// KMSKeyID is the symmetric KMS key that wraps the data keys sealing previews.
	KMSKeyID string `yaml:"kms_key_id"`

	// Env-only settings (secrets).
	// HMACSecretARN names the Secrets Manager secret shared with moose-email
	// that authenticates the service API; HMACSecret is the dev alternative.
	HMACSecretARN string `yaml:"-"`
	HMACSecret    string `yaml:"-"`
	// DevKey (base64, 32 bytes) stands in for the symmetric KMS key in dev.
	DevKey string `yaml:"-"`
	// DevSigningKey is a PEM EC private key standing in for the KMS signing key
	// in dev; when empty in dev, an ephemeral key is generated at start.
	DevSigningKey string `yaml:"-"`
}

// Default returns the built-in defaults. Per-server sections are nil: a
// section exists only when the file asks for it.
func Default() Config {
	return Config{
		Env:           EnvDev,
		Hostname:      "moose",
		Identity:      Identity{Mode: IdentityTSNet},
		Store:         Store{Kind: StoreSQLite, SQLitePath: "/data/moose.db"},
		TSNetStateDir: "/data/tsnet",
	}
}

// Load reads the file named by MOOSE_CONFIG (if set), applies env overrides
// via getenv, and validates the common settings. Callers then validate their
// own section.
func Load(getenv func(string) string) (Config, error) {
	cfg := Default()
	if path := getenv("MOOSE_CONFIG"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := Parse(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}
	applyEnv(&cfg, getenv)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Parse decodes YAML onto cfg, rejecting unknown keys.
func Parse(b []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func applyEnv(cfg *Config, getenv func(string) string) {
	set := func(dst *string, key string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	set(&cfg.Env, "MOOSE_ENV")
	set(&cfg.TSAuthKey, "TS_AUTHKEY")
	set(&cfg.TSNetStateDir, "MOOSE_TSNET_STATE_DIR")
	set(&cfg.BootstrapAdmin, "MOOSE_BOOTSTRAP_ADMIN")
	set(&cfg.RelaySecretARN, "MOOSE_RELAY_SECRET_ARN")
	set(&cfg.Listen, "MOOSE_LISTEN")
	set(&cfg.DevIdentity, "MOOSE_DEV_IDENTITY")
	if cfg.Drive != nil {
		set(&cfg.Drive.GDriveKeyFile, "MOOSE_GDRIVE_KEY_FILE")
		set(&cfg.Drive.GDriveKeySecretARN, "MOOSE_GDRIVE_KEY_SECRET_ARN")
	}
	if cfg.Email != nil {
		set(&cfg.Email.BaseURL, "MOOSE_EMAIL_BASE_URL")
		set(&cfg.Email.KMSKeyID, "MOOSE_EMAIL_KMS_KEY_ID")
		set(&cfg.Email.DevKey, "MOOSE_EMAIL_DEV_KEY")
		set(&cfg.Email.Owner, "MOOSE_EMAIL_OWNER")
		set(&cfg.Email.RPID, "MOOSE_EMAIL_RP_ID")
		set(&cfg.Email.ReceiptPublicKey, "MOOSE_EMAIL_RECEIPT_PUBLIC_KEY")
		set(&cfg.Email.ApproverURL, "MOOSE_EMAIL_APPROVER_URL")
		set(&cfg.Email.ApproverSecretARN, "MOOSE_EMAIL_APPROVER_SECRET_ARN")
		set(&cfg.Email.ApproverSecret, "MOOSE_EMAIL_APPROVER_SECRET")
		set(&cfg.Email.GoogleSecretARN, "MOOSE_GOOGLE_OAUTH_SECRET_ARN")
		set(&cfg.Email.GoogleClientID, "MOOSE_GOOGLE_CLIENT_ID")
		set(&cfg.Email.GoogleClientSecret, "MOOSE_GOOGLE_CLIENT_SECRET")
	}
	if cfg.Approver != nil {
		set(&cfg.Approver.RPID, "MOOSE_APPROVER_RP_ID")
		set(&cfg.Approver.Origin, "MOOSE_APPROVER_ORIGIN")
		set(&cfg.Approver.Owner, "MOOSE_APPROVER_OWNER")
		set(&cfg.Approver.SigningKeyID, "MOOSE_APPROVER_SIGNING_KEY_ID")
		set(&cfg.Approver.KMSKeyID, "MOOSE_APPROVER_KMS_KEY_ID")
		set(&cfg.Approver.HMACSecretARN, "MOOSE_APPROVER_HMAC_SECRET_ARN")
		set(&cfg.Approver.HMACSecret, "MOOSE_APPROVER_HMAC_SECRET")
		set(&cfg.Approver.DevKey, "MOOSE_APPROVER_DEV_KEY")
		set(&cfg.Approver.DevSigningKey, "MOOSE_APPROVER_DEV_SIGNING_KEY")
	}
	if v := getenv("MOOSE_DYNAMO_TABLE"); v != "" {
		cfg.Store.DynamoTable = v
	}
}

// Validate checks the common settings and the production safety rules.
// Per-server sections are checked by ValidateDrive, ValidateEmail and
// ValidateApprover.
func (c Config) Validate() error {
	var errs []error
	switch c.Env {
	case EnvDev, EnvProd:
	default:
		errs = append(errs, fmt.Errorf("env must be dev or prod, got %q", c.Env))
	}
	switch c.Identity.Mode {
	case IdentityTSNet, IdentityRelayHeaders, IdentityDev, IdentityPATOnly:
	default:
		errs = append(errs, fmt.Errorf("identity.mode %q is not one of tsnet, relay-headers, dev, pat-only", c.Identity.Mode))
	}
	switch c.Store.Kind {
	case StoreSQLite:
		if c.Store.SQLitePath == "" {
			errs = append(errs, errors.New("store.sqlite_path is required for sqlite"))
		}
	case StoreDynamo:
		if c.Store.DynamoTable == "" {
			errs = append(errs, errors.New("store.dynamo_table (or MOOSE_DYNAMO_TABLE) is required for dynamo"))
		}
	default:
		errs = append(errs, fmt.Errorf("store.kind %q is not sqlite or dynamo", c.Store.Kind))
	}
	for _, tag := range c.Identity.AllowedTags {
		if !core.IsTag(tag) {
			errs = append(errs, fmt.Errorf("identity.allowed_tags entry %q must start with tag:", tag))
		}
	}
	if c.Identity.Mode == IdentityDev && c.DevIdentity == "" {
		errs = append(errs, errors.New("identity.mode dev requires MOOSE_DEV_IDENTITY"))
	}
	if c.Env == EnvProd {
		if c.Listen != "" {
			errs = append(errs, errors.New("MOOSE_LISTEN (plain-HTTP dev listener) is refused when env is prod"))
		}
		if c.Identity.Mode == IdentityDev {
			errs = append(errs, errors.New("identity.mode dev is refused when env is prod"))
		}
		if c.DevIdentity != "" {
			errs = append(errs, errors.New("MOOSE_DEV_IDENTITY is refused when env is prod"))
		}
	}
	return joinErrs(errs)
}

// ValidateDrive checks the drive: section, which moose-drive requires.
func (c Config) ValidateDrive() error {
	d := c.Drive
	if d == nil {
		return errors.New("config: the drive: section is required by the Drive server")
	}
	var errs []error
	for _, p := range d.Redaction.Patterns {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("drive.redaction pattern %q: %v", p, err))
		}
	}
	for id, cc := range d.Connectors {
		if cc.MaxDocumentBytes < 0 {
			errs = append(errs, fmt.Errorf("drive.connectors.%s.max_document_bytes must not be negative", id))
		}
	}
	return joinErrs(errs)
}

// ValidateEmail checks the email: section, which moose-email requires.
func (c Config) ValidateEmail() error {
	e := c.Email
	if e == nil {
		return errors.New("config: the email: section is required by the email server")
	}
	var errs []error
	if e.Provider != ProviderGmail {
		errs = append(errs, fmt.Errorf("email.provider %q is not %s", e.Provider, ProviderGmail))
	}
	if e.BaseURL != "" {
		errs = append(errs, c.validateEmailConnect()...)
	}
	if e.OAuthEnabled() {
		errs = append(errs, c.validateEmailOAuth()...)
	} else if c.Env == EnvProd && e.BaseURL != "" {
		errs = append(errs, errors.New("email.owner, email.rp_id and email.receipt_public_key are required when env is prod: a public /mcp needs the OAuth server"))
	}
	return joinErrs(errs)
}

// OAuthEnabled reports whether the OAuth authorization server is configured.
func (e *Email) OAuthEnabled() bool {
	return e.Owner != "" || e.RPID != "" || e.ReceiptPublicKey != ""
}

// validateEmailOAuth checks what the authorization server and the protected
// /mcp need: the public origin, the owner, the pinned receipt key and the
// shared secret for the service API.
func (c Config) validateEmailOAuth() []error {
	e := c.Email
	var errs []error
	if e.BaseURL == "" {
		errs = append(errs, errors.New("email.base_url is required for OAuth (it is the issuer and the /mcp resource)"))
	}
	if e.Owner == "" {
		errs = append(errs, errors.New("email.owner is required for OAuth"))
	}
	if e.RPID == "" {
		errs = append(errs, errors.New("email.rp_id is required for OAuth"))
	}
	if blk, _ := pem.Decode([]byte(e.ReceiptPublicKey)); blk == nil || blk.Type != "PUBLIC KEY" {
		errs = append(errs, errors.New("email.receipt_public_key must be a PEM PUBLIC KEY"))
	}
	if e.ApproverURL != "" {
		if u, err := url.Parse(e.ApproverURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			errs = append(errs, fmt.Errorf("email.approver_url %q must be an http(s) URL", e.ApproverURL))
		}
	}
	switch {
	case e.ApproverSecretARN == "" && e.ApproverSecret == "":
		errs = append(errs, errors.New("MOOSE_EMAIL_APPROVER_SECRET_ARN (or MOOSE_EMAIL_APPROVER_SECRET in dev) is required for OAuth"))
	case e.ApproverSecretARN != "" && e.ApproverSecret != "":
		errs = append(errs, errors.New("set MOOSE_EMAIL_APPROVER_SECRET_ARN or MOOSE_EMAIL_APPROVER_SECRET, not both"))
	case e.ApproverSecret != "" && len(e.ApproverSecret) < 32:
		errs = append(errs, errors.New("MOOSE_EMAIL_APPROVER_SECRET must be at least 32 characters"))
	}
	if c.Env == EnvProd && e.ApproverSecret != "" {
		errs = append(errs, errors.New("MOOSE_EMAIL_APPROVER_SECRET is refused when env is prod; use MOOSE_EMAIL_APPROVER_SECRET_ARN"))
	}
	return errs
}

// validateEmailConnect checks what the Google connect page needs: a public
// origin, a key to seal tokens with, and the Google OAuth client.
func (c Config) validateEmailConnect() []error {
	e := c.Email
	var errs []error
	u, err := url.Parse(e.BaseURL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		errs = append(errs, fmt.Errorf("email.base_url %q must be an http(s) origin", e.BaseURL))
	case u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "":
		errs = append(errs, fmt.Errorf("email.base_url %q must be an origin with no path", e.BaseURL))
	case c.Env == EnvProd && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("email.base_url %q must start with https:// when env is prod", e.BaseURL))
	}
	switch {
	case e.KMSKeyID == "" && e.DevKey == "":
		errs = append(errs, errors.New("email.kms_key_id (or MOOSE_EMAIL_DEV_KEY in dev) is required to seal tokens"))
	case e.KMSKeyID != "" && e.DevKey != "":
		errs = append(errs, errors.New("set email.kms_key_id or MOOSE_EMAIL_DEV_KEY, not both"))
	case e.DevKey != "":
		if k, err := base64.StdEncoding.DecodeString(e.DevKey); err != nil || len(k) != 32 {
			errs = append(errs, errors.New("MOOSE_EMAIL_DEV_KEY must be 32 bytes, base64 encoded"))
		}
	}
	hasPair := e.GoogleClientID != "" && e.GoogleClientSecret != ""
	switch {
	case e.GoogleSecretARN == "" && !hasPair:
		errs = append(errs, errors.New("MOOSE_GOOGLE_OAUTH_SECRET_ARN (or MOOSE_GOOGLE_CLIENT_ID and MOOSE_GOOGLE_CLIENT_SECRET in dev) is required"))
	case e.GoogleSecretARN != "" && (e.GoogleClientID != "" || e.GoogleClientSecret != ""):
		errs = append(errs, errors.New("set MOOSE_GOOGLE_OAUTH_SECRET_ARN or the client id and secret, not both"))
	}
	if c.Env == EnvProd {
		if e.DevKey != "" {
			errs = append(errs, errors.New("MOOSE_EMAIL_DEV_KEY is refused when env is prod"))
		}
		if e.GoogleClientSecret != "" {
			errs = append(errs, errors.New("MOOSE_GOOGLE_CLIENT_SECRET is refused when env is prod; use MOOSE_GOOGLE_OAUTH_SECRET_ARN"))
		}
	}
	return errs
}

// ValidateApprover checks the approver: section, which moose-approver requires.
func (c Config) ValidateApprover() error {
	a := c.Approver
	if a == nil {
		return errors.New("config: the approver: section is required by the approver server")
	}
	var errs []error
	if a.RPID == "" {
		errs = append(errs, errors.New("approver.rp_id is required"))
	}
	switch {
	case a.Origin == "":
		errs = append(errs, errors.New("approver.origin is required"))
	case c.Env == EnvProd && !strings.HasPrefix(a.Origin, "https://"):
		errs = append(errs, fmt.Errorf("approver.origin %q must start with https:// when env is prod", a.Origin))
	}
	return joinErrs(errs)
}

// ValidateApproverServe checks what moose-approver needs to serve requests, on
// top of ValidateApprover: the owner, a receipt signing key, a key to seal
// previews and the service API secret. Production takes the keys from KMS and
// the secret from Secrets Manager and refuses the dev alternatives.
func (c Config) ValidateApproverServe() error {
	if err := c.ValidateApprover(); err != nil {
		return err
	}
	a := c.Approver
	var errs []error
	if a.Owner == "" {
		errs = append(errs, errors.New("approver.owner (or MOOSE_APPROVER_OWNER) is required"))
	}
	if u, err := url.Parse(a.Origin); err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, fmt.Errorf("approver.origin %q must be an origin with no path", a.Origin))
	}
	switch {
	case a.KMSKeyID == "" && a.DevKey == "":
		errs = append(errs, errors.New("approver.kms_key_id (or MOOSE_APPROVER_DEV_KEY in dev) is required to seal previews"))
	case a.KMSKeyID != "" && a.DevKey != "":
		errs = append(errs, errors.New("set approver.kms_key_id or MOOSE_APPROVER_DEV_KEY, not both"))
	case a.DevKey != "":
		if k, err := base64.StdEncoding.DecodeString(a.DevKey); err != nil || len(k) != 32 {
			errs = append(errs, errors.New("MOOSE_APPROVER_DEV_KEY must be 32 bytes, base64 encoded"))
		}
	}
	if a.SigningKeyID != "" && a.DevSigningKey != "" {
		errs = append(errs, errors.New("set approver.signing_key_id or MOOSE_APPROVER_DEV_SIGNING_KEY, not both"))
	}
	switch {
	case a.HMACSecretARN == "" && a.HMACSecret == "":
		errs = append(errs, errors.New("MOOSE_APPROVER_HMAC_SECRET_ARN (or MOOSE_APPROVER_HMAC_SECRET in dev) is required"))
	case a.HMACSecretARN != "" && a.HMACSecret != "":
		errs = append(errs, errors.New("set MOOSE_APPROVER_HMAC_SECRET_ARN or MOOSE_APPROVER_HMAC_SECRET, not both"))
	case a.HMACSecret != "" && len(a.HMACSecret) < 32:
		errs = append(errs, errors.New("MOOSE_APPROVER_HMAC_SECRET must be at least 32 characters"))
	}
	if c.Env == EnvProd {
		if a.SigningKeyID == "" {
			errs = append(errs, errors.New("approver.signing_key_id is required when env is prod"))
		}
		if a.KMSKeyID == "" || a.DevKey != "" || a.DevSigningKey != "" || a.HMACSecret != "" {
			errs = append(errs, errors.New("prod needs approver.kms_key_id and MOOSE_APPROVER_HMAC_SECRET_ARN, and refuses the MOOSE_APPROVER_DEV_* and MOOSE_APPROVER_HMAC_SECRET settings"))
		}
	}
	return joinErrs(errs)
}

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: %w", errors.Join(errs...))
}
