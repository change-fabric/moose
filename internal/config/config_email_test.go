package config

import (
	"encoding/base64"
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
