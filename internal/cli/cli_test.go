package cli_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/cli"
	"github.com/change-fabric/moose/internal/connectors/fake"
	"github.com/change-fabric/moose/internal/core"
	"github.com/change-fabric/moose/internal/server"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

func TestCLIAgainstServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "moose.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = auth.EnsureBootstrapAdmin(ctx, st, "me@example.com", time.Now())
	svc, _ := core.NewService([]core.Connector{fake.New("gdrive",
		core.Document{DocRef: core.DocRef{ID: "doc1", Title: "Roadmap"}, Content: "moose roadmap", ContentType: "text/plain"})}, core.Options{})
	a := &auth.Authenticator{Store: st, Identity: auth.DevIdentity{Login: "me@example.com"}}
	srv := httptest.NewServer(server.New(server.Deps{Service: svc, Store: st, Auth: a, Version: "test"}))
	defer func() { srv.Close(); a.Wait() }()

	envs := map[string]string{"MOOSE_CREDENTIALS": filepath.Join(dir, "creds.json")}
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		root := cli.NewRoot(cli.Env{Getenv: func(k string) string { return envs[k] }, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Version: "test"})
		root.SetArgs(args)
		err := root.ExecuteContext(ctx)
		return cli.ExitCode(err), out.String() + errb.String()
	}
	expect := func(code int, args ...string) string {
		t.Helper()
		got, out := run(args...)
		if got != code {
			t.Fatalf("moose %v = exit %d, want %d: %s", args, got, code, out)
		}
		return out
	}

	expect(cli.ExitUsage, "whoami") // no URL configured
	expect(cli.ExitOK, "token", "bootstrap", "--url", srv.URL, "--name", "dev")
	if out := expect(cli.ExitOK, "whoami"); !strings.Contains(out, "me@example.com") {
		t.Fatalf("whoami: %s", out)
	}
	expect(cli.ExitOK, "sources")
	if out := expect(cli.ExitOK, "search", "roadmap"); !strings.Contains(out, "doc1") {
		t.Fatalf("search: %s", out)
	}
	if out := expect(cli.ExitOK, "get", "doc1"); out != "moose roadmap\n" {
		t.Fatalf("get: %q", out)
	}
	expect(cli.ExitNotFound, "get", "nope")
	expect(cli.ExitOK, "user", "add", "alice@example.com")
	expect(cli.ExitOK, "user", "list")
	expect(cli.ExitOK, "token", "list", "--all")
	expect(cli.ExitOK, "audit", "--since", "1h")
	expect(cli.ExitUsage, "get")
	expect(cli.ExitUsage, "bogus")
	envs["MOOSE_PAT"] = "moose_pat_abcdefgh2345_bad"
	expect(cli.ExitAuth, "whoami")
}
