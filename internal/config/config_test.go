package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "moose.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsValid(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvDev || cfg.Store.Kind != StoreSQLite || !cfg.Connectors["gdrive"].Enabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadFileAndEnvOverrides(t *testing.T) {
	p := writeYAML(t, `
env: dev
tailnet: example.ts.net
identity:
  mode: tsnet
  allowed_tags: [tag:moose-client]
connectors:
  gdrive:
    enabled: true
    max_document_bytes: 1024
redaction:
  patterns: ["AKIA[0-9A-Z]{16}"]
store:
  kind: dynamo
  dynamo_table: from-file
`)
	cfg, err := Load(env(map[string]string{
		"MOOSE_CONFIG": p, "MOOSE_ENV": "prod", "MOOSE_DYNAMO_TABLE": "from-env",
		"MOOSE_BOOTSTRAP_ADMIN": "me@example.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvProd || cfg.Store.DynamoTable != "from-env" || cfg.BootstrapAdmin != "me@example.com" ||
		cfg.Connectors["gdrive"].MaxDocumentBytes != 1024 || cfg.Tailnet != "example.ts.net" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestProdRefusals(t *testing.T) {
	cases := map[string]struct {
		yaml string
		env  map[string]string
		want string
	}{
		"listen":       {"env: prod\n", map[string]string{"MOOSE_LISTEN": "127.0.0.1:8787"}, "MOOSE_LISTEN"},
		"dev mode":     {"env: prod\nidentity: {mode: dev}\n", map[string]string{"MOOSE_DEV_IDENTITY": "me@x"}, "identity.mode dev"},
		"dev identity": {"env: prod\n", map[string]string{"MOOSE_DEV_IDENTITY": "me@x"}, "MOOSE_DEV_IDENTITY"},
		"env override": {"env: dev\n", map[string]string{"MOOSE_ENV": "prod", "MOOSE_LISTEN": ":1"}, "MOOSE_LISTEN"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := map[string]string{"MOOSE_CONFIG": writeYAML(t, tc.yaml)}
			for k, v := range tc.env {
				e[k] = v
			}
			_, err := Load(env(e))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDevAllowsListenAndDevIdentity(t *testing.T) {
	p := writeYAML(t, "env: dev\nidentity: {mode: dev}\n")
	cfg, err := Load(env(map[string]string{"MOOSE_CONFIG": p, "MOOSE_LISTEN": "127.0.0.1:8787", "MOOSE_DEV_IDENTITY": "me@example.com"}))
	if err != nil || cfg.Listen != "127.0.0.1:8787" {
		t.Fatalf("got %+v %v", cfg, err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"bad env":       "env: staging\n",
		"bad mode":      "identity: {mode: magic}\n",
		"bad regex":     "redaction: {patterns: ['(']}\n",
		"bad store":     "store: {kind: postgres}\n",
		"dynamo no tbl": "store: {kind: dynamo}\n",
		"unknown key":   "access_rules: []\n",
		"bad tag":       "identity: {mode: tsnet, allowed_tags: [moose-client]}\n",
		"dev no ident":  "identity: {mode: dev}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(env(map[string]string{"MOOSE_CONFIG": writeYAML(t, body)})); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(env(map[string]string{"MOOSE_CONFIG": "/nonexistent/moose.yaml"})); err == nil {
		t.Fatal("want error")
	}
}
