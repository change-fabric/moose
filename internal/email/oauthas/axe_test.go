package oauthas_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/email/oauthas"
)

// TestDumpPagesForAxe writes the dashboard sign-in page, its approved state
// (the paste panel the script reveals) and its error page to MOOSE_AXE_DUMP so
// `make a11y-approver` can run axe-core over them.
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
	e := newEnv(t)
	_, wait := e.get(e.browser(), e.base+oauthas.LoginPath)
	// The dump is opened from a file, so the poll never answers; drop the
	// script and show the approved state as the script would.
	if i := strings.Index(wait, "<script"); i >= 0 {
		wait = wait[:i] + "</main></body></html>"
	}
	write("login-wait", wait)
	write("login-approved", strings.NewReplacer(
		`id="paste" class="card" hidden`, `id="paste" class="card"`,
		`class="qr"`, `class="qr" hidden`, `class="scan"`, `class="scan" hidden`, `class="here"`, `class="here" hidden`,
		`aria-live="polite">Waiting for approval`, `aria-live="polite">Approved. One step left.`,
	).Replace(wait))

	e.ap.failCreate = true
	_, failed := e.get(e.browser(), e.base+oauthas.LoginPath)
	write("login-error", failed)
}
