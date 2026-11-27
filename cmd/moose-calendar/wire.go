package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/change-fabric/moose/internal/accounts/store"
	emaildynamo "github.com/change-fabric/moose/internal/accounts/store/dynamo"
	emailsqlite "github.com/change-fabric/moose/internal/accounts/store/sqlite"
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
	if e.BaseURL == "" {
		log.Warn("moose-calendar: no email.base_url, serving /calendar/mcp without OAuth as the dev identity " + cfg.DevIdentity)
		return w, nil
	}
	v := oauthas.NewVerifier(st, e.BaseURL, oauthas.CalendarResource, nil)
	w.protect = v.Protect
	return w, nil
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
