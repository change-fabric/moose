package approver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
)

// TestDumpPagesForAxe renders every kind of approver page to HTML files so that
// `make a11y-approver` can run axe-core over them in a browserless Chromium
// container. It writes nothing unless MOOSE_AXE_DUMP names a directory, because
// the browser step needs Docker.
func TestDumpPagesForAxe(t *testing.T) {
	dir := os.Getenv("MOOSE_AXE_DUMP")
	if dir == "" {
		t.Skip("set MOOSE_AXE_DUMP to a directory (make a11y-approver does)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.enroll()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".html"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A busy banner: a recent send, every kind of look-alike warning, and an
	// account past the first pass of colours, which gets a pattern overlay.
	patterned := acct{"acc_13", "Thirteenth", "alice@example.com", "Alice Doe", 13}
	siblings := []api.Sibling{
		{AccountID: "s1", Nickname: "Old work", Address: "аlice@example.com", DisplayName: "Someone"},
		{AccountID: "s2", Nickname: "Typo", Address: "alice@exarnple.com", DisplayName: "Other"},
		{AccountID: "s3", Nickname: "Namesake", Address: "totally.different@elsewhere.example", DisplayName: "Alice Doe"},
	}
	last := &api.LastSent{At: t0.Add(-3 * time.Hour), Recipients: 2}
	send := h.create(sendBody(patterned, sendOpts{Siblings: siblings, Last: last}))
	write("send-before-tap", h.get("/approve/r/"+send.RequestID).Body)
	h.tap(send.RequestID)
	write("send-after-tap", h.get("/approve/r/"+send.RequestID).Body)

	plain := h.create(sendBody(personal, sendOpts{Draft: "plain"}))
	write("send-plain", h.get("/approve/r/"+plain.RequestID).Body)

	enable := h.create(enableBody(work, "enable-axe"))
	write("enable", h.get("/approve/r/"+enable.RequestID).Body)

	batch, _, _ := h.batchOf(work, "one", "two")
	write("batch", h.get("/approve/r/"+batch.RequestID).Body)

	trust, _ := json.Marshal(api.ContactTrustPayload{Owner: owner, AccountID: work.ID, Changes: []api.TrustChange{{Address: "carol@x.example", To: "trusted"}}})
	pv, _ := json.Marshal(api.Preview{Nickname: work.Nick})
	tr := h.create(api.CreateBody{IdempotencyKey: "trust-axe", Kind: api.KindContactTrustBatch, Payload: trust, Preview: pv})
	write("trust", h.get("/approve/r/"+tr.RequestID).Body)

	dash, _ := json.Marshal(api.DashboardSessionPayload{Owner: owner, ClientID: "client-1", ClientName: "Claude", RedirectURIHost: "claude.ai", TxnHash: sum("x")})
	ds := h.create(api.CreateBody{IdempotencyKey: "dash-axe", Kind: api.KindDashboardSession, Payload: dash})
	write("dashboard", h.get("/approve/r/"+ds.RequestID).Body)

	write("queue-signed-out", h.get("/approve/queue").Body)
	write("queue", h.get("/approve/queue", h.queueLogin()).Body)

	h.decide(ds.RequestID, "deny")
	write("denied", h.get("/approve/r/"+ds.RequestID).Body)
	write("not-found", h.get("/approve/r/apr_missing").Body)
}
