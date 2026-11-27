package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/change-fabric/moose/internal/config"
)

// runSweepSends implements `moose-email sweep-sends`: one pass of the native
// draft-send sweep (reconcile submitted and unknown Microsoft sends, delete the
// staging drafts of failed and sent ledgers). It uses the same wiring as the
// server and prints the report as JSON. cmd/moose-attach-sweeper runs the same
// step once the sweeper Lambda is given the table, KMS and Microsoft secret
// access it needs (Terraform, Phase 8).
func runSweepSends(ctx context.Context, out io.Writer, loadAWS func(context.Context) (aws.Config, error)) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateEmail(); err != nil {
		return err
	}
	w, err := buildConnect(ctx, cfg, slog.New(slog.NewJSONHandler(os.Stderr, nil)), loadAWS)
	if err != nil {
		return err
	}
	if w == nil || w.send == nil {
		return errors.New("the send engine is not configured (email.base_url and the OAuth server are required)")
	}
	defer w.store.Close()
	rep, err := w.send.SweepSends(ctx)
	b, _ := json.Marshal(rep)
	fmt.Fprintln(out, string(b))
	return err
}
