// Command moose-lambda serves the moose http.Handler from AWS Lambda behind a
// private REST API Gateway. Requests arrive from the Tailscale relay EC2, and
// caller identity comes from the tailscale serve headers it forwards, trusted
// only alongside the relay shared secret.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"

	"github.com/change-fabric/moose/internal/bootstrap"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Cold start: load config, secrets, store, and connectors once.
	ctx := context.Background()
	h, err := newHandler(ctx, log)
	if err != nil {
		log.Error("moose-lambda init failed", "err", err)
		os.Exit(1)
	}
	lambda.Start(proxy(h))
}

func newHandler(ctx context.Context, log *slog.Logger) (http.Handler, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	sm := secretsmanager.NewFromConfig(awsCfg)
	rt, err := bootstrap.FromEnv(ctx, bootstrap.Options{
		Logger:       log,
		DynamoClient: dynamodb.NewFromConfig(awsCfg),
		Secret: func(ctx context.Context, arn string) ([]byte, error) {
			out, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
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
		},
	})
	if err != nil {
		return nil, err
	}
	return rt.Handler(version), nil
}

// proxy adapts h to REST API (v1) proxy events.
func proxy(h http.Handler) func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	return httpadapter.New(h).ProxyWithContext
}
