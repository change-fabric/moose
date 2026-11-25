package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/email/store/sqlite"
	"github.com/change-fabric/moose/internal/email/store/storetest"
)

func open(t *testing.T) *sqlite.Backend {
	t.Helper()
	b, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestBackendConformance(t *testing.T) {
	storetest.RunBackend(t, func(t *testing.T) store.Backend { return open(t) })
}

func TestStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return store.New(open(t)) })
}

func TestPurgeExpired(t *testing.T) {
	ctx := context.Background()
	b := open(t)
	now := time.Now()
	if err := b.Transact(ctx, []store.Op{
		{Kind: store.OpPut, Item: store.Item{PK: "p", SK: "old", Data: []byte("x"), TTL: now.Add(-time.Minute)}},
		{Kind: store.OpPut, Item: store.Item{PK: "p", SK: "new", Data: []byte("x"), TTL: now.Add(time.Hour)}},
		{Kind: store.OpPut, Item: store.Item{PK: "p", SK: "forever", Data: []byte("x")}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := b.PurgeExpired(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("PurgeExpired = %d, %v; want 1", n, err)
	}
	if _, ok, _ := b.Get(ctx, "p", "old"); ok {
		t.Fatal("expired row survived")
	}
	for _, sk := range []string{"new", "forever"} {
		if _, ok, _ := b.Get(ctx, "p", sk); !ok {
			t.Fatalf("row %s was purged", sk)
		}
	}
}
