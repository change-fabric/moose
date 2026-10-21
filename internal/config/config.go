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
)

// Config is the full runtime configuration.
type Config struct {
	Env        string                     `yaml:"env"`
	Tailnet    string                     `yaml:"tailnet"`
	Hostname   string                     `yaml:"hostname"`
	Identity   Identity                   `yaml:"identity"`
	Connectors map[string]ConnectorConfig `yaml:"connectors"`
	Redaction  Redaction                  `yaml:"redaction"`
	Store      Store                      `yaml:"store"`

	// Env-only settings (secrets and runtime wiring).
	TSAuthKey          string `yaml:"-"`
	TSNetStateDir      string `yaml:"-"`
	BootstrapAdmin     string `yaml:"-"`
	GDriveKeyFile      string `yaml:"-"`
	GDriveKeySecretARN string `yaml:"-"`
	RelaySecretARN     string `yaml:"-"`
	Listen             string `yaml:"-"`
	DevIdentity        string `yaml:"-"`
}

type Identity struct {
	Mode          string   `yaml:"mode"`
	AllowedLogins []string `yaml:"allowed_logins"`
	AllowedTags   []string `yaml:"allowed_tags"`
}

type ConnectorConfig struct {
	Enabled          bool  `yaml:"enabled"`
	MaxDocumentBytes int64 `yaml:"max_document_bytes"`
}

type Redaction struct {
	Patterns []string `yaml:"patterns"`
}

type Store struct {
	Kind        string `yaml:"kind"`
	SQLitePath  string `yaml:"sqlite_path"`
	DynamoTable string `yaml:"dynamo_table"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Env:      EnvDev,
		Hostname: "moose",
		Identity: Identity{Mode: IdentityTSNet},
		Connectors: map[string]ConnectorConfig{
			"gdrive": {Enabled: true, MaxDocumentBytes: 2 << 20},
		},
		Store:         Store{Kind: StoreSQLite, SQLitePath: "/data/moose.db"},
		TSNetStateDir: "/data/tsnet",
	}
}

// Load reads the file named by MOOSE_CONFIG (if set), applies env overrides
// via getenv, and validates the result.
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
	set(&cfg.GDriveKeyFile, "MOOSE_GDRIVE_KEY_FILE")
	set(&cfg.GDriveKeySecretARN, "MOOSE_GDRIVE_KEY_SECRET_ARN")
	set(&cfg.RelaySecretARN, "MOOSE_RELAY_SECRET_ARN")
	set(&cfg.Listen, "MOOSE_LISTEN")
	set(&cfg.DevIdentity, "MOOSE_DEV_IDENTITY")
	if v := getenv("MOOSE_DYNAMO_TABLE"); v != "" {
		cfg.Store.DynamoTable = v
	}
}

// Validate checks consistency and prod safety rules.
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
	for _, p := range c.Redaction.Patterns {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("redaction pattern %q: %v", p, err))
		}
	}
	for id, cc := range c.Connectors {
		if cc.MaxDocumentBytes < 0 {
			errs = append(errs, fmt.Errorf("connectors.%s.max_document_bytes must not be negative", id))
		}
	}
	for _, tag := range c.Identity.AllowedTags {
		if !strings.HasPrefix(tag, "tag:") {
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
	if len(errs) > 0 {
		return fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return nil
}
