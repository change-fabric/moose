package connect_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/store"
)

// TestDumpPagesForAxe writes the accounts page and its error page to
// MOOSE_AXE_DUMP so `make a11y-approver` can run axe-core over them.
func TestDumpPagesForAxe(t *testing.T) {
	dir := os.Getenv("MOOSE_AXE_DUMP")
	if dir == "" {
		t.Skip("set MOOSE_AXE_DUMP to a directory (make a11y-approver does)")
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t)
	_, empty := r.get(connect.PagePath)
	write("connect-empty", empty)

	r.seed("Alpha", "alpha@x.example", 0, store.StatusOK)
	on := r.seed("Bravo", "bravo@x.example", 1, store.StatusOK)
	_ = r.st.UpdateAccount(context.Background(), on.Owner, on.ID, store.AccountPatch{SendEnabled: store.Ptr(true)})
	r.seed("Charlie", "charlie@x.example", 2, store.StatusNeedsReauth)
	r.seed("Delta", "delta@x.example", 13, store.StatusDisconnected)
	res, body := r.get(connect.PagePath + "?connected=" + on.ID)
	r.wantStatus(res, http.StatusOK)
	write("connect-accounts", body)

	_, failed := r.get("/connect/google/callback?state=nope&code=x")
	write("connect-error", failed)
}
