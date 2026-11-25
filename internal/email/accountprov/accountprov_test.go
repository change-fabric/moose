package accountprov_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/email/accountprov"
	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/email/store/sqlite"
)

type fixture struct {
	src      *accountprov.Source
	st       store.Store
	acct     store.Account
	refreshN *atomic.Int32
	gmailHdr chan string
}

// newFixture stands up a token endpoint that answers `tokenStatus` and a Gmail
// stub that records the Authorization header it sees.
func newFixture(t *testing.T, tokenStatus int, tokenBody string) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seal, err := envelope.NewLocal([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, refreshN: &atomic.Int32{}, gmailHdr: make(chan string, 200)}

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.refreshN.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		_ = r.ParseForm()
		if r.PostForm.Get("refresh_token") != "the-refresh-token" {
			t.Errorf("refresh_token = %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(tokenStatus)
		_, _ = w.Write([]byte(tokenBody))
	}))
	t.Cleanup(tokenSrv.Close)
	gmailSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gmailHdr <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}}})
	}))
	t.Cleanup(gmailSrv.Close)

	sealed, err := seal.Seal(ctx, []byte("the-refresh-token"), envelope.Context{Owner: "pat", AccountID: "acc_one"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.acct = store.Account{Owner: "pat", ID: "acc_one", Provider: "gmail", ProviderUID: "a@example.com", Address: "a@example.com",
		Nickname: "A", Status: store.StatusOK, HourlyCap: 10, RefreshToken: sealed, Created: now, Updated: now}
	if err := st.CreateAccount(ctx, f.acct); err != nil {
		t.Fatal(err)
	}
	f.src = &accountprov.Source{
		Store: st, Seal: seal,
		OAuth:        &oauth2.Config{ClientID: "id", ClientSecret: "sec", Endpoint: oauth2.Endpoint{TokenURL: tokenSrv.URL, AuthStyle: oauth2.AuthStyleInParams}},
		GmailOptions: []option.ClientOption{option.WithEndpoint(gmailSrv.URL)},
	}
	return f
}

const goodToken = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600}`

func TestFiftyConcurrentCallsRefreshExactlyOnce(t *testing.T) {
	f := newFixture(t, 200, goodToken)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := f.src.Provider(context.Background(), f.acct)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := p.ListMailboxes(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.refreshN.Load(); n != 1 {
		t.Fatalf("token endpoint called %d times, want exactly 1", n)
	}
	close(f.gmailHdr)
	n := 0
	for h := range f.gmailHdr {
		n++
		if h != "Bearer at-1" {
			t.Fatalf("Gmail saw Authorization %q", h)
		}
	}
	if n != 50 {
		t.Fatalf("Gmail saw %d requests, want 50", n)
	}
}

func TestExpiredTokenIsRefreshedAgain(t *testing.T) {
	f := newFixture(t, 200, `{"access_token":"at-1","token_type":"Bearer","expires_in":30}`)
	// 30s lifetime is inside the one-minute skew, so every call refreshes.
	for i := 0; i < 3; i++ {
		if _, err := f.src.Token(context.Background(), f.acct); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.refreshN.Load(); n != 3 {
		t.Fatalf("refreshes = %d, want 3", n)
	}
}

func TestInvalidGrantMovesTheAccountToNeedsReauth(t *testing.T) {
	f := newFixture(t, 400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	_, err := f.src.Token(context.Background(), f.acct)
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	got, err := f.st.GetAccount(context.Background(), "pat", "acc_one")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusNeedsReauth || got.LastError != "invalid_grant" {
		t.Fatalf("account = %s / %q", got.Status, got.LastError)
	}
}

func TestOtherRefreshFailuresLeaveTheAccountAlone(t *testing.T) {
	f := newFixture(t, 500, `{"error":"backend_error"}`)
	if _, err := f.src.Token(context.Background(), f.acct); err == nil || errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want a plain refresh error", err)
	}
	got, _ := f.st.GetAccount(context.Background(), "pat", "acc_one")
	if got.Status != store.StatusOK {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestMissingOrForeignCredentialIsAuthError(t *testing.T) {
	f := newFixture(t, 200, goodToken)
	none := f.acct
	none.RefreshToken = nil
	if _, err := f.src.Token(context.Background(), none); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("no credential: %v", err)
	}
	// A blob sealed for another account does not open (encryption context).
	other := f.acct
	other.ID = "acc_other"
	if _, err := f.src.Token(context.Background(), other); err == nil {
		t.Fatal("a credential sealed for acc_one must not open as acc_other")
	}
}

func TestUnsupportedProvider(t *testing.T) {
	f := newFixture(t, 200, goodToken)
	a := f.acct
	a.Provider = "imap"
	if _, err := f.src.Provider(context.Background(), a); err == nil {
		t.Fatal("want an error")
	}
}
