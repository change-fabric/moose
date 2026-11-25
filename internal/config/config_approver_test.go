package config

import (
	"strings"
	"testing"
)

const approverBase = "approver: {rp_id: moose.example.org, origin: 'https://moose.example.org', owner: pat%s}\n"

var hmacSecret = strings.Repeat("h", 40)

func TestApproverServeDevSettings(t *testing.T) {
	cfg := load(t, strings.Replace(approverBase, "%s", "", 1), map[string]string{
		"MOOSE_APPROVER_DEV_KEY": devKey, "MOOSE_APPROVER_HMAC_SECRET": hmacSecret,
	})
	if err := cfg.ValidateApproverServe(); err != nil {
		t.Fatal(err)
	}
	if cfg.Approver.DevKey != devKey || cfg.Approver.HMACSecret != hmacSecret {
		t.Fatalf("env settings not applied: %+v", cfg.Approver)
	}
}

func TestApproverServeRequirements(t *testing.T) {
	good := map[string]string{"MOOSE_APPROVER_DEV_KEY": devKey, "MOOSE_APPROVER_HMAC_SECRET": hmacSecret}
	with := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for k, v := range over {
			if v == "" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	cases := []struct {
		name, section string
		env           map[string]string
		want          string
	}{
		{"no owner", "approver: {rp_id: x, origin: 'https://x'}\n", good, "approver.owner"},
		{"origin with a path", "approver: {rp_id: x, origin: 'https://x/approve', owner: pat}\n", good, "no path"},
		{"no preview key", strings.Replace(approverBase, "%s", "", 1), with(map[string]string{"MOOSE_APPROVER_DEV_KEY": ""}), "approver.kms_key_id"},
		{"both preview keys", strings.Replace(approverBase, "%s", ", kms_key_id: k", 1), good, "not both"},
		{"short dev key", strings.Replace(approverBase, "%s", "", 1), with(map[string]string{"MOOSE_APPROVER_DEV_KEY": "c2hvcnQ="}), "32 bytes"},
		{"no secret", strings.Replace(approverBase, "%s", "", 1), with(map[string]string{"MOOSE_APPROVER_HMAC_SECRET": ""}), "MOOSE_APPROVER_HMAC_SECRET_ARN"},
		{"short secret", strings.Replace(approverBase, "%s", "", 1), with(map[string]string{"MOOSE_APPROVER_HMAC_SECRET": "short"}), "at least 32"},
		{"secret and arn", strings.Replace(approverBase, "%s", "", 1), with(map[string]string{"MOOSE_APPROVER_HMAC_SECRET_ARN": "arn:x"}), "not both"},
		{"two signing keys", strings.Replace(approverBase, "%s", ", signing_key_id: k", 1), with(map[string]string{"MOOSE_APPROVER_DEV_SIGNING_KEY": "pem"}), "signing_key_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, load(t, tc.section, tc.env).ValidateApproverServe(), tc.want)
		})
	}
}

func TestApproverServeProdRefusesDevSettings(t *testing.T) {
	prodSection := "env: prod\n" + strings.Replace(approverBase, "%s", ", kms_key_id: k, signing_key_id: s", 1)
	ok := load(t, prodSection, map[string]string{"MOOSE_APPROVER_HMAC_SECRET_ARN": "arn:x"})
	if err := ok.ValidateApproverServe(); err != nil {
		t.Fatalf("a complete prod config: %v", err)
	}
	wantErr(t, load(t, "env: prod\n"+strings.Replace(approverBase, "%s", ", kms_key_id: k", 1), map[string]string{"MOOSE_APPROVER_HMAC_SECRET_ARN": "arn:x"}).ValidateApproverServe(), "signing_key_id is required")
	wantErr(t, load(t, "env: prod\n"+strings.Replace(approverBase, "%s", ", signing_key_id: s", 1), map[string]string{"MOOSE_APPROVER_DEV_KEY": devKey, "MOOSE_APPROVER_HMAC_SECRET_ARN": "arn:x"}).ValidateApproverServe(), "refuses")
	wantErr(t, load(t, prodSection, map[string]string{"MOOSE_APPROVER_HMAC_SECRET": hmacSecret}).ValidateApproverServe(), "refuses")
}

func TestApproverSettingsComeFromTheEnvironment(t *testing.T) {
	cfg := load(t, "approver: {}\n", map[string]string{
		"MOOSE_APPROVER_RP_ID": "moose.example.org", "MOOSE_APPROVER_ORIGIN": "https://moose.example.org", "MOOSE_APPROVER_OWNER": "pat",
		"MOOSE_APPROVER_SIGNING_KEY_ID": "alias/receipts", "MOOSE_APPROVER_KMS_KEY_ID": "alias/seal", "MOOSE_APPROVER_HMAC_SECRET_ARN": "arn:x",
	})
	a := cfg.Approver
	if a.RPID != "moose.example.org" || a.Origin != "https://moose.example.org" || a.Owner != "pat" ||
		a.SigningKeyID != "alias/receipts" || a.KMSKeyID != "alias/seal" || a.HMACSecretARN != "arn:x" {
		t.Fatalf("env overrides not applied: %+v", a)
	}
}
