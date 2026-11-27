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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	estore "github.com/change-fabric/moose/internal/accounts/store"
	emaildynamo "github.com/change-fabric/moose/internal/accounts/store/dynamo"
	emailsqlite "github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/pages"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/approver/webauthn"
	"github.com/change-fabric/moose/internal/config"
)

// wiring is everything the approver serves with, built from config.
type wiring struct {
	cfg       config.Approver
	insecure  bool
	store     *store.Store
	signer    receipt.Signer
	publicKey func(context.Context) ([]byte, error)
	secret    []byte
	wa        *webauthn.Service
	log       *slog.Logger
}

// build opens the store, the preview sealer, the receipt signer and the API
// secret. AWS is loaded lazily, only when the config names a DynamoDB table, a
// KMS key or a secret.
func build(ctx context.Context, cfg config.Config, log *slog.Logger, loadAWS func(context.Context) (aws.Config, error)) (*wiring, error) {
	a := *cfg.Approver
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

	backend, err := openBackend(ctx, cfg, needAWS)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*wiring, error) {
		_ = backend.Close()
		return nil, err
	}

	var seal envelope.Sealer
	if a.KMSKeyID != "" {
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		seal = envelope.NewKMS(kms.NewFromConfig(ac), a.KMSKeyID)
	} else {
		key, err := base64.StdEncoding.DecodeString(a.DevKey)
		if err != nil {
			return fail(fmt.Errorf("MOOSE_APPROVER_DEV_KEY: %w", err))
		}
		if seal, err = envelope.NewLocal(key); err != nil {
			return fail(err)
		}
	}

	w := &wiring{cfg: a, insecure: cfg.Env != config.EnvProd, log: log}
	switch {
	case a.SigningKeyID != "":
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		ks := receipt.NewKMSSigner(kms.NewFromConfig(ac), a.SigningKeyID)
		w.signer, w.publicKey = ks, ks.PublicKeyPEM
	default:
		ls, err := devSigner(a.DevSigningKey, log)
		if err != nil {
			return fail(err)
		}
		w.signer = ls
		w.publicKey = func(context.Context) ([]byte, error) { return ls.PublicKeyPEM() }
	}

	if a.HMACSecretARN != "" {
		ac, err := needAWS()
		if err != nil {
			return fail(err)
		}
		out, err := secretsmanager.NewFromConfig(ac).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(a.HMACSecretARN)})
		if err != nil {
			return fail(fmt.Errorf("hmac secret: %w", err))
		}
		switch {
		case out.SecretString != nil:
			w.secret = []byte(*out.SecretString)
		case out.SecretBinary != nil:
			w.secret = out.SecretBinary
		default:
			return fail(errors.New("hmac secret has no value"))
		}
	} else {
		w.secret = []byte(a.HMACSecret)
	}
	if len(w.secret) < 32 {
		return fail(errors.New("the service API secret must be at least 32 bytes"))
	}

	w.store = store.New(backend, seal)
	w.wa, err = webauthn.New(webauthn.Config{RPID: a.RPID, Origin: a.Origin}, w.store, time.Now)
	if err != nil {
		return fail(err)
	}
	return w, nil
}

func devSigner(pem string, log *slog.Logger) (*receipt.LocalSigner, error) {
	if pem != "" {
		return receipt.ParseLocalSigner([]byte(pem))
	}
	s, err := receipt.GenerateLocalSigner()
	if err != nil {
		return nil, err
	}
	pub, _ := s.PublicKeyPEM()
	log.Warn("moose-approver: signing receipts with an ephemeral dev key; pin this public key in moose-email", "public_key", string(pub))
	return s, nil
}

func openBackend(ctx context.Context, cfg config.Config, needAWS func() (aws.Config, error)) (estore.Backend, error) {
	switch cfg.Store.Kind {
	case config.StoreSQLite:
		if dir := filepath.Dir(cfg.Store.SQLitePath); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("store: create %s: %w", dir, err)
			}
		}
		return emailsqlite.Open(ctx, cfg.Store.SQLitePath)
	case config.StoreDynamo:
		ac, err := needAWS()
		if err != nil {
			return nil, err
		}
		return emaildynamo.New(dynamodb.NewFromConfig(ac), cfg.Store.DynamoTable), nil
	}
	return nil, fmt.Errorf("store: unknown kind %q", cfg.Store.Kind)
}

// handler serves /healthz, the service API and the approval pages.
func (w *wiring) handler() http.Handler {
	origin, _ := url.Parse(w.cfg.Origin)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"ok":true}`))
	})
	mux.Handle(api.Prefix, (&api.Server{
		Store: w.store, Owner: w.cfg.Owner, BaseURL: w.cfg.Origin, Secret: w.secret, Log: w.log,
	}).Handler())
	mux.Handle("/approve/", (&pages.Server{
		Store: w.store, WebAuthn: w.wa, Signer: w.signer, RPID: w.cfg.RPID, Origin: w.cfg.Origin,
		Owner: w.cfg.Owner, Secret: w.secret, InsecureCookies: origin != nil && origin.Scheme != "https", Log: w.log,
	}).Handler())
	return mux
}

func defaultAWS(ctx context.Context) (aws.Config, error) { return awsconfig.LoadDefaultConfig(ctx) }
