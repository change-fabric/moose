// Package oauthwire holds the wiring both MCP servers share to reach the
// tenant's OAuth clients: reading a Secrets Manager secret and turning it, or
// the dev config values, into the Google and Microsoft client credentials.
package oauthwire

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/change-fabric/moose/internal/config"
)

// SecretFunc reads a Secrets Manager secret.
type SecretFunc func(ctx context.Context, arn string) ([]byte, error)

func SecretsManager(c *secretsmanager.Client) SecretFunc {
	return func(ctx context.Context, arn string) ([]byte, error) {
		out, err := c.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
		if err != nil {
			return nil, err
		}
		if out.SecretString != nil {
			return []byte(*out.SecretString), nil
		}
		if out.SecretBinary != nil {
			return out.SecretBinary, nil
		}
		return nil, errors.New("secret has no value")
	}
}

// GoogleCredentials returns the tenant's Google OAuth client id and secret from
// the Secrets Manager secret (JSON {"client_id", "client_secret"}) or, in dev,
// from the environment.
func GoogleCredentials(ctx context.Context, e config.Email, secret SecretFunc) (id, sec string, err error) {
	if e.GoogleSecretARN == "" {
		return e.GoogleClientID, e.GoogleClientSecret, nil
	}
	if secret == nil {
		return "", "", errors.New("google oauth secret: no secret reader")
	}
	raw, err := secret(ctx, e.GoogleSecretARN)
	if err != nil {
		return "", "", fmt.Errorf("google oauth secret: %w", err)
	}
	var v struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", "", errors.New("google oauth secret is not JSON with client_id and client_secret")
	}
	if v.ClientID == "" || v.ClientSecret == "" {
		return "", "", errors.New("google oauth secret lacks client_id or client_secret")
	}
	return v.ClientID, v.ClientSecret, nil
}

// MicrosoftCredentials returns the Entra app's client id (email.microsoft.client_id)
// and secret, from the Secrets Manager secret client_secret_ref names (JSON
// {"client_secret": ...}, or the bare secret) or, in dev, from the environment.
func MicrosoftCredentials(ctx context.Context, e config.Email, secret SecretFunc) (id, sec string, err error) {
	m := e.Microsoft
	if m.ClientSecretRef == "" {
		return m.ClientID, m.ClientSecret, nil
	}
	if secret == nil {
		return "", "", errors.New("microsoft oauth secret: no secret reader")
	}
	raw, err := secret(ctx, m.ClientSecretRef)
	if err != nil {
		return "", "", fmt.Errorf("microsoft oauth secret: %w", err)
	}
	sec = strings.TrimSpace(string(raw))
	if strings.HasPrefix(sec, "{") {
		var v struct {
			ClientSecret string `json:"client_secret"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", "", errors.New("microsoft oauth secret is not JSON with client_secret")
		}
		sec = v.ClientSecret
	}
	if sec == "" {
		return "", "", errors.New("microsoft oauth secret has no client_secret")
	}
	return m.ClientID, sec, nil
}

// DeriveKey makes a purpose-bound key from base so one secret never signs two
// kinds of thing. An empty base gives a random key, which suits one process only.
func DeriveKey(base []byte, purpose string) []byte {
	if len(base) == 0 {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		return k
	}
	m := hmac.New(sha256.New, base)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}
