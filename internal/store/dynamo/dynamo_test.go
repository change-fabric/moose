package dynamo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/change-fabric/moose/internal/store"
	"github.com/change-fabric/moose/internal/store/dynamo"
	"github.com/change-fabric/moose/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	endpoint := os.Getenv("MOOSE_DYNAMO_ENDPOINT")
	if endpoint == "" {
		t.Skip("MOOSE_DYNAMO_ENDPOINT not set; run make test-store")
	}
	client := awsdynamodb.New(awsdynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	storetest.Run(t, func(t *testing.T) store.Store {
		ctx := context.Background()
		var b [6]byte
		_, _ = rand.Read(b[:])
		table := "moose-test-" + hex.EncodeToString(b[:])
		if err := dynamo.CreateTable(ctx, client, table); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = client.DeleteTable(ctx, &awsdynamodb.DeleteTableInput{TableName: aws.String(table)})
		})
		return dynamo.New(client, table)
	})
}
