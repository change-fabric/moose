package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/change-fabric/moose/internal/store"
	"github.com/change-fabric/moose/internal/store/sqlite"
	"github.com/change-fabric/moose/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "moose.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
