package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/connectors/fake"
	"github.com/change-fabric/moose/internal/core"
	"github.com/change-fabric/moose/internal/server"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

const (
	me     = "me@example.com"
	secret = "relay-secret-for-tests"
)

func TestLambdaAdapter(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "moose.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := auth.EnsureBootstrapAdmin(ctx, st, me, time.Now()); err != nil {
		t.Fatal(err)
	}
	plain, tok, err := auth.NewToken(me, "test", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	svc, err := core.NewService([]core.Connector{fake.New("gdrive")}, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Authenticator{Store: st, Identity: auth.HeaderIdentity{Secret: []byte(secret)}}
	t.Cleanup(a.Wait)
	invoke := proxy(server.New(server.Deps{Service: svc, Store: st, Auth: a, Version: "test"}))

	relay := map[string]string{
		"Authorization":        "Bearer " + plain,
		auth.HeaderRelaySecret: secret,
		auth.HeaderTSUserLogin: me,
		auth.HeaderTSUserName:  "laptop",
	}
	withSecret := func(s string) map[string]string {
		h := map[string]string{}
		for k, v := range relay {
			h[k] = v
		}
		h[auth.HeaderRelaySecret] = s
		return h
	}
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"healthz", "/healthz", nil, 200},
		{"whoami via relay", "/v1/whoami", relay, 200},
		{"whoami wrong relay secret", "/v1/whoami", withSecret("nope"), 401},
		{"whoami no relay secret", "/v1/whoami", withSecret(""), 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := invoke(ctx, events.APIGatewayProxyRequest{
				HTTPMethod: "GET",
				Path:       c.path,
				Headers:    c.headers,
				RequestContext: events.APIGatewayProxyRequestContext{
					Stage: "v0", HTTPMethod: "GET", Path: "/v0" + c.path,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d; body %s", resp.StatusCode, c.want, resp.Body)
			}
			if c.path == "/v1/whoami" && c.want == 200 {
				var body map[string]any
				if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
					t.Fatal(err)
				}
				if body["login"] != me {
					t.Fatalf("whoami = %v", body)
				}
			}
		})
	}
}

// The config bundled into the Lambda zip must validate with the env Terraform sets.
func TestBundledConfig(t *testing.T) {
	env := map[string]string{
		"MOOSE_CONFIG":       "../../deploy/lambda/moose.yaml",
		"MOOSE_DYNAMO_TABLE": "moose-dev",
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Kind != config.StoreDynamo || cfg.Identity.Mode != config.IdentityRelayHeaders || cfg.Env != config.EnvProd {
		t.Fatalf("cfg = %+v", cfg)
	}
}
