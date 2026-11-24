// Package wiring assembles the Drive server from a bootstrap.Runtime: the
// connectors, the core service, and the HTTP handler. The Drive cmds
// (moosed and moose-lambda) share it.
package wiring

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/change-fabric/moose/internal/bootstrap"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/drive/api"
	"github.com/change-fabric/moose/internal/drive/connectors/gdrive"
	"github.com/change-fabric/moose/internal/drive/core"
	"github.com/change-fabric/moose/internal/server"
)

// Handler validates the drive: section, builds the Drive service, and returns
// the composed http.Handler.
func Handler(ctx context.Context, rt *bootstrap.Runtime, version string) (http.Handler, error) {
	if err := rt.Config.ValidateDrive(); err != nil {
		return nil, err
	}
	d := rt.Config.Drive
	conns, maxBytes, err := connectors(ctx, rt, d)
	if err != nil {
		return nil, err
	}
	svc, err := core.NewService(conns, core.Options{RedactPatterns: d.Redaction.Patterns, MaxBytes: maxBytes})
	if err != nil {
		return nil, err
	}
	return server.New(server.Deps{Store: rt.Store, Auth: rt.Auth, Logger: rt.Logger,
		Mounts: api.Mounts(api.Deps{Service: svc, Store: rt.Store, Version: version})}), nil
}

func connectors(ctx context.Context, rt *bootstrap.Runtime, d *config.Drive) ([]core.Connector, int64, error) {
	gc, ok := d.Connectors[gdrive.ID]
	if !ok || !gc.Enabled {
		return nil, 0, nil
	}
	var key []byte
	var keyErr error
	switch {
	case d.GDriveKeyFile != "":
		key, keyErr = os.ReadFile(d.GDriveKeyFile)
	case d.GDriveKeySecretARN != "" && rt.Secret != nil:
		key, keyErr = rt.Secret(ctx, d.GDriveKeySecretARN)
	case d.GDriveKeySecretARN != "":
		keyErr = errors.New("MOOSE_GDRIVE_KEY_SECRET_ARN is read by the Lambda entrypoint only")
	default:
		keyErr = errors.New("no service account key configured (set MOOSE_GDRIVE_KEY_FILE)")
	}
	if keyErr == nil {
		c, err := gdrive.New(ctx, key)
		if err == nil {
			return []core.Connector{c}, gc.MaxDocumentBytes, nil
		}
		keyErr = err
	}
	// Plan failure mode: start anyway, report gdrive unhealthy, answer 503.
	rt.Logger.Warn("gdrive connector unavailable", "err", keyErr)
	return []core.Connector{Unavailable{IDValue: gdrive.ID, NameValue: "Google Drive", Err: keyErr}}, gc.MaxDocumentBytes, nil
}

// Unavailable stands in for a connector whose credentials could not be
// loaded: health reports the reason and every data call is a 503.
type Unavailable struct {
	IDValue, NameValue string
	Err                error
}

func (u Unavailable) ID() string   { return u.IDValue }
func (u Unavailable) Name() string { return u.NameValue }
func (u Unavailable) Kind() string { return u.IDValue }
func (u Unavailable) Detail(context.Context) string {
	return "unavailable: " + u.Err.Error()
}
func (u Unavailable) Health(context.Context) error { return core.ErrUpstreamUnavailable }
func (u Unavailable) List(context.Context, string, int) (core.SearchResult, error) {
	return core.SearchResult{}, core.ErrUpstreamUnavailable
}
func (u Unavailable) Search(context.Context, core.SearchQuery) (core.SearchResult, error) {
	return core.SearchResult{}, core.ErrUpstreamUnavailable
}
func (u Unavailable) Get(context.Context, string, int64) (core.Document, error) {
	return core.Document{}, core.ErrUpstreamUnavailable
}
