// Package dynamo implements store.Store on a single DynamoDB table keyed by
// pk/sk, per the schema in the plan. No GSIs: token-by-owner index items are
// written in the same transaction as the token, and audit events are
// partitioned by UTC day with a 90-day ttl.
package dynamo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/change-fabric/moose/internal/store"
)

const (
	auditTTL   = 90 * 24 * time.Hour
	dayLayout  = "2006-01-02"
	skTimeFmt  = "2006-01-02T15:04:05.000000000Z07:00" // fixed width so sk sorts by time
	metaSK     = "META"
	userPrefix = "USER#"
	tokPrefix  = "TOKEN#"
)

// API is the subset of the DynamoDB client the store uses.
type API interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// Store is a DynamoDB-backed store.Store.
type Store struct {
	api   API
	table string
	now   func() time.Time
}

var _ store.Store = (*Store)(nil)

// New returns a store over an existing table.
func New(api API, table string) *Store {
	return &Store{api: api, table: table, now: time.Now}
}

// CreateTable creates the on-demand pk/sk table with ttl enabled and waits for
// it to become active. Production tables come from Terraform; this is for
// tests and local development.
func CreateTable(ctx context.Context, c *dynamodb.Client, table string) error {
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(table),
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS},
		},
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
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

type item = map[string]types.AttributeValue

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
func b(v bool) types.AttributeValue { return &types.AttributeValueMemberBOOL{Value: v} }
func key(pk, sk string) item        { return item{"pk": s(pk), "sk": s(sk)} }
func ms(t time.Time) int64          { return t.UTC().UnixMilli() }
func fromMS(v int64) time.Time      { return time.UnixMilli(v).UTC() }
func getS(it item, k string) string { v, _ := it[k].(*types.AttributeValueMemberS); return strOr(v) }
func strOr(v *types.AttributeValueMemberS) string {
	if v == nil {
		return ""
	}
	return v.Value
}

func getN(it item, k string) (int64, bool) {
	v, ok := it[k].(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(v.Value, 10, 64)
	return i, err == nil
}

func getB(it item, k string) bool {
	v, ok := it[k].(*types.AttributeValueMemberBOOL)
	return ok && v.Value
}

func getTime(it item, k string) *time.Time {
	v, ok := getN(it, k)
	if !ok {
		return nil
	}
	t := fromMS(v)
	return &t
}

func isCondFailed(err error) bool {
	var ccf *types.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return true
	}
	var tce *types.TransactionCanceledException
	if errors.As(err, &tce) {
		for _, r := range tce.CancellationReasons {
			if aws.ToString(r.Code) == "ConditionalCheckFailed" {
				return true
			}
		}
	}
	return false
}

func (st *Store) get(ctx context.Context, pk string) (item, error) {
	out, err := st.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(st.table), Key: key(pk, metaSK), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, store.ErrNotFound
	}
	return out.Item, nil
}

func (st *Store) PutUser(ctx context.Context, u store.User) error {
	it := key(userPrefix+u.Login, metaSK)
	it["login"], it["role"], it["created"], it["disabled"] = s(u.Login), s(u.Role), n(ms(u.Created)), b(u.Disabled)
	_, err := st.api.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(st.table), Item: it})
	return err
}

func toUser(it item) store.User {
	created, _ := getN(it, "created")
	return store.User{
		Login: strings.TrimPrefix(getS(it, "pk"), userPrefix), Role: getS(it, "role"),
		Created: fromMS(created), Disabled: getB(it, "disabled"),
	}
}

func (st *Store) GetUser(ctx context.Context, login string) (store.User, error) {
	it, err := st.get(ctx, userPrefix+login)
	if err != nil {
		return store.User{}, err
	}
	return toUser(it), nil
}

func (st *Store) scanPrefix(ctx context.Context, prefix string) ([]item, error) {
	var out []item
	var start item
	for {
		res, err := st.api.Scan(ctx, &dynamodb.ScanInput{
			TableName:                 aws.String(st.table),
			FilterExpression:          aws.String("begins_with(pk, :p) AND sk = :m"),
			ExpressionAttributeValues: item{":p": s(prefix), ":m": s(metaSK)},
			ExclusiveStartKey:         start,
			ConsistentRead:            aws.Bool(true),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if len(res.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = res.LastEvaluatedKey
	}
}

func (st *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	items, err := st.scanPrefix(ctx, userPrefix)
	if err != nil {
		return nil, err
	}
	users := make([]store.User, 0, len(items))
	for _, it := range items {
		users = append(users, toUser(it))
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Login < users[j].Login })
	return users, nil
}

func (st *Store) CreateToken(ctx context.Context, t store.Token) error {
	tok := key(tokPrefix+t.ID, metaSK)
	tok["owner"], tok["name"], tok["hash"] = s(t.Owner), s(t.Name), &types.AttributeValueMemberB{Value: t.Hash}
	tok["created"], tok["revoked"] = n(ms(t.Created)), b(t.Revoked)
	if t.LastUsed != nil {
		tok["last_used"] = n(ms(*t.LastUsed))
	}
	if t.Expires != nil {
		tok["expires"] = n(ms(*t.Expires))
	}
	idx := key("OWNER#"+t.Owner, tokPrefix+t.ID)
	idx["name"], idx["created"], idx["revoked"] = s(t.Name), n(ms(t.Created)), b(t.Revoked)
	_, err := st.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: aws.String(st.table), Item: tok, ConditionExpression: aws.String("attribute_not_exists(pk)")}},
		{Put: &types.Put{TableName: aws.String(st.table), Item: idx}},
	}})
	if isCondFailed(err) {
		return store.ErrConflict
	}
	return err
}

func toToken(it item) store.Token {
	created, _ := getN(it, "created")
	t := store.Token{
		ID: strings.TrimPrefix(getS(it, "pk"), tokPrefix), Owner: getS(it, "owner"), Name: getS(it, "name"),
		Created: fromMS(created), LastUsed: getTime(it, "last_used"), Expires: getTime(it, "expires"),
		Revoked: getB(it, "revoked"),
	}
	if h, ok := it["hash"].(*types.AttributeValueMemberB); ok {
		t.Hash = h.Value
	}
	return t
}

func (st *Store) GetToken(ctx context.Context, id string) (store.Token, error) {
	it, err := st.get(ctx, tokPrefix+id)
	if err != nil {
		return store.Token{}, err
	}
	return toToken(it), nil
}

func (st *Store) ListTokens(ctx context.Context, owner string) ([]store.Token, error) {
	var toks []store.Token
	if owner == "" {
		items, err := st.scanPrefix(ctx, tokPrefix)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			toks = append(toks, toToken(it))
		}
	} else {
		var start item
		for {
			res, err := st.api.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String(st.table),
				KeyConditionExpression:    aws.String("pk = :p AND begins_with(sk, :t)"),
				ExpressionAttributeValues: item{":p": s("OWNER#" + owner), ":t": s(tokPrefix)},
				ExclusiveStartKey:         start,
				ConsistentRead:            aws.Bool(true),
			})
			if err != nil {
				return nil, err
			}
			for _, idx := range res.Items {
				t, err := st.GetToken(ctx, strings.TrimPrefix(getS(idx, "sk"), tokPrefix))
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, err
				}
				toks = append(toks, t)
			}
			if len(res.LastEvaluatedKey) == 0 {
				break
			}
			start = res.LastEvaluatedKey
		}
	}
	sort.Slice(toks, func(i, j int) bool {
		if !toks[i].Created.Equal(toks[j].Created) {
			return toks[i].Created.Before(toks[j].Created)
		}
		return toks[i].ID < toks[j].ID
	})
	return toks, nil
}

func (st *Store) RevokeToken(ctx context.Context, id string) error {
	t, err := st.GetToken(ctx, id)
	if err != nil {
		return err
	}
	upd := func(k item, cond string) types.TransactWriteItem {
		u := &types.Update{
			TableName: aws.String(st.table), Key: k,
			UpdateExpression:          aws.String("SET revoked = :r"),
			ExpressionAttributeValues: item{":r": b(true)},
		}
		if cond != "" {
			u.ConditionExpression = aws.String(cond)
		}
		return types.TransactWriteItem{Update: u}
	}
	_, err = st.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		upd(key(tokPrefix+id, metaSK), "attribute_exists(pk)"),
		upd(key("OWNER#"+t.Owner, tokPrefix+id), ""),
	}})
	if isCondFailed(err) {
		return store.ErrNotFound
	}
	return err
}

func (st *Store) TouchToken(ctx context.Context, id string, at time.Time) error {
	_, err := st.api.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(st.table), Key: key(tokPrefix+id, metaSK),
		UpdateExpression:          aws.String("SET last_used = :t"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeValues: item{":t": n(ms(at))},
	})
	if isCondFailed(err) {
		return store.ErrNotFound
	}
	return err
}

func (st *Store) GetConnectorConfig(ctx context.Context, id string) (map[string]string, error) {
	it, err := st.get(ctx, "CONN#"+id)
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{}
	if err := json.Unmarshal([]byte(getS(it, "json")), &cfg); err != nil {
		return nil, fmt.Errorf("dynamo: decode connector config %q: %w", id, err)
	}
	return cfg, nil
}

func (st *Store) PutConnectorConfig(ctx context.Context, id string, cfg map[string]string) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	it := key("CONN#"+id, metaSK)
	it["json"], it["updated"] = s(string(raw)), n(ms(st.now()))
	_, err = st.api.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(st.table), Item: it})
	return err
}

func auditPK(t time.Time) string { return "AUDIT#" + t.UTC().Format(dayLayout) }

func (st *Store) AppendAudit(ctx context.Context, e store.AuditEvent) error {
	at := e.At.UTC()
	it := key(auditPK(at), at.Format(skTimeFmt)+"#"+e.RequestID)
	it["at"], it["request_id"], it["actor"], it["token_id"] = n(ms(at)), s(e.RequestID), s(e.Actor), s(e.TokenID)
	it["surface"], it["action"], it["source"], it["doc_id"] = s(e.Surface), s(e.Action), s(e.Source), s(e.DocID)
	it["status"], it["latency_ms"], it["detail"] = n(int64(e.Status)), n(e.LatencyMS), s(e.Detail)
	it["ttl"] = n(at.Add(auditTTL).Unix())
	_, err := st.api.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(st.table), Item: it})
	return err
}

// ListAudit walks day partitions backward from today (UTC) to the day of since.
func (st *Store) ListAudit(ctx context.Context, since time.Time, limit int) ([]store.AuditEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	since = since.UTC()
	sinceDay := since.Truncate(24 * time.Hour)
	var out []store.AuditEvent
	for day := st.now().UTC().Truncate(24 * time.Hour); !day.Before(sinceDay); day = day.Add(-24 * time.Hour) {
		var start item
		for {
			res, err := st.api.Query(ctx, &dynamodb.QueryInput{
				TableName:                 aws.String(st.table),
				KeyConditionExpression:    aws.String("pk = :p AND sk >= :s"),
				ExpressionAttributeValues: item{":p": s(auditPK(day)), ":s": s(since.Format(skTimeFmt))},
				ScanIndexForward:          aws.Bool(false),
				ExclusiveStartKey:         start,
				Limit:                     aws.Int32(int32(limit - len(out))),
				ConsistentRead:            aws.Bool(true),
			})
			if err != nil {
				return nil, err
			}
			for _, it := range res.Items {
				at, _ := getN(it, "at")
				status, _ := getN(it, "status")
				lat, _ := getN(it, "latency_ms")
				out = append(out, store.AuditEvent{
					At: fromMS(at), RequestID: getS(it, "request_id"), Actor: getS(it, "actor"),
					TokenID: getS(it, "token_id"), Surface: getS(it, "surface"), Action: getS(it, "action"),
					Source: getS(it, "source"), DocID: getS(it, "doc_id"), Status: int(status),
					LatencyMS: lat, Detail: getS(it, "detail"),
				})
			}
			if len(out) >= limit {
				return out[:limit], nil
			}
			if len(res.LastEvaluatedKey) == 0 {
				break
			}
			start = res.LastEvaluatedKey
		}
	}
	return out, nil
}

// Close is a no-op; the AWS client holds no resources that need releasing.
func (st *Store) Close() error { return nil }
