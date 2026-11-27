package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/change-fabric/moose/internal/accounts/accountprov"
	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	emaildynamo "github.com/change-fabric/moose/internal/accounts/store/dynamo"
	emailsqlite "github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/email/attach"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/oauthas"
)

// mount is one extra route the HTTP handler serves.
type mount struct {
	pattern string
	handler http.Handler
}

// secretFunc reads a Secrets Manager secret.
type secretFunc func(ctx context.Context, arn string) ([]byte, error)

// wiring is everything the Google connect page and the OAuth server need, built
// from config.
type wiring struct {
	store   store.Store
	connect *connect.Server
	// oauth is nil unless the email: section configures the OAuth server.
	oauth *oauthas.Server
	// providers builds the mailbox provider for a stored account, with the
	// per-account access-token cache.
	providers *accountprov.Source
	// attach serves email_get_attachment; nil when email.attach_bucket is unset.
	attach *attach.Service
	// cursorKey signs search cursors; every Lambda instance derives the same one.
	cursorKey []byte
	// send is the send engine; nil unless the OAuth server and its service
	// client are configured.
	send *sendflow.Service
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
	google := connect.GoogleConfig(id, sec, e.BaseURL)
	// The service client exists only where the OAuth server does: both need the
	// HMAC key shared with the approval service. Without it, Enable send says
	// it is unavailable.
	var (
		key      []byte
		client   *api.Client
		enableAp connect.Approver
	)
	if e.OAuthEnabled() {
		if key, err = approverKey(ctx, *e, secret, needAWS); err != nil {
			return fail(err)
		}
		client = &api.Client{BaseURL: serviceBase(*e), Secret: key}
		enableAp = sendflow.ConnectApprover{Approver: client}
	}
	srv, err := connect.New(connect.Deps{
		Store: st, Seal: seal, OAuth: google,
		Profile: connect.GmailProfile(), Auth: auth, Approver: enableAp,
		BaseURL: e.BaseURL, InsecureCookies: base.Scheme != "https", Log: log,
	})
	if err != nil {
		return fail(err)
	}
	w := &wiring{store: st, connect: srv, providers: &accountprov.Source{Store: st, Seal: seal, OAuth: google}}
	var cursorBase []byte
	if e.OAuthEnabled() {
		if w.oauth, err = buildOAuth(*e, key, st, seal, log, base.Scheme != "https"); err != nil {
			return fail(err)
		}
		w.send = &sendflow.Service{
			Store: st, Seal: seal, Approver: client, Providers: w.providers,
			ReceiptKey: []byte(e.ReceiptPublicKey), RPID: e.RPID, Live: e.Send.Live, Log: log,
		}
		cursorBase = key
	} else {
		cursorBase = []byte(e.DevKey)
	}
	if e.AttachBucket != "" {
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		w.attach = &attach.Service{Store: attach.NewS3(s3.NewFromConfig(ac), e.AttachBucket, e.KMSKeyID)}
	}
	w.cursorKey = deriveKey(cursorBase, "moose-email/search-cursor/v1")
	return w, nil
}

// approverKey returns the HMAC key shared with the approver's service API, from
// the config value or the Secrets Manager secret it names.
func approverKey(ctx context.Context, e config.Email, secret secretFunc, needAWS func() (aws.Config, error)) ([]byte, error) {
	key := []byte(e.ApproverSecret)
	if e.ApproverSecretARN != "" {
		if secret == nil {
			ac, err := needAWS()
			if err != nil {
				return nil, err
			}
			secret = secretsManager(secretsmanager.NewFromConfig(ac))
		}
		var err error
		if key, err = secret(ctx, e.ApproverSecretARN); err != nil {
			return nil, fmt.Errorf("approver service secret: %w", err)
		}
	}
	if len(key) < 32 {
		return nil, errors.New("the approver service secret must be at least 32 bytes")
	}
	return key, nil
}

// deriveKey makes a purpose-bound key from base so one secret never signs two
// kinds of thing. An empty base gives a random key, which suits one process only.
func deriveKey(base []byte, purpose string) []byte {
	if len(base) == 0 {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		return k
	}
	m := hmac.New(sha256.New, base)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

// buildOAuth builds the authorization server: an HMAC client for the approver's
// service API (URL from email.approver_url, defaulting to the public origin) and
// the pinned receipt key.
func buildOAuth(e config.Email, key []byte, st store.Store, seal envelope.Sealer, log *slog.Logger, insecure bool) (*oauthas.Server, error) {
	return oauthas.New(oauthas.Deps{
		Store: st, Seal: seal, Approver: &api.Client{BaseURL: serviceBase(e), Secret: key},
		ReceiptKey: []byte(e.ReceiptPublicKey), RPID: e.RPID, Owner: e.Owner,
		BaseURL: e.BaseURL, InsecureCookies: insecure, Log: log,
	})
}

// serviceBase is where the approval service's API lives: email.approver_url,
// defaulting to the public origin.
func serviceBase(e config.Email) string {
	if e.ApproverURL != "" {
		return e.ApproverURL
	}
	return e.BaseURL
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
