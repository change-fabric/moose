package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/email/connect"
	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/store"
	emaildynamo "github.com/change-fabric/moose/internal/email/store/dynamo"
	emailsqlite "github.com/change-fabric/moose/internal/email/store/sqlite"
)

// mount is one extra route the HTTP handler serves.
type mount struct {
	pattern string
	handler http.Handler
}

// secretFunc reads a Secrets Manager secret.
type secretFunc func(ctx context.Context, arn string) ([]byte, error)

// wiring is everything the Google connect page needs, built from config.
type wiring struct {
	store   store.Store
	connect *connect.Server
}

// buildConnect opens the email store and the token sealer and returns the
// connect page. It returns nil when email.base_url is unset, which leaves the
// server as a bare MCP endpoint. aws is loaded lazily, only when the config
// names a DynamoDB table, a KMS key or a secret.
func buildConnect(ctx context.Context, cfg config.Config, log *slog.Logger, loadAWS func(context.Context) (aws.Config, error)) (*wiring, error) {
	e := cfg.Email
	if e == nil || e.BaseURL == "" {
		return nil, nil
	}
	var awsCfg *aws.Config
	needAWS := func() (aws.Config, error) {
		if awsCfg == nil {
			c, err := loadAWS(ctx)
			if err != nil {
				return aws.Config{}, fmt.Errorf("aws config: %w", err)
			}
			awsCfg = &c
		}
		return *awsCfg, nil
	}

	st, err := openStore(ctx, cfg, needAWS)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*wiring, error) {
		_ = st.Close()
		return nil, err
	}

	var seal envelope.Sealer
	if e.KMSKeyID != "" {
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		seal = envelope.NewKMS(kms.NewFromConfig(ac), e.KMSKeyID)
	} else {
		key, err := base64.StdEncoding.DecodeString(e.DevKey)
		if err != nil {
			return fail(fmt.Errorf("MOOSE_EMAIL_DEV_KEY: %w", err))
		}
		if seal, err = envelope.NewLocal(key); err != nil {
			return fail(err)
		}
	}

	var secret secretFunc
	if e.GoogleSecretARN != "" {
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		secret = secretsManager(secretsmanager.NewFromConfig(ac))
	}
	id, sec, err := googleCredentials(ctx, *e, secret)
	if err != nil {
		return fail(err)
	}

	var auth connect.Authenticator = connect.SessionAuth{Store: st}
	if cfg.Env == config.EnvDev && cfg.DevIdentity != "" {
		log.Warn("moose-email: dev identity signs every connect request in as " + cfg.DevIdentity)
		auth = connect.StaticOwner(cfg.DevIdentity)
	}
	base, err := url.Parse(e.BaseURL)
	if err != nil {
		return fail(err)
	}
	srv, err := connect.New(connect.Deps{
		Store: st, Seal: seal, OAuth: connect.GoogleConfig(id, sec, e.BaseURL),
		Profile: connect.GmailProfile(), Auth: auth,
		BaseURL: e.BaseURL, InsecureCookies: base.Scheme != "https", Log: log,
		// Approver stays nil until moose-approver exists; the page then says
		// that Enable send is unavailable.
	})
	if err != nil {
		return fail(err)
	}
	return &wiring{store: st, connect: srv}, nil
}

func openStore(ctx context.Context, cfg config.Config, needAWS func() (aws.Config, error)) (store.Store, error) {
	switch cfg.Store.Kind {
	case config.StoreSQLite:
		if dir := filepath.Dir(cfg.Store.SQLitePath); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("store: create %s: %w", dir, err)
			}
		}
		return emailsqlite.OpenStore(ctx, cfg.Store.SQLitePath)
	case config.StoreDynamo:
		ac, err := needAWS()
		if err != nil {
			return nil, err
		}
		return emaildynamo.NewStore(dynamodb.NewFromConfig(ac), cfg.Store.DynamoTable), nil
	}
	return nil, fmt.Errorf("store: unknown kind %q", cfg.Store.Kind)
}

func secretsManager(c *secretsmanager.Client) secretFunc {
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

// googleCredentials returns the tenant's Google OAuth client id and secret from
// the Secrets Manager secret (JSON {"client_id", "client_secret"}) or, in dev,
// from the environment.
func googleCredentials(ctx context.Context, e config.Email, secret secretFunc) (id, sec string, err error) {
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

func defaultAWS(ctx context.Context) (aws.Config, error) { return awsconfig.LoadDefaultConfig(ctx) }
