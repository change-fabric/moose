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
	if cfg.Env != EnvDev || cfg.Store.Kind != StoreSQLite || cfg.Drive != nil || cfg.Email != nil || cfg.Approver != nil {
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
drive:
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
		cfg.Drive.Connectors["gdrive"].MaxDocumentBytes != 1024 || cfg.Tailnet != "example.ts.net" {
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

func load(t *testing.T, body string, extra map[string]string) Config {
	t.Helper()
	e := map[string]string{"MOOSE_CONFIG": writeYAML(t, body)}
	for k, v := range extra {
		e[k] = v
	}
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestMissingServerSections(t *testing.T) {
	// A file with only common settings loads, then each server refuses to start.
	cfg := load(t, "env: dev\n", nil)
	wantErr(t, cfg.ValidateDrive(), "drive: section is required")
	wantErr(t, cfg.ValidateEmail(), "email: section is required")
	wantErr(t, cfg.ValidateApprover(), "approver: section is required")
}

func TestSectionsAreValidatedPerServer(t *testing.T) {
	// A bad email section does not stop the Drive server, and vice versa.
	cfg := load(t, `
drive:
  connectors:
    gdrive: {enabled: true}
email:
  provider: outlook
`, nil)
	if err := cfg.ValidateDrive(); err != nil {
		t.Fatalf("drive should be valid: %v", err)
	}
	wantErr(t, cfg.ValidateEmail(), "email.provider")
	wantErr(t, cfg.ValidateApprover(), "approver: section is required")
}

func TestValidateDrive(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"bad regex": {"drive: {redaction: {patterns: ['(']}}\n", "drive.redaction pattern"},
		"negative":  {"drive: {connectors: {gdrive: {max_document_bytes: -1}}}\n", "drive.connectors.gdrive.max_document_bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantErr(t, load(t, tc.yaml, nil).ValidateDrive(), tc.want)
		})
	}
}

func TestDriveEnvOnlySettings(t *testing.T) {
	cfg := load(t, "drive: {}\n", map[string]string{"MOOSE_GDRIVE_KEY_FILE": "/k.json", "MOOSE_GDRIVE_KEY_SECRET_ARN": "arn:x"})
	if cfg.Drive.GDriveKeyFile != "/k.json" || cfg.Drive.GDriveKeySecretARN != "arn:x" {
		t.Fatalf("env settings not applied: %+v", cfg.Drive)
	}
	// Without a drive: section the Drive env settings have nowhere to go.
	if cfg := load(t, "env: dev\n", map[string]string{"MOOSE_GDRIVE_KEY_FILE": "/k.json"}); cfg.Drive != nil {
		t.Fatalf("drive section appeared from env alone: %+v", cfg.Drive)
	}
}

func TestValidateEmail(t *testing.T) {
	cfg := load(t, "email: {provider: gmail}\n", nil)
	if err := cfg.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	if cfg.Email.Send.Live {
		t.Fatal("send.live must default to false")
	}
	wantErr(t, load(t, "email: {}\n", nil).ValidateEmail(), "email.provider")
}

func TestValidateCalendar(t *testing.T) {
	wantErr(t, load(t, "env: dev\n", nil).ValidateCalendar(), "email: section is required")
	if load(t, "email: {provider: gmail}\n", nil).Email.Calendar.Enabled {
		t.Fatal("email.calendar.enabled must default to false")
	}
	wantErr(t, load(t, "email: {provider: gmail}\n", nil).ValidateCalendar(), "email.calendar.enabled is false")
	on := "email: {provider: gmail, calendar: {enabled: true}"

	// Dev without a public origin needs a dev identity.
	wantErr(t, load(t, on+"}\n", nil).ValidateCalendar(), "dev identity")
	if err := load(t, on+"}\n", map[string]string{"MOOSE_DEV_IDENTITY": "pat"}).ValidateCalendar(); err != nil {
		t.Fatalf("dev with a dev identity: %v", err)
	}
	if err := load(t, on+", base_url: 'https://moose.example.org'}\n", nil).ValidateCalendar(); err != nil {
		t.Fatalf("with a public origin: %v", err)
	}
	wantErr(t, load(t, on+", base_url: 'https://moose.example.org/x'}\n", nil).ValidateCalendar(), "no path")
	wantErr(t, load(t, on+", base_url: 'ftp://moose.example.org'}\n", nil).ValidateCalendar(), "http(s) origin")
	wantErr(t, load(t, "env: prod\n"+on+"}\n", map[string]string{"MOOSE_DYNAMO_TABLE": "t"}).ValidateCalendar(), "email.base_url is required")
	wantErr(t, load(t, "env: prod\n"+on+", base_url: 'http://moose.example.org'}\n", map[string]string{"MOOSE_DYNAMO_TABLE": "t"}).ValidateCalendar(), "https://")
	// Both flags are independent: calendar on does not turn Microsoft on.
	if cfg := load(t, on+"}\n", nil); cfg.Email.MicrosoftEnabled() {
		t.Error("calendar.enabled turned Microsoft on")
	}
}

func TestValidateApprover(t *testing.T) {
	ok := load(t, "approver: {rp_id: moose.example.org, origin: 'https://moose.example.org'}\n", nil)
	if err := ok.ValidateApprover(); err != nil {
		t.Fatal(err)
	}
	wantErr(t, load(t, "approver: {}\n", nil).ValidateApprover(), "approver.rp_id")
	wantErr(t, load(t, "approver: {rp_id: x}\n", nil).ValidateApprover(), "approver.origin")
	wantErr(t, load(t, "env: prod\napprover: {rp_id: x, origin: 'http://x'}\n", nil).ValidateApprover(), "https://")
	if err := load(t, "approver: {rp_id: localhost, origin: 'http://localhost:8080'}\n", nil).ValidateApprover(); err != nil {
		t.Fatalf("http origin is fine in dev: %v", err)
	}
}

func TestOldTopLevelDriveKeysRejected(t *testing.T) {
	for _, body := range []string{"connectors: {gdrive: {enabled: true}}\n", "redaction: {patterns: []}\n"} {
		if _, err := Load(env(map[string]string{"MOOSE_CONFIG": writeYAML(t, body)})); err == nil {
			t.Fatalf("pre-split key accepted: %s", body)
		}
	}
}

func TestShippedConfigsLoad(t *testing.T) {
	cases := map[string]struct {
		path string
		env  map[string]string
	}{
		"example": {"../../moose.example.yaml", nil},
		"lambda":  {"../../deploy/lambda/moose.yaml", map[string]string{"MOOSE_DYNAMO_TABLE": "t"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := map[string]string{"MOOSE_CONFIG": tc.path}
			for k, v := range tc.env {
				e[k] = v
			}
			cfg, err := Load(env(e))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ValidateDrive(); err != nil {
				t.Fatal(err)
			}
			if !cfg.Drive.Connectors["gdrive"].Enabled {
				t.Fatalf("gdrive not enabled: %+v", cfg.Drive)
			}
		})
	}
}

func TestShippedLambdaCalendarFlags(t *testing.T) {
	prodEnv := map[string]string{"MOOSE_DYNAMO_TABLE": "t", "MOOSE_EMAIL_BASE_URL": "https://moose.example.org"}
	load := func(path string) Config {
		t.Helper()
		e := map[string]string{"MOOSE_CONFIG": path}
		for k, v := range prodEnv {
			e[k] = v
		}
		cfg, err := Load(env(e))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	// The email Lambda keeps calendar off in committed config: no calendar token
	// can be issued and no Grant calendar button shows until acceptance.
	if load("../../deploy/lambda/moose-email.yaml").Email.Calendar.Enabled {
		t.Fatal("deploy/lambda/moose-email.yaml turns calendar on; it stays false until docs/acceptance/calendar.md is complete")
	}
	// The calendar Lambda's own config serves, and keeps the providers aligned with email's.
	cal := load("../../deploy/lambda/moose-calendar.yaml")
	if err := cal.ValidateCalendar(); err != nil {
		t.Fatalf("deploy/lambda/moose-calendar.yaml: %v", err)
	}
	em := load("../../deploy/lambda/moose-email.yaml")
	if cal.Email.MicrosoftEnabled() != em.Email.MicrosoftEnabled() || strings.Join(cal.Email.ProviderList(), ",") != strings.Join(em.Email.ProviderList(), ",") {
		t.Errorf("the calendar and email configs disagree on providers: %v/%v vs %v/%v",
			cal.Email.ProviderList(), cal.Email.MicrosoftEnabled(), em.Email.ProviderList(), em.Email.MicrosoftEnabled())
	}
}
