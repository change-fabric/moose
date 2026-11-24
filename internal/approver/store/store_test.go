package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/email/envelope"
	estore "github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/email/store/dynamo"
	"github.com/change-fabric/moose/internal/email/store/sqlite"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// clock is a settable clock shared by a store and its test.
type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time      { return time.Unix(c.t.Load(), 0).UTC() }
func (c *clock) set(t time.Time)     { c.t.Store(t.Unix()) }
func (c *clock) add(d time.Duration) { c.t.Add(int64(d / time.Second)) }

type env struct {
	st  *store.Store
	b   estore.Backend
	clk *clock
}

func sealer(t *testing.T) envelope.Sealer {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s, err := envelope.NewLocal(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// backends runs fn against SQLite and, when MOOSE_DYNAMO_ENDPOINT is set
// (make test-store), against DynamoDB local too.
func backends(t *testing.T, fn func(t *testing.T, e env)) {
	t.Run("sqlite", func(t *testing.T) {
		b, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "approver.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		clk := &clock{}
		clk.set(t0)
		fn(t, env{store.NewWithClock(b, sealer(t), clk.now), b, clk})
	})
	t.Run("dynamodb", func(t *testing.T) {
		endpoint := os.Getenv("MOOSE_DYNAMO_ENDPOINT")
		if endpoint == "" {
			t.Skip("MOOSE_DYNAMO_ENDPOINT not set; run make test-store")
		}
		c := awsdynamodb.New(awsdynamodb.Options{
			Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
			Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		})
		var rnd [6]byte
		_, _ = rand.Read(rnd[:])
		table := "moose-approver-test-" + hex.EncodeToString(rnd[:])
		if err := dynamo.CreateTable(context.Background(), c, table); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = c.DeleteTable(context.Background(), &awsdynamodb.DeleteTableInput{TableName: aws.String(table)})
		})
		b := dynamo.New(c, table)
		clk := &clock{}
		clk.set(t0)
		fn(t, env{store.NewWithClock(b, sealer(t), clk.now), b, clk})
	})
}

func signer(t *testing.T) *receipt.LocalSigner {
	s, err := receipt.GenerateLocalSigner()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mkReq(id, key, kind string, now time.Time) store.Request {
	payload := json.RawMessage(`{"account_id":"acc_1","from_address":"a@x.example","owner":"pat"}`)
	return store.Request{
		ID: id, Owner: "pat", Kind: kind, IdempotencyKey: key, AccountID: "acc_1", FromAddress: "a@x.example",
		PayloadSHA256: "sha-" + id, Payload: payload, Created: now, Expires: now.Add(store.RequestLifetime),
	}
}

func signFn(t *testing.T, s receipt.Signer, now time.Time) func(store.Request) (receipt.Receipt, error) {
	return func(r store.Request) (receipt.Receipt, error) {
		return receipt.Sign(context.Background(), s, receipt.Body{
			RequestID: r.ID, Kind: r.Kind, PayloadSHA256: r.PayloadSHA256,
			ApprovedAt: now, ExpiresAt: now.Add(store.ReceiptLifetime), RPID: "moose.example.org",
		})
	}
}

func create(t *testing.T, e env, id, key, kind string) store.Request {
	t.Helper()
	r, created, err := e.st.CreateRequest(context.Background(), mkReq(id, key, kind, e.clk.now()), []byte(`{"secret":"PREVIEW-BODY-TEXT"}`))
	if err != nil || !created {
		t.Fatalf("CreateRequest %s: created=%v err=%v", id, created, err)
	}
	return r
}

func hasPreview(t *testing.T, e env, id string) bool {
	t.Helper()
	ok, err := e.st.HasPreview(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestCreateIsIdempotentByKey(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		first := create(t, e, "apr_1", "key-1", "email-send")
		dup := mkReq("apr_2", "key-1", "email-send", e.clk.now())
		dup.PayloadSHA256 = first.PayloadSHA256
		again, created, err := e.st.CreateRequest(ctx, dup, []byte("x"))
		if err != nil || created {
			t.Fatalf("repeat: created=%v err=%v, want the existing request", created, err)
		}
		if again.ID != first.ID {
			t.Fatalf("repeat returned %s, want %s", again.ID, first.ID)
		}
		if _, err := e.st.GetRequest(ctx, "apr_2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a duplicate request row was written: %v", err)
		}
		other := mkReq("apr_3", "key-1", "email-send", e.clk.now())
		other.PayloadSHA256 = "different"
		if _, _, err := e.st.CreateRequest(ctx, other, []byte("x")); !errors.Is(err, store.ErrIdempotencyReuse) {
			t.Fatalf("same key, other payload: err = %v, want ErrIdempotencyReuse", err)
		}
	})
}

func TestPreviewIsSealedAndHasShortTTL(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		r := create(t, e, "apr_1", "k", "email-send")
		it, ok, err := e.b.Get(ctx, "REQ#apr_1", "PREVIEW")
		if err != nil || !ok {
			t.Fatalf("preview row: ok=%v err=%v", ok, err)
		}
		if bytes.Contains(it.Data, []byte("PREVIEW-BODY-TEXT")) {
			t.Fatal("the preview is stored in the clear")
		}
		if max := r.Created.Add(24 * time.Hour); it.TTL.After(max) || it.TTL.IsZero() {
			t.Fatalf("preview ttl = %v, want at most %v", it.TTL, max)
		}
		meta, _, _ := e.b.Get(ctx, "REQ#apr_1", "META")
		if bytes.Contains(meta.Data, []byte("PREVIEW-BODY-TEXT")) {
			t.Fatal("the request row carries preview content")
		}
		got, err := e.st.Preview(ctx, r)
		if err != nil || string(got) != `{"secret":"PREVIEW-BODY-TEXT"}` {
			t.Fatalf("Preview = %q, %v", got, err)
		}
	})
}

func TestPreviewDeletedOnDenyExpiryAndConsume(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		sg := signer(t)

		create(t, e, "apr_deny", "k1", "email-send")
		if _, err := e.st.Deny(ctx, "apr_deny", e.clk.now()); err != nil {
			t.Fatal(err)
		}
		if hasPreview(t, e, "apr_deny") {
			t.Error("preview survived deny")
		}

		create(t, e, "apr_exp", "k2", "email-send")
		create(t, e, "apr_exp_lazy", "k2b", "email-send")
		create(t, e, "apr_use", "k3", "email-send")
		if _, err := e.st.Approve(ctx, "apr_use", e.clk.now(), false, signFn(t, sg, e.clk.now())); err != nil {
			t.Fatal(err)
		}
		if !hasPreview(t, e, "apr_use") {
			t.Error("preview must survive approval until the request is consumed")
		}
		if _, err := e.st.Consume(ctx, "apr_use", "claimant", e.clk.now()); err != nil {
			t.Fatal(err)
		}
		if hasPreview(t, e, "apr_use") {
			t.Error("preview survived consume")
		}

		e.clk.add(store.RequestLifetime + time.Second)
		// The sweep finds the unread one; a read finds the other.
		res, err := e.st.Sweep(ctx, "pat", e.clk.now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Expired != 2 {
			t.Fatalf("sweep expired %d, want 2", res.Expired)
		}
		for _, id := range []string{"apr_exp", "apr_exp_lazy"} {
			if hasPreview(t, e, id) {
				t.Errorf("preview of %s survived expiry", id)
			}
			if r, _ := e.st.GetRequest(ctx, id); r.Status != store.StatusExpired {
				t.Errorf("%s status = %s, want expired", id, r.Status)
			}
		}
	})
}

func TestFreshExpiresLazily(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		create(t, e, "apr_1", "k", "email-send")
		r, err := e.st.Fresh(ctx, "apr_1", e.clk.now().Add(store.RequestLifetime-time.Second))
		if err != nil || r.Status != store.StatusPending {
			t.Fatalf("just before expiry: %s, %v", r.Status, err)
		}
		r, err = e.st.Fresh(ctx, "apr_1", e.clk.now().Add(store.RequestLifetime))
		if err != nil || r.Status != store.StatusExpired {
			t.Fatalf("at expiry: %s, %v", r.Status, err)
		}
		if hasPreview(t, e, "apr_1") {
			t.Fatal("preview survived expiry")
		}
		if _, err := e.st.Approve(ctx, "apr_1", e.clk.now(), false, signFn(t, signer(t), e.clk.now())); err == nil {
			t.Fatal("an expired request was approved")
		}
	})
}

func TestStatusTransitions(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		sg := signer(t)
		now := e.clk.now()

		create(t, e, "apr_1", "k", "email-send")
		if _, err := e.st.Consume(ctx, "apr_1", "c", now); !errors.Is(err, store.ErrInvalidTransition) {
			t.Fatalf("consume while pending: %v", err)
		}
		if _, err := e.st.Approve(ctx, "apr_1", now, false, signFn(t, sg, now)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Approve(ctx, "apr_1", now, false, signFn(t, sg, now)); !errors.Is(err, store.ErrInvalidTransition) {
			t.Fatalf("approve twice: %v", err)
		}
		if _, err := e.st.Deny(ctx, "apr_1", now); !errors.Is(err, store.ErrInvalidTransition) {
			t.Fatalf("deny after approve: %v", err)
		}
		r, _ := e.st.GetRequest(ctx, "apr_1")
		if r.Receipt == nil || r.Receipt.Body.PayloadSHA256 != r.PayloadSHA256 {
			t.Fatalf("approved request has no matching receipt: %+v", r.Receipt)
		}
		if _, err := e.st.Consume(ctx, "apr_1", "c", now); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Consume(ctx, "apr_1", "other", now); !errors.Is(err, store.ErrAlreadyConsumed) {
			t.Fatalf("second consume by another claimant: %v", err)
		}
		if _, err := e.st.Consume(ctx, "apr_1", "", now); !errors.Is(err, store.ErrAlreadyConsumed) {
			t.Fatalf("second consume without a claimant: %v", err)
		}
		if r, err := e.st.Consume(ctx, "apr_1", "c", now); err != nil || r.Status != store.StatusConsumed {
			t.Fatalf("same claimant again must succeed: %v %v", r.Status, err)
		}

		create(t, e, "apr_2", "k2", "email-send")
		if _, err := e.st.Deny(ctx, "apr_2", now); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Approve(ctx, "apr_2", now, false, signFn(t, sg, now)); !errors.Is(err, store.ErrInvalidTransition) {
			t.Fatalf("approve after deny: %v", err)
		}
	})
}

func TestConsumeRefusesLapsedApproval(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		now := e.clk.now()
		create(t, e, "apr_1", "k", "email-send")
		if _, err := e.st.Approve(ctx, "apr_1", now, false, signFn(t, signer(t), now)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Consume(ctx, "apr_1", "c", now.Add(store.ReceiptLifetime)); !errors.Is(err, store.ErrExpired) {
			t.Fatalf("consume after the receipt lapsed: %v", err)
		}
		e.clk.add(store.ReceiptLifetime + time.Second)
		res, err := e.st.Sweep(ctx, "pat", e.clk.now())
		if err != nil || res.PreviewsDeleted != 1 || hasPreview(t, e, "apr_1") {
			t.Fatalf("sweep of a lapsed approval: %+v, %v, preview=%v", res, err, hasPreview(t, e, "apr_1"))
		}
	})
}

func TestApproveNeedsRecordedFromTap(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		now := e.clk.now()
		create(t, e, "apr_1", "k", "email-send")
		if _, err := e.st.Approve(ctx, "apr_1", now, true, signFn(t, signer(t), now)); !errors.Is(err, store.ErrNeedsFromConfirm) {
			t.Fatalf("approve without the tap: %v", err)
		}
		if r, _ := e.st.GetRequest(ctx, "apr_1"); r.Status != store.StatusPending || r.Receipt != nil {
			t.Fatalf("a refused approval changed the request: %+v", r)
		}
		first, err := e.st.ConfirmFrom(ctx, "apr_1", now)
		if err != nil || first.FromConfirmedAt == nil {
			t.Fatalf("ConfirmFrom: %v %v", first.FromConfirmedAt, err)
		}
		again, err := e.st.ConfirmFrom(ctx, "apr_1", now.Add(time.Minute))
		if err != nil || !again.FromConfirmedAt.Equal(*first.FromConfirmedAt) {
			t.Fatalf("a second tap moved the recorded time: %v vs %v", again.FromConfirmedAt, first.FromConfirmedAt)
		}
		if _, err := e.st.Approve(ctx, "apr_1", now, true, signFn(t, signer(t), now)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestChallengeIsSingleUse(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		c := store.Challenge{Hash: store.Hash("chal"), Purpose: "approve", Owner: "pat", RequestID: "apr_1", PayloadSHA256: "p", Session: json.RawMessage(`{}`), Expires: e.clk.now().Add(store.ChallengeTTL)}
		if err := e.st.PutChallenge(ctx, c); err != nil {
			t.Fatal(err)
		}
		var ok, notFound atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := e.st.TakeChallenge(ctx, c.Hash, e.clk.now())
				switch {
				case err == nil && got.RequestID == "apr_1":
					ok.Add(1)
				case errors.Is(err, store.ErrNotFound):
					notFound.Add(1)
				default:
					t.Errorf("TakeChallenge: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 1 || notFound.Load() != 7 {
			t.Fatalf("8 concurrent takes: %d succeeded, %d not found; want exactly 1", ok.Load(), notFound.Load())
		}
	})
}

func TestChallengeExpires(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		c := store.Challenge{Hash: store.Hash("chal"), Purpose: "approve", Owner: "pat", RequestID: "apr_1", Session: json.RawMessage(`{}`), Expires: e.clk.now().Add(store.ChallengeTTL)}
		_ = e.st.PutChallenge(ctx, c)
		if _, err := e.st.TakeChallenge(ctx, c.Hash, c.Expires); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expired challenge: %v", err)
		}
	})
}

func TestEnrollTokenIsHashedSingleUseAndExpires(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		now := e.clk.now()
		tok := store.NewToken()
		if err := e.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok), Owner: "pat", Expires: now.Add(store.EnrollTTL)}); err != nil {
			t.Fatal(err)
		}
		raw, ok, _ := e.b.Get(ctx, "ENROLL#"+store.Hash(tok), "META")
		if !ok || bytes.Contains(raw.Data, []byte(tok)) {
			t.Fatalf("token row: ok=%v, or it holds the token in the clear", ok)
		}
		if _, err := e.st.PeekEnrollToken(ctx, store.Hash(tok), now); err != nil {
			t.Fatal(err)
		}
		cred := store.Credential{Owner: "pat", ID: "cred1", Data: json.RawMessage(`{}`), Created: now}
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := cred
				c.ID = "cred" + string(rune('a'+i))
				if err := e.st.RedeemEnrollToken(ctx, store.Hash(tok), now, c); err == nil {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d concurrent redemptions won, want exactly 1", wins.Load())
		}
		if err := e.st.RedeemEnrollToken(ctx, store.Hash(tok), now, cred); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("token reuse: %v", err)
		}
		if creds, _ := e.st.ListCredentials(ctx, "pat"); len(creds) != 1 {
			t.Fatalf("%d credentials stored, want 1", len(creds))
		}

		tok2 := store.NewToken()
		_ = e.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok2), Owner: "pat", Expires: now.Add(store.EnrollTTL)})
		if _, err := e.st.PeekEnrollToken(ctx, store.Hash(tok2), now.Add(store.EnrollTTL)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expired token peeked: %v", err)
		}
		if err := e.st.RedeemEnrollToken(ctx, store.Hash(tok2), now.Add(store.EnrollTTL), store.Credential{Owner: "pat", ID: "late"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expired token redeemed: %v", err)
		}
	})
}

func TestApproveBatchApprovesKeptAndDeniesDropped(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		now := e.clk.now()
		for _, id := range []string{"apr_a", "apr_b", "apr_c"} {
			create(t, e, id, "k-"+id, "email-send")
		}
		create(t, e, "apr_batch", "kb", "email-send-batch")
		if _, err := e.st.ConfirmFrom(ctx, "apr_batch", now); err != nil {
			t.Fatal(err)
		}
		_, err := e.st.ApproveBatch(ctx, "apr_batch", now, []string{"apr_a", "apr_c"}, []string{"apr_b"},
			func(store.Request) ([]byte, string, error) { return []byte(`{"final":true}`), "final-sha", nil },
			signFn(t, signer(t), now))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := e.st.GetRequest(ctx, "apr_batch")
		if b.Status != store.StatusApproved || b.PayloadSHA256 != "final-sha" || b.OrigPayloadSHA256 != "sha-apr_batch" || b.Receipt.Body.PayloadSHA256 != "final-sha" {
			t.Fatalf("batch after approval: %+v", b)
		}
		for id, want := range map[string]string{"apr_a": store.StatusApproved, "apr_b": store.StatusDenied, "apr_c": store.StatusApproved} {
			r, _ := e.st.GetRequest(ctx, id)
			if r.Status != want || r.BatchID != "apr_batch" {
				t.Errorf("%s: status %s batch %q, want %s via apr_batch", id, r.Status, r.BatchID, want)
			}
		}
		if hasPreview(t, e, "apr_b") {
			t.Error("a dropped item kept its preview")
		}
		// Each kept item is consumed on its own.
		if _, err := e.st.Consume(ctx, "apr_a", "claim-a", now); err != nil {
			t.Fatal(err)
		}
		if r, _ := e.st.GetRequest(ctx, "apr_c"); r.Status != store.StatusApproved {
			t.Fatalf("consuming one item changed another: %s", r.Status)
		}
	})
}

func TestApproveBatchIsAtomic(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		now := e.clk.now()
		create(t, e, "apr_a", "ka", "email-send")
		create(t, e, "apr_batch", "kb", "email-send-batch")
		_, _ = e.st.ConfirmFrom(ctx, "apr_batch", now)
		// One item was already decided elsewhere.
		create(t, e, "apr_b", "kb2", "email-send")
		_, _ = e.st.Deny(ctx, "apr_b", now)
		_, err := e.st.ApproveBatch(ctx, "apr_batch", now, []string{"apr_a", "apr_b"}, nil,
			func(store.Request) ([]byte, string, error) { return []byte(`{}`), "s", nil }, signFn(t, signer(t), now))
		if err == nil {
			t.Fatal("a batch with a decided item was approved")
		}
		for _, id := range []string{"apr_a", "apr_batch"} {
			if r, _ := e.st.GetRequest(ctx, id); r.Status != store.StatusPending {
				t.Errorf("%s moved to %s although the batch failed", id, r.Status)
			}
		}
	})
}

func TestListOpenReturnsOnlyPendingAndApproved(t *testing.T) {
	backends(t, func(t *testing.T, e env) {
		ctx := context.Background()
		now := e.clk.now()
		create(t, e, "apr_p", "k1", "email-send")
		create(t, e, "apr_d", "k2", "email-send")
		create(t, e, "apr_a", "k3", "email-send")
		_, _ = e.st.Deny(ctx, "apr_d", now)
		_, _ = e.st.Approve(ctx, "apr_a", now, false, signFn(t, signer(t), now))
		open, err := e.st.ListOpen(ctx, "pat")
		if err != nil || len(open) != 2 {
			t.Fatalf("ListOpen = %d rows, %v; want pending and approved only", len(open), err)
		}
		if other, _ := e.st.ListOpen(ctx, "someone-else"); len(other) != 0 {
			t.Fatal("ListOpen leaked another owner's requests")
		}
	})
}
