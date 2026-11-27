package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/kms"
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
	"github.com/change-fabric/moose/internal/calendar/inviteflow"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/oauthas"
)

// wiring is what the calendar server needs, built from config.
type wiring struct {
	// store is the shared account store: the same table moose-email writes, so
	// the accounts, grants and OAuth sessions are one set.
	store store.Store
	// tools is store with accounts of a switched-off provider reading as
	// provider_disabled, so the tools hide them while their credentials are kept.
	tools store.Store
	// providers builds the calendar provider for a stored account, with the
	// per-account access-token cache. It is nil when this server has no way to
	// open stored credentials (no KMS key and no dev key): the tools then say
	// calendar access is not configured.
	providers *accountprov.Source
	// invites is the approved-invite engine for events with attendees. It is nil
	// when the approval service is not configured here.
	invites *inviteflow.Service
	// cursorKey signs list_events cursors; every Lambda instance derives the same one.
	cursorKey []byte
	// protect wraps the MCP endpoint in bearer-token verification for the
	// calendar resource. It is nil only in dev without a public origin.
	protect func(http.Handler) http.Handler
}

// buildWiring opens the shared store and the calendar token verifier. aws is
// loaded lazily, only when the config names a DynamoDB table.
//
// The verifier needs the store alone: tokens are issued by moose-email's
// authorization server, which records each with the audience of the resource it
// was issued for, and this server accepts only the calendar audience. A token
// issued for email is refused here and a calendar token is refused there.
func buildWiring(ctx context.Context, cfg config.Config, log *slog.Logger, loadAWS func(context.Context) (aws.Config, error)) (*wiring, error) {
	e := cfg.Email
	st, err := openStore(ctx, cfg, loadAWS)
	if err != nil {
		return nil, err
	}
	w := &wiring{store: st, tools: store.GateProviders(st, enabledProviders(*e))}
	if err := w.buildProviders(ctx, *e, log, loadAWS); err != nil {
		_ = st.Close()
		return nil, err
	}
	if e.BaseURL == "" {
		log.Warn("moose-calendar: no email.base_url, serving /calendar/mcp without OAuth as the dev identity " + cfg.DevIdentity)
		return w, nil
	}
	v := oauthas.NewVerifier(st, e.BaseURL, oauthas.CalendarResource, nil)
	w.protect = v.Protect
	return w, nil
}

// buildProviders sets up the calendar provider source: the sealer that opens
// stored refresh tokens (KMS, or the dev key) and one OAuth client per enabled
// provider, which only token refresh uses. The calendar server never runs the
// connect flow, so no redirect is involved. With no sealer the source stays nil.
func (w *wiring) buildProviders(ctx context.Context, e config.Email, log *slog.Logger, loadAWS func(context.Context) (aws.Config, error)) error {
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
	var seal envelope.Sealer
	switch {
	case e.KMSKeyID != "":
		ac, err := needAWS()
		if err != nil {
			return err
		}
		seal = envelope.NewKMS(kms.NewFromConfig(ac), e.KMSKeyID)
	case e.DevKey != "":
		key, err := base64.StdEncoding.DecodeString(e.DevKey)
		if err != nil {
			return fmt.Errorf("MOOSE_EMAIL_DEV_KEY: %w", err)
		}
		if seal, err = envelope.NewLocal(key); err != nil {
			return err
		}
	default:
		log.Warn("moose-calendar: no email.kms_key_id or dev key, so stored credentials cannot be opened; the calendar read tools will report that calendar access is not configured")
		w.cursorKey = oauthwire.DeriveKey(nil, "")
		return nil
	}

	var secret oauthwire.SecretFunc
	if e.GoogleSecretARN != "" || (e.MicrosoftEnabled() && e.Microsoft.ClientSecretRef != "") {
		ac, err := needAWS()
		if err != nil {
			return err
		}
		secret = oauthwire.SecretsManager(secretsmanager.NewFromConfig(ac))
	}
	configs := map[string]*oauth2.Config{}
	var google *oauth2.Config
	var cursorBase []byte
	if e.DevKey != "" {
		cursorBase = []byte(e.DevKey)
	}
	if e.HasProvider(config.MailProviderGoogle) {
		id, sec, err := oauthwire.GoogleCredentials(ctx, e, secret)
		if err != nil {
			return err
		}
		google = connect.GoogleConfig(id, sec, e.BaseURL)
		configs[store.ProviderGoogle] = google
		if cursorBase == nil {
			cursorBase = []byte(sec)
		}
	}
	if e.MicrosoftEnabled() {
		id, sec, err := oauthwire.MicrosoftCredentials(ctx, e, secret)
		if err != nil {
			return err
		}
		configs[store.ProviderMicrosoft] = connect.MicrosoftConfig(id, sec, e.BaseURL)
		if cursorBase == nil {
			cursorBase = []byte(sec)
		}
	}
	w.providers = &accountprov.Source{Store: w.store, Seal: seal, OAuth: google, Configs: configs, Log: log}
	w.cursorKey = oauthwire.DeriveKey(cursorBase, "moose-calendar/list-cursor/v1")
	return w.buildInvites(ctx, e, seal, secret, log, needAWS)
}

// buildInvites sets up the approved-invite engine: an HMAC client for the
// approver's service API (URL from email.approver_url, defaulting to the public
// origin), the pinned receipt key and the relying party id, the same values
// moose-email's send engine uses. Without them (a dev setup with no approval
// service) the engine stays nil and events with attendees remain
// invites_not_enabled. email.calendar.invite.live decides whether an approved
// change reaches the provider or ends in would_apply.
func (w *wiring) buildInvites(ctx context.Context, e config.Email, seal envelope.Sealer, secret oauthwire.SecretFunc, log *slog.Logger, needAWS func() (aws.Config, error)) error {
	if !e.OAuthEnabled() || e.BaseURL == "" || (e.ApproverSecret == "" && e.ApproverSecretARN == "") {
		log.Warn("moose-calendar: the approval service is not configured (email.owner, rp_id, receipt_public_key, base_url and the approver secret), so events with attendees cannot be approved and stay invites_not_enabled")
		return nil
	}
	key := []byte(e.ApproverSecret)
	if e.ApproverSecretARN != "" {
		if secret == nil {
			ac, err := needAWS()
			if err != nil {
				return err
			}
			secret = oauthwire.SecretsManager(secretsmanager.NewFromConfig(ac))
		}
		var err error
		if key, err = secret(ctx, e.ApproverSecretARN); err != nil {
			return fmt.Errorf("approver service secret: %w", err)
		}
	}
	if len(key) < 32 {
		return errors.New("the approver service secret must be at least 32 bytes")
	}
	w.invites = &inviteflow.Service{
		Store: w.tools, Seal: seal, Approver: &api.Client{BaseURL: serviceBase(e), Secret: key}, Providers: w.providers,
		ReceiptKey: []byte(e.ReceiptPublicKey), RPID: e.RPID, Live: e.Calendar.Invite.Live, Log: log,
	}
	return nil
}

// serviceBase is where the approval service's API lives: email.approver_url,
// defaulting to the public origin.
func serviceBase(e config.Email) string {
	if e.ApproverURL != "" {
		return e.ApproverURL
	}
	return e.BaseURL
}

// enabledProviders reports which providers are on, as moose-email decides it:
// Google when email.providers lists it, Microsoft when it is listed and
// email.microsoft.enabled is true.
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

func openStore(ctx context.Context, cfg config.Config, loadAWS func(context.Context) (aws.Config, error)) (store.Store, error) {
	switch cfg.Store.Kind {
	case config.StoreSQLite:
		if dir := filepath.Dir(cfg.Store.SQLitePath); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("store: create %s: %w", dir, err)
			}
		}
		return emailsqlite.OpenStore(ctx, cfg.Store.SQLitePath)
	case config.StoreDynamo:
		ac, err := loadAWS(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
		return emaildynamo.NewStore(dynamodb.NewFromConfig(ac), cfg.Store.DynamoTable), nil
	}
	return nil, fmt.Errorf("store: unknown kind %q", cfg.Store.Kind)
}

func defaultAWS(ctx context.Context) (aws.Config, error) { return awsconfig.LoadDefaultConfig(ctx) }
