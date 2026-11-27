// Command moose-attach-sweeper deletes email attachment objects older than one
// hour from the private attachments bucket. It runs as an AWS Lambda on an
// EventBridge schedule (every 15 minutes); outside Lambda it sweeps once and
// exits, which suits a manual run.
//
// Each run logs one CloudWatch Embedded Metric Format line, so the age of the
// oldest object left behind becomes the OldestAttachmentAgeSeconds metric that
// the two-hour alarm watches. A failed run returns the error, so Lambda's
// async retry (twice) and then the dead-letter queue take over.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/change-fabric/moose/internal/email/attach"
	"github.com/change-fabric/moose/internal/email/sendflow"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const name = "moose-attach-sweeper"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(name, version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	bucket := os.Getenv("MOOSE_EMAIL_ATTACH_BUCKET")
	if bucket == "" {
		log.Error(name + " exiting: MOOSE_EMAIL_ATTACH_BUCKET is not set")
		os.Exit(1)
	}
	handler := func(ctx context.Context) error {
		st, err := openStore(ctx, bucket)
		if err != nil {
			return err
		}
		return errors.Join(run(ctx, log, st, time.Now), runSends(ctx, log, openSendSweeper()))
	}
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(handler)
		return
	}
	if err := handler(context.Background()); err != nil {
		log.Error(name+" exiting", "err", err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context, bucket string) (attach.Store, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return attach.NewS3(s3.NewFromConfig(cfg), bucket, ""), nil
}

// run sweeps st once and prints the metric line to stdout, where Lambda sends it to CloudWatch Logs.
func run(ctx context.Context, log *slog.Logger, st attach.Store, now func() time.Time) error {
	rep, err := attach.Sweep(ctx, st, now(), attach.MaxAge)
	// The metric goes out even after a failed delete: the objects that stayed
	// are what the alarm must see. It does not go out when listing failed,
	// because then nothing is known.
	if rep.Surveyed {
		fmt.Println(attach.MetricLine(rep, now()))
	}
	log.Info(name+" swept", "listed", rep.Listed, "deleted", rep.Deleted, "remaining", rep.Remaining,
		"oldest_remaining_seconds", int64(rep.OldestRemaining/time.Second))
	if err != nil {
		return errors.Join(errors.New("sweep incomplete"), err)
	}
	return nil
}

// sendSweeper is the native draft-send step: reconcile submitted and unknown
// Microsoft sends and delete the staging drafts of failed and sent ledgers
// (sendflow.Service implements it).
type sendSweeper interface {
	SweepSends(ctx context.Context) (sendflow.SweepReport, error)
}

// openSendSweeper returns the send sweeper, or nil when this deployment has not
// been given one. Building it needs the email table, the KMS key and the
// Microsoft client secret, which this Lambda does not have until the Terraform
// of Phase 8 grants them; until then `moose-email sweep-sends` runs the same
// step. email.microsoft.enabled is false, so no staging draft exists to sweep.
func openSendSweeper() sendSweeper { return nil }

// runSends runs the send step once. A nil sweeper is not an error.
func runSends(ctx context.Context, log *slog.Logger, sw sendSweeper) error {
	if sw == nil {
		return nil
	}
	rep, err := sw.SweepSends(ctx)
	log.Info(name+" swept sends", "examined", rep.Examined, "reconciled", rep.Reconciled, "cleaned", rep.Cleaned,
		"escalated", rep.Escalated, "skipped", rep.Skipped, "errors", rep.Errors)
	if err != nil {
		return errors.Join(errors.New("send sweep incomplete"), err)
	}
	return nil
}
