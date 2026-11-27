package dynamo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/dynamo"
	"github.com/change-fabric/moose/internal/accounts/store/storetest"
)

func client(t *testing.T) *awsdynamodb.Client {
	endpoint := os.Getenv("MOOSE_DYNAMO_ENDPOINT")
	if endpoint == "" {
		t.Skip("MOOSE_DYNAMO_ENDPOINT not set; run make test-store")
	}
	return awsdynamodb.New(awsdynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
}

func newTable(t *testing.T, c *awsdynamodb.Client) string {
	t.Helper()
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	table := "moose-email-test-" + hex.EncodeToString(b[:])
	if err := dynamo.CreateTable(ctx, c, table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.DeleteTable(ctx, &awsdynamodb.DeleteTableInput{TableName: aws.String(table)}) })
	return table
}

func TestBackendConformance(t *testing.T) {
	c := client(t)
	storetest.RunBackend(t, func(t *testing.T) store.Backend { return dynamo.New(c, newTable(t, c)) })
}

func TestStoreConformance(t *testing.T) {
	c := client(t)
	storetest.Run(t, func(t *testing.T) store.Store { return dynamo.NewStore(c, newTable(t, c)) })
}

// The ttl attribute DynamoDB's sweeper reads must be a Number of epoch seconds,
// and the table must have ttl enabled on that attribute.
func TestTTLAttributeIsEpochSeconds(t *testing.T) {
	ctx := context.Background()
	c := client(t)
	table := newTable(t, c)
	b := dynamo.New(c, table)
	want := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	err := b.Transact(ctx, []store.Op{{Kind: store.OpPut, Item: store.Item{PK: "p", SK: "s", Data: []byte("x"), TTL: want}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.GetItem(ctx, &awsdynamodb.GetItemInput{
		TableName: aws.String(table), ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: "p"}, "sk": &types.AttributeValueMemberS{Value: "s"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := out.Item["ttl"].(*types.AttributeValueMemberN)
	if !ok || n.Value != strconv.FormatInt(want.Unix(), 10) {
		t.Fatalf("raw ttl attribute = %#v, want N %d", out.Item["ttl"], want.Unix())
	}
	ttl, err := c.DescribeTimeToLive(ctx, &awsdynamodb.DescribeTimeToLiveInput{TableName: aws.String(table)})
	if err != nil {
		t.Fatal(err)
	}
	d := ttl.TimeToLiveDescription
	if d == nil || aws.ToString(d.AttributeName) != "ttl" ||
		(d.TimeToLiveStatus != types.TimeToLiveStatusEnabled && d.TimeToLiveStatus != types.TimeToLiveStatusEnabling) {
		t.Fatalf("table ttl spec = %+v, want enabled on attribute ttl", d)
	}
}
