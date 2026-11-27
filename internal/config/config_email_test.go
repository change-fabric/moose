package config

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

var devKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

func TestEmailConnectIsOptionalAndOffByDefault(t *testing.T) {
	cfg := load(t, "email: {provider: gmail}\n", nil)
	if err := cfg.ValidateEmail(); err != nil {
		t.Fatalf("no base_url means no connect page and no extra requirements: %v", err)
	}
}

func TestEmailConnectDevSettings(t *testing.T) {
	cfg := load(t, "email: {provider: gmail, base_url: 'http://localhost:8080'}\n", map[string]string{
		"MOOSE_EMAIL_DEV_KEY": devKey, "MOOSE_GOOGLE_CLIENT_ID": "id", "MOOSE_GOOGLE_CLIENT_SECRET": "secret",
	})
	if err := cfg.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	if cfg.Email.DevKey != devKey || cfg.Email.GoogleClientID != "id" {
		t.Fatalf("env settings not applied: %+v", cfg.Email)
	}
}

func TestEmailConnectRequirements(t *testing.T) {
	const section = "email: {provider: gmail, base_url: 'https://moose.example.org'%s}\n"
	cases := []struct {
		name, kms string
		env       map[string]string
		want      string
	}{
		{"no key", "", map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x"}, "email.kms_key_id"},
		{"both keys", ", kms_key_id: k", map[string]string{"MOOSE_EMAIL_DEV_KEY": devKey, "MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x"}, "not both"},
		{"short dev key", "", map[string]string{"MOOSE_EMAIL_DEV_KEY": "c2hvcnQ=", "MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x"}, "32 bytes"},
		{"no google client", ", kms_key_id: k", nil, "MOOSE_GOOGLE_OAUTH_SECRET_ARN"},
		{"half a client", ", kms_key_id: k", map[string]string{"MOOSE_GOOGLE_CLIENT_ID": "id"}, "MOOSE_GOOGLE_OAUTH_SECRET_ARN"},
		{"secret and pair", ", kms_key_id: k", map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x", "MOOSE_GOOGLE_CLIENT_ID": "id", "MOOSE_GOOGLE_CLIENT_SECRET": "s"}, "not both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, load(t, strings.Replace(section, "%s", tc.kms, 1), tc.env).ValidateEmail(), tc.want)
		})
	}
	ok := load(t, strings.Replace(section, "%s", ", kms_key_id: k", 1), map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x"})
	if err := ok.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
}

func TestEmailBaseURLAndKeyComeFromTheEnvironment(t *testing.T) {
	cfg := load(t, "email: {provider: gmail}\n", map[string]string{
		"MOOSE_EMAIL_BASE_URL": "https://moose.example.org", "MOOSE_EMAIL_KMS_KEY_ID": "alias/moose", "MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x"})
	if cfg.Email.BaseURL != "https://moose.example.org" || cfg.Email.KMSKeyID != "alias/moose" {
		t.Fatalf("env overrides not applied: %+v", cfg.Email)
	}
	if err := cfg.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
}

func TestEmailBaseURLShape(t *testing.T) {
	env := map[string]string{"MOOSE_EMAIL_DEV_KEY": devKey, "MOOSE_GOOGLE_CLIENT_ID": "i", "MOOSE_GOOGLE_CLIENT_SECRET": "s"}
	for _, bad := range []string{"moose.example.org", "ftp://x", "https://x/path", "https://x?q=1"} {
		wantErr(t, load(t, "email: {provider: gmail, base_url: '"+bad+"'}\n", env).ValidateEmail(), "email.base_url")
	}
	if err := load(t, "email: {provider: gmail, base_url: 'https://x.example/'}\n", env).ValidateEmail(); err != nil {
		t.Fatalf("a trailing slash is fine: %v", err)
	}
}

func TestEmailConnectRefusalsInProd(t *testing.T) {
	cfg := load(t, "env: prod\nstore: {kind: dynamo, dynamo_table: t}\nemail: {provider: gmail, base_url: 'http://x.example', kms_key_id: k}\n",
		map[string]string{"MOOSE_GOOGLE_CLIENT_ID": "i", "MOOSE_GOOGLE_CLIENT_SECRET": "s"})
	err := cfg.ValidateEmail()
	wantErr(t, err, "must start with https://")
	wantErr(t, err, "MOOSE_GOOGLE_CLIENT_SECRET is refused")
	dev := load(t, "env: prod\nstore: {kind: dynamo, dynamo_table: t}\nemail: {provider: gmail, base_url: 'https://x.example'}\n",
		map[string]string{"MOOSE_EMAIL_DEV_KEY": devKey, "MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:x"})
	wantErr(t, dev.ValidateEmail(), "MOOSE_EMAIL_DEV_KEY is refused")
}

const testPubKeyPEM = "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n"

func oauthSection(extra string) string {
	return "email: {provider: gmail, base_url: 'https://x.example', kms_key_id: k, owner: pat, rp_id: x.example, receipt_public_key: \"" +
		strings.ReplaceAll(testPubKeyPEM, "\n", `\n`) + "\"" + extra + "}\n"
}

func TestEmailOAuthIsOffUntilConfigured(t *testing.T) {
	cfg := load(t, "email: {provider: gmail, base_url: 'http://localhost:8080'}\n", map[string]string{
		"MOOSE_EMAIL_DEV_KEY": devKey, "MOOSE_GOOGLE_CLIENT_ID": "i", "MOOSE_GOOGLE_CLIENT_SECRET": "s"})
	if err := cfg.ValidateEmail(); err != nil || cfg.Email.OAuthEnabled() {
		t.Fatalf("OAuth must be opt-in outside prod: %v", err)
	}
}

func TestEmailOAuthRequirements(t *testing.T) {
	google := map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:g"}
	with := func(k, v string) map[string]string {
		m := map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:g"}
		m[k] = v
		return m
	}
	ok := load(t, oauthSection(""), with("MOOSE_EMAIL_APPROVER_SECRET_ARN", "arn:a"))
	if err := ok.ValidateEmail(); err != nil || !ok.Email.OAuthEnabled() {
		t.Fatalf("complete OAuth config: %v", err)
	}
	wantErr(t, load(t, oauthSection(""), google).ValidateEmail(), "MOOSE_EMAIL_APPROVER_SECRET_ARN")
	wantErr(t, load(t, oauthSection(""), with("MOOSE_EMAIL_APPROVER_SECRET", "short")).ValidateEmail(), "at least 32")
	both := with("MOOSE_EMAIL_APPROVER_SECRET", strings.Repeat("s", 32))
	both["MOOSE_EMAIL_APPROVER_SECRET_ARN"] = "arn:a"
	wantErr(t, load(t, oauthSection(""), both).ValidateEmail(), "not both")
	wantErr(t, load(t, strings.Replace(oauthSection(""), "owner: pat, ", "", 1), with("MOOSE_EMAIL_APPROVER_SECRET_ARN", "arn:a")).ValidateEmail(), "email.owner")
	wantErr(t, load(t, strings.Replace(oauthSection(""), "rp_id: x.example, ", "", 1), with("MOOSE_EMAIL_APPROVER_SECRET_ARN", "arn:a")).ValidateEmail(), "email.rp_id")
	wantErr(t, load(t, strings.Replace(oauthSection(""), "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE", "x", 1), with("MOOSE_EMAIL_APPROVER_SECRET_ARN", "arn:a")).ValidateEmail(), "receipt_public_key")
	wantErr(t, load(t, oauthSection(", approver_url: 'nope'"), with("MOOSE_EMAIL_APPROVER_SECRET_ARN", "arn:a")).ValidateEmail(), "approver_url")
	// A dev HMAC secret is refused in prod.
	prod := load(t, "env: prod\nstore: {kind: dynamo, dynamo_table: t}\n"+oauthSection(""), with("MOOSE_EMAIL_APPROVER_SECRET", strings.Repeat("s", 32)))
	wantErr(t, prod.ValidateEmail(), "MOOSE_EMAIL_APPROVER_SECRET is refused")
}

func TestEmailProdWithAPublicOriginNeedsOAuth(t *testing.T) {
	cfg := load(t, "env: prod\nstore: {kind: dynamo, dynamo_table: t}\nemail: {provider: gmail, base_url: 'https://x.example', kms_key_id: k}\n",
		map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:g"})
	wantErr(t, cfg.ValidateEmail(), "a public /mcp needs the OAuth server")
}

func TestEmailProvidersDefaultToGoogleAndValidate(t *testing.T) {
	cfg := load(t, "email: {provider: gmail}\n", nil)
	if got := cfg.Email.ProviderList(); len(got) != 1 || got[0] != "google" {
		t.Fatalf("default providers = %v, want [google]", got)
	}
	if cfg.Email.MicrosoftEnabled() {
		t.Fatal("Microsoft must default to off")
	}
	for name, tc := range map[string]struct{ body, want string }{
		"unknown":    {"email: {provider: gmail, providers: [google, imap]}\n", `"imap" is not google or microsoft`},
		"duplicate":  {"email: {provider: gmail, providers: [google, google]}\n", "twice"},
		"empty":      {"email: {provider: gmail, providers: []}\n", "at least one"},
		"not listed": {"email: {provider: gmail, providers: [google], microsoft: {enabled: true}}\n", "does not list microsoft"},
	} {
		t.Run(name, func(t *testing.T) { wantErr(t, load(t, tc.body, nil).ValidateEmail(), tc.want) })
	}
	both := load(t, "email: {provider: gmail, providers: [google, microsoft]}\n", nil)
	if err := both.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	if both.Email.MicrosoftEnabled() {
		t.Fatal("listed but not enabled must stay off")
	}
}

func TestEmailMicrosoftNeedsItsOwnSecretsOnlyWhenEnabled(t *testing.T) {
	const section = "email: {provider: gmail, base_url: 'https://moose.example.org', kms_key_id: k, providers: [%s], microsoft: {enabled: %s%s}}\n"
	body := func(providers, enabled, extra string) string { return fmt.Sprintf(section, providers, enabled, extra) }
	google := map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:g"}

	// Listed and off: no Microsoft secret is needed.
	if err := load(t, body("google, microsoft", "false", ""), google).ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	// Enabled: a client id and exactly one secret source.
	wantErr(t, load(t, body("google, microsoft", "true", ""), google).ValidateEmail(), "email.microsoft.client_id")
	wantErr(t, load(t, body("google, microsoft", "true", ", client_id: ms"), google).ValidateEmail(), "client_secret_ref")
	both := map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:g", "MOOSE_MICROSOFT_CLIENT_SECRET": "s"}
	wantErr(t, load(t, body("google, microsoft", "true", ", client_id: ms, client_secret_ref: arn:m"), both).ValidateEmail(), "not both")
	ok := load(t, body("google, microsoft", "true", ", client_id: ms, client_secret_ref: arn:m"), google)
	if err := ok.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	if !ok.Email.MicrosoftEnabled() {
		t.Fatal("listed and enabled must be on")
	}
	// A dev secret is accepted in dev and refused in prod.
	dev := map[string]string{"MOOSE_GOOGLE_OAUTH_SECRET_ARN": "arn:g", "MOOSE_MICROSOFT_CLIENT_SECRET": "s"}
	if err := load(t, body("google, microsoft", "true", ", client_id: ms"), dev).ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	prod := "env: prod\n" + body("google, microsoft", "true", ", client_id: ms")
	wantErr(t, load(t, prod, dev).ValidateEmail(), "refused when env is prod")
}

func TestEmailGoogleSecretsAreNotNeededWithoutGoogle(t *testing.T) {
	const body = "email: {provider: gmail, base_url: 'https://moose.example.org', kms_key_id: k, providers: [microsoft], microsoft: {enabled: true, client_id: ms, client_secret_ref: 'arn:m'}}\n"
	if err := load(t, body, nil).ValidateEmail(); err != nil {
		t.Fatalf("a Microsoft-only deployment needs no Google client: %v", err)
	}
}

func TestEmailMicrosoftSettingsComeFromTheEnvironment(t *testing.T) {
	cfg := load(t, "email: {provider: gmail}\n", map[string]string{
		"MOOSE_MICROSOFT_CLIENT_ID": "ms-id", "MOOSE_MICROSOFT_OAUTH_SECRET_ARN": "arn:m", "MOOSE_MICROSOFT_CLIENT_SECRET": "s"})
	m := cfg.Email.Microsoft
	if m.ClientID != "ms-id" || m.ClientSecretRef != "arn:m" || m.ClientSecret != "s" {
		t.Fatalf("env overrides not applied: %+v", m)
	}
}
