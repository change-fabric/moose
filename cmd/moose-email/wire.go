package main

import (
	"context"
	"encoding/base64"
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
	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/accounts/accountprov"
	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/oauthwire"
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

// wiring is everything the connect page and the OAuth server need, built from
// config.
type wiring struct {
	// store is the real account store: the connect page, token refresh and the
	// OAuth server use it and see every account's true status.
	store store.Store
	// tools is store with accounts of a switched-off provider reading as
	// provider_disabled. The MCP tools and the send engine use it, so those
	// accounts are hidden from every tool while their credentials are kept.
	tools   store.Store
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

	var secret oauthwire.SecretFunc
	if e.GoogleSecretARN != "" || (e.MicrosoftEnabled() && e.Microsoft.ClientSecretRef != "") {
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		secret = oauthwire.SecretsManager(secretsmanager.NewFromConfig(ac))
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
	adapters, configs, google, err := buildAdapters(ctx, *e, secret)
	if err != nil {
		return fail(err)
	}
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
	deps := connect.Deps{
		Store: st, Seal: seal, Adapters: adapters, Auth: auth, Approver: enableAp,
		BaseURL: e.BaseURL, InsecureCookies: base.Scheme != "https", Log: log,
		CalendarEnabled: e.Calendar.Enabled,
	}
	if google != nil {
		deps.OAuth, deps.Profile = google, connect.GmailProfile()
	}
	srv, err := connect.New(deps)
	if err != nil {
		return fail(err)
	}
	gated := store.GateProviders(st, enabledProviders(*e))
	w := &wiring{store: st, tools: gated, connect: srv,
		providers: &accountprov.Source{Store: st, Seal: seal, OAuth: google, Configs: configs, Log: log}}
	var cursorBase []byte
	if e.OAuthEnabled() {
		if w.oauth, err = buildOAuth(*e, key, st, seal, log, base.Scheme != "https"); err != nil {
			return fail(err)
		}
		w.send = &sendflow.Service{
			Store: gated, Seal: seal, Approver: client, Providers: w.providers,
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
	w.cursorKey = oauthwire.DeriveKey(cursorBase, "moose-email/search-cursor/v1")
	return w, nil
}

// enabledProviders reports which providers are on: Google when email.providers
// lists it, Microsoft when it is listed and email.microsoft.enabled is true.
func enabledProviders(e config.Email) func(provider string) bool {
	return func(p string) bool {
		switch p {
		case store.ProviderGoogle:
			return e.HasProvider(config.MailProviderGoogle)
		case store.ProviderMicrosoft:
			return e.MicrosoftEnabled()
		}
		return false
	}
}

// buildAdapters registers one connect adapter per enabled provider in
// email.providers and returns the OAuth client of each for token refresh. The
// Google client is returned on its own too: it is the connect page's built-in
// provider, nil when Google is not offered.
func buildAdapters(ctx context.Context, e config.Email, secret oauthwire.SecretFunc) (adapters []connect.Adapter, configs map[string]*oauth2.Config, google *oauth2.Config, err error) {
	configs = map[string]*oauth2.Config{}
	if e.HasProvider(config.MailProviderGoogle) {
		id, sec, err := oauthwire.GoogleCredentials(ctx, e, secret)
		if err != nil {
			return nil, nil, nil, err
		}
		google = connect.GoogleConfig(id, sec, e.BaseURL)
		configs[store.ProviderGoogle] = google
	}
	if e.MicrosoftEnabled() {
		id, sec, err := oauthwire.MicrosoftCredentials(ctx, e, secret)
		if err != nil {
			return nil, nil, nil, err
		}
		ms := connect.MicrosoftConfig(id, sec, e.BaseURL)
		adapters = append(adapters, connect.NewMicrosoftAdapter(ms))
		configs[store.ProviderMicrosoft] = ms
	}
	return adapters, configs, google, nil
}

// approverKey returns the HMAC key shared with the approver's service API, from
// the config value or the Secrets Manager secret it names.
func approverKey(ctx context.Context, e config.Email, secret oauthwire.SecretFunc, needAWS func() (aws.Config, error)) ([]byte, error) {
	key := []byte(e.ApproverSecret)
	if e.ApproverSecretARN != "" {
		if secret == nil {
			ac, err := needAWS()
			if err != nil {
				return nil, err
			}
			secret = oauthwire.SecretsManager(secretsmanager.NewFromConfig(ac))
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

// buildOAuth builds the authorization server: an HMAC client for the approver's
// service API (URL from email.approver_url, defaulting to the public origin) and
// the pinned receipt key.
func buildOAuth(e config.Email, key []byte, st store.Store, seal envelope.Sealer, log *slog.Logger, insecure bool) (*oauthas.Server, error) {
	return oauthas.New(oauthas.Deps{
		Store: st, Seal: seal, Approver: &api.Client{BaseURL: serviceBase(e), Secret: key},
		ReceiptKey: []byte(e.ReceiptPublicKey), RPID: e.RPID, Owner: e.Owner,
		BaseURL: e.BaseURL, InsecureCookies: insecure, Log: log,
		Resources: oauthResources(e),
	})
}

// oauthResources lists the resources the authorization server issues tokens
// for: email always, calendar only while email.calendar.enabled is on. With it
// off a /authorize request naming the calendar resource is refused
// (invalid_target) and no calendar token can exist.
func oauthResources(e config.Email) []oauthas.Resource {
	rs := []oauthas.Resource{oauthas.EmailResource}
	if e.Calendar.Enabled {
		rs = append(rs, oauthas.CalendarResource)
	}
	return rs
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

func defaultAWS(ctx context.Context) (aws.Config, error) { return awsconfig.LoadDefaultConfig(ctx) }
