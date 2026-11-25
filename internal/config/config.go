// Package config loads moose.yaml and applies MOOSE_* environment overrides.
// The file holds deployment settings only, never document access rules.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
	return joinErrs(errs)
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

func joinErrs(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: %w", errors.Join(errs...))
}
