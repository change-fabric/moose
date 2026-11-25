package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func testPublicKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// These maps mirror the environment deploy/terraform/email/lambdas.tf gives each
// function. If a variable is renamed on one side only, the Lambda fails to start
// in production, so this fails first.
func TestEmailLambdaConfigAcceptsTheEnvironmentTerraformSets(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"MOOSE_CONFIG":                    "../../deploy/lambda/moose-email.yaml",
		"MOOSE_ENV":                       "prod",
		"MOOSE_DYNAMO_TABLE":              "moose-email-prod",
		"MOOSE_EMAIL_BASE_URL":            "https://moose.example.org",
		"MOOSE_EMAIL_KMS_KEY_ID":          "arn:aws:kms:us-east-1:111122223333:key/abc",
		"MOOSE_GOOGLE_OAUTH_SECRET_ARN":   "arn:aws:secretsmanager:us-east-1:111122223333:secret:g",
		"MOOSE_EMAIL_OWNER":               "patrick",
		"MOOSE_EMAIL_RP_ID":               "moose.example.org",
		"MOOSE_EMAIL_RECEIPT_PUBLIC_KEY":  testPublicKeyPEM(t),
		"MOOSE_EMAIL_APPROVER_SECRET_ARN": "arn:aws:secretsmanager:us-east-1:111122223333:secret:h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateEmail(); err != nil {
		t.Fatal(err)
	}
	// Live sending was turned on deliberately after the end-to-end approval
	// check; every send still needs a passkey receipt and fits the hourly cap.
	if !cfg.Email.Send.Live {
		t.Fatal("email.send.live is expected on in the shipped config")
	}
	if !cfg.Email.OAuthEnabled() {
		t.Fatal("the OAuth server must be on for the public deployment")
	}
}

func TestApproverLambdaConfigAcceptsTheEnvironmentTerraformSets(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"MOOSE_CONFIG":                   "../../deploy/lambda/moose-approver.yaml",
		"MOOSE_ENV":                      "prod",
		"MOOSE_DYNAMO_TABLE":             "moose-approver-prod",
		"MOOSE_APPROVER_RP_ID":           "moose.example.org",
		"MOOSE_APPROVER_ORIGIN":          "https://moose.example.org",
		"MOOSE_APPROVER_OWNER":           "patrick",
		"MOOSE_APPROVER_SIGNING_KEY_ID":  "arn:aws:kms:us-east-1:111122223333:key/sig",
		"MOOSE_APPROVER_KMS_KEY_ID":      "arn:aws:kms:us-east-1:111122223333:key/seal",
		"MOOSE_APPROVER_HMAC_SECRET_ARN": "arn:aws:secretsmanager:us-east-1:111122223333:secret:h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateApproverServe(); err != nil {
		t.Fatal(err)
	}
}
