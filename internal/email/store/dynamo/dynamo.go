// Package dynamo is the DynamoDB storage backend for the email store. One
// on-demand table holds every record, keyed by pk/sk, with a sparse global
// secondary index on g1 (the lowercased address) and the table's ttl attribute
// holding epoch seconds. Production tables come from Terraform and must match
// CreateTable.
package dynamo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/change-fabric/moose/internal/email/store"
)

// IndexName is the name of the g1 global secondary index.
const IndexName = "g1-index"

// API is the subset of the DynamoDB client the backend uses.
type API interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Backend is a DynamoDB store.Backend.
type Backend struct {
	api   API
	table string
}

var _ store.Backend = (*Backend)(nil)

// New returns a backend over an existing table.
func New(api API, table string) *Backend { return &Backend{api: api, table: table} }

// NewStore returns the email Store over an existing table.
func NewStore(api API, table string) store.Store { return store.New(New(api, table)) }

// CreateTable creates the table with its g1 index and ttl enabled, and waits
// for it to become active. For tests and local development.
func CreateTable(ctx context.Context, c *dynamodb.Client, table string) error {
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(table),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("g1"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{{
			IndexName:  aws.String(IndexName),
			KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("g1"), KeyType: types.KeyTypeHash}},
			Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
		}},
	})
	if err != nil {
		return err
	}
	if err := dynamodb.NewTableExistsWaiter(c).Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)}, time.Minute); err != nil {
		return err
	}
	_, err = c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName:               aws.String(table),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{AttributeName: aws.String("ttl"), Enabled: aws.Bool(true)},
	})
	return err
}

func (b *Backend) Close() error { return nil }

type attrs = map[string]types.AttributeValue

func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}

func toAttrs(it store.Item) attrs {
	a := attrs{
		"pk":  &types.AttributeValueMemberS{Value: it.PK},
		"sk":  &types.AttributeValueMemberS{Value: it.SK},
		"ver": n(it.Ver),
	}
	if it.G1 != "" {
		a["g1"] = &types.AttributeValueMemberS{Value: it.G1}
	}
	if it.Data != nil {
		a["data"] = &types.AttributeValueMemberB{Value: it.Data}
	}
	if it.N != 0 {
		a["n"] = n(it.N)
	}
	if !it.TTL.IsZero() {
		a["ttl"] = n(it.TTL.Unix())
	}
	return a
}

func num(a attrs, k string) (int64, bool) {
	v, ok := a[k].(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(v.Value, 10, 64)
	return i, err == nil
}

func str(a attrs, k string) string {
	v, _ := a[k].(*types.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}

func fromAttrs(a attrs) store.Item {
	it := store.Item{PK: str(a, "pk"), SK: str(a, "sk"), G1: str(a, "g1")}
	if d, ok := a["data"].(*types.AttributeValueMemberB); ok {
		it.Data = d.Value
	}
	it.Ver, _ = num(a, "ver")
	it.N, _ = num(a, "n")
	if t, ok := num(a, "ttl"); ok {
		it.TTL = time.Unix(t, 0).UTC()
	}
	return it
}

func (b *Backend) Get(ctx context.Context, pk, sk string) (store.Item, bool, error) {
	out, err := b.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(b.table), ConsistentRead: aws.Bool(true),
		Key: attrs{"pk": &types.AttributeValueMemberS{Value: pk}, "sk": &types.AttributeValueMemberS{Value: sk}},
	})
	if err != nil {
		return store.Item{}, false, err
	}
	if out.Item == nil {
		return store.Item{}, false, nil
	}
	return fromAttrs(out.Item), true, nil
}

func (b *Backend) query(ctx context.Context, in *dynamodb.QueryInput) ([]store.Item, error) {
	in.TableName = aws.String(b.table)
	var out []store.Item
	for {
		res, err := b.api.Query(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, a := range res.Items {
			out = append(out, fromAttrs(a))
		}
		if len(res.LastEvaluatedKey) == 0 {
			return out, nil
		}
		in.ExclusiveStartKey = res.LastEvaluatedKey
	}
}

func (b *Backend) Query(ctx context.Context, pk, skPrefix string) ([]store.Item, error) {
	in := &dynamodb.QueryInput{
		ConsistentRead:            aws.Bool(true),
		KeyConditionExpression:    aws.String("pk = :p"),
		ExpressionAttributeValues: attrs{":p": &types.AttributeValueMemberS{Value: pk}},
	}
	if skPrefix != "" {
		in.KeyConditionExpression = aws.String("pk = :p AND begins_with(sk, :s)")
		in.ExpressionAttributeValues[":s"] = &types.AttributeValueMemberS{Value: skPrefix}
	}
	return b.query(ctx, in)
}

func (b *Backend) QueryIndex(ctx context.Context, g1 string) ([]store.Item, error) {
	return b.query(ctx, &dynamodb.QueryInput{
		IndexName:                 aws.String(IndexName),
		KeyConditionExpression:    aws.String("g1 = :g"),
		ExpressionAttributeValues: attrs{":g": &types.AttributeValueMemberS{Value: g1}},
	})
}

// transactRetries bounds retries of TransactionConflict, which DynamoDB
// returns when another transaction touches the same item concurrently.
const transactRetries = 8

func (b *Backend) Transact(ctx context.Context, ops []store.Op) error {
	items := make([]types.TransactWriteItem, len(ops))
	for i, op := range ops {
		w, err := b.writeItem(op)
		if err != nil {
			return err
		}
		items[i] = w
	}
	for attempt := 0; ; attempt++ {
		_, err := b.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: items})
		if err == nil {
			return nil
		}
		var tce *types.TransactionCanceledException
		if !errors.As(err, &tce) {
			return err
		}
		conflict := false
		for i, r := range tce.CancellationReasons {
			switch aws.ToString(r.Code) {
			case "ConditionalCheckFailed":
				return &store.CondError{Index: i}
			case "TransactionConflict":
				conflict = true
			}
		}
		if !conflict || attempt >= transactRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 15 * time.Millisecond):
		}
	}
}

func (b *Backend) writeItem(op store.Op) (types.TransactWriteItem, error) {
	table := aws.String(b.table)
	names := map[string]string{}
	vals := attrs{}
	var cond *string
	switch op.Cond.Kind {
	case store.CondAbsent:
		names["#pk"] = "pk"
		cond = aws.String("attribute_not_exists(#pk)")
	case store.CondVer:
		names["#v"] = "ver"
		vals[":cv"] = n(op.Cond.Ver)
		cond = aws.String("#v = :cv")
	}
	key := attrs{"pk": &types.AttributeValueMemberS{Value: op.Item.PK}, "sk": &types.AttributeValueMemberS{Value: op.Item.SK}}
	// DynamoDB rejects expression maps that are empty or unused.
	opt := func(m map[string]string, v attrs) (map[string]string, attrs) {
		if len(m) == 0 {
			m = nil
		}
		if len(v) == 0 {
			v = nil
		}
		return m, v
	}
	switch op.Kind {
	case store.OpPut:
		nm, vv := opt(names, vals)
		return types.TransactWriteItem{Put: &types.Put{
			TableName: table, Item: toAttrs(op.Item), ConditionExpression: cond,
			ExpressionAttributeNames: nm, ExpressionAttributeValues: vv,
		}}, nil
	case store.OpDelete:
		nm, vv := opt(names, vals)
		return types.TransactWriteItem{Delete: &types.Delete{
			TableName: table, Key: key, ConditionExpression: cond,
			ExpressionAttributeNames: nm, ExpressionAttributeValues: vv,
		}}, nil
	case store.OpIncr:
		names = map[string]string{"#n": "n"}
		vals = attrs{":d": n(op.Delta)}
		update := "ADD #n :d"
		if !op.Item.TTL.IsZero() {
			names["#t"] = "ttl"
			vals[":t"] = n(op.Item.TTL.Unix())
			update = "SET #t = :t ADD #n :d"
		}
		cond = nil
		switch {
		case op.Delta > 0 && op.Max > 0:
			vals[":lim"] = n(op.Max - op.Delta)
			cond = aws.String("attribute_not_exists(#n) OR #n <= :lim")
		case op.Delta < 0:
			vals[":need"] = n(-op.Delta)
			cond = aws.String("#n >= :need")
		}
		return types.TransactWriteItem{Update: &types.Update{
			TableName: table, Key: key, UpdateExpression: aws.String(update), ConditionExpression: cond,
			ExpressionAttributeNames: names, ExpressionAttributeValues: vals,
		}}, nil
	}
	return types.TransactWriteItem{}, fmt.Errorf("emailstore dynamo: unknown op kind %d", op.Kind)
}
