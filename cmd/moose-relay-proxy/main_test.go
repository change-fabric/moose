package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyAddsSecretAndKeepsStage(t *testing.T) {
	var gotPath, gotSecret, gotLogin, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotSecret, gotLogin, gotHost = r.URL.Path, r.Header.Get(secretHeader), r.Header.Get("Tailscale-User-Login"), r.Host
	}))
	defer up.Close()
	h, err := newProxy(up.URL+"/v0", "api.example", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set(secretHeader, "forged")
	req.Header.Set("Tailscale-User-Login", "me@example.com")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if gotPath != "/v0/v1/whoami" || gotSecret != "s3cret" || gotLogin != "me@example.com" || gotHost != "api.example" {
		t.Fatalf("path=%q secret=%q login=%q host=%q", gotPath, gotSecret, gotLogin, gotHost)
	}
}
