package graphapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "email", "graph", "testdata", "synthetic", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func serve(t *testing.T, status int, headers map[string]string, body string) *graphapi.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	c := graphapi.New(nil)
	c.BaseURL = ts.URL
	return c
}

func TestDefaultBaseURL(t *testing.T) {
	if got := graphapi.New(nil).BaseURL; got != "https://graph.microsoft.com/v1.0" {
		t.Errorf("BaseURL = %q", got)
	}
}

func TestRequestsCarryBearerAndImmutableID(t *testing.T) {
	var gotAuth, gotPrefer, gotURI string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPrefer, gotURI = r.Header.Get("Authorization"), r.Header.Get("Prefer"), r.URL.RequestURI()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c := graphapi.New(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok-123", TokenType: "Bearer"}))
	c.BaseURL = ts.URL
	var out map[string]any
	err := c.GetJSON(context.Background(), "/me/messages", url.Values{"$search": {`"a b"`}},
		http.Header{"Prefer": {`outlook.body-content-type="text"`}}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotPrefer != `IdType="ImmutableId", outlook.body-content-type="text"` {
		t.Errorf("Prefer = %q, want ImmutableId plus the caller's value", gotPrefer)
	}
	if strings.Contains(gotURI, "+") || !strings.Contains(gotURI, "a%20b") {
		t.Errorf("uri %q should encode spaces as %%20", gotURI)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		is      error
		retry   time.Duration
		notAny  bool // none of the sentinels
	}{
		{name: "401 is auth", status: 401, body: `{}`, is: provider.ErrAuth},
		{name: "401 claims challenge is auth", status: 401, headers: map[string]string{"WWW-Authenticate": `Bearer error="insufficient_claims", claims="e30="`},
			body: "fixture:error_claims_challenge.json", is: provider.ErrAuth},
		{name: "AADSTS70008 is auth", status: 400, body: `{"error":{"code":"invalid_grant","message":"AADSTS70008: The provided authorization code or refresh token has expired"}}`, is: provider.ErrAuth},
		{name: "AADSTS700082 is auth", status: 400, body: `{"error":{"code":"x","message":"AADSTS700082: The refresh token has expired due to inactivity."}}`, is: provider.ErrAuth},
		{name: "interaction_required is auth", status: 403, body: `{"error":{"code":"interaction_required","message":"x"}}`, is: provider.ErrAuth},
		{name: "404 is not found", status: 404, body: "fixture:error_item_not_found.json", is: provider.ErrNotFound},
		{name: "ErrorInvalidIdMalformed is not found", status: 400, body: "fixture:error_id_malformed.json", is: provider.ErrNotFound},
		{name: "429 with Retry-After", status: 429, headers: map[string]string{"Retry-After": "7"}, body: "fixture:error_throttled.json", is: provider.ErrRateLimited, retry: 7 * time.Second},
		{name: "429 without Retry-After", status: 429, body: `{}`, is: provider.ErrRateLimited},
		{name: "503 with Retry-After is rate limited", status: 503, headers: map[string]string{"Retry-After": "30"}, body: `{}`, is: provider.ErrRateLimited, retry: 30 * time.Second},
		{name: "503 without Retry-After is plain", status: 503, body: `{}`, notAny: true},
		{name: "InefficientFilter is invalid query", status: 400, body: "fixture:error_inefficient_filter.json", is: provider.ErrInvalidQuery},
		{name: "other 400 is invalid query", status: 400, body: `{"error":{"code":"ErrorInvalidRequest","message":"x"}}`, is: provider.ErrInvalidQuery},
		{name: "other 4xx is rejected", status: 409, body: `{"error":{"code":"ErrorConflict","message":"x"}}`, is: provider.ErrRejected},
		{name: "500 is plain", status: 500, body: `not json`, notAny: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if name, ok := strings.CutPrefix(body, "fixture:"); ok {
				body = fixture(t, name)
			}
			c := serve(t, tc.status, tc.headers, body)
			_, err := c.Do(context.Background(), graphapi.Request{Path: "/me"})
			if err == nil {
				t.Fatal("no error")
			}
			if tc.notAny {
				for _, s := range []error{provider.ErrAuth, provider.ErrNotFound, provider.ErrRateLimited, provider.ErrInvalidQuery, provider.ErrRejected} {
					if errors.Is(err, s) {
						t.Errorf("err %v wrongly wraps %v", err, s)
					}
				}
				return
			}
			if !errors.Is(err, tc.is) {
				t.Fatalf("err = %v, want %v", err, tc.is)
			}
			if tc.is == provider.ErrRateLimited {
				var rl *provider.RateLimitError
				if !errors.As(err, &rl) || rl.RetryAfter != tc.retry {
					t.Errorf("RetryAfter = %+v, want %v", rl, tc.retry)
				}
			}
		})
	}
}

type failingSource struct{ err error }

func (f failingSource) Token() (*oauth2.Token, error) { return nil, f.err }

func TestTokenRefreshFailuresMapToAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request sent without a token") }))
	defer ts.Close()
	cases := []struct {
		name string
		err  error
		auth bool
	}{
		{"invalid_grant", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant", Body: []byte(`{"error":"invalid_grant"}`)}, true},
		{"AADSTS700082", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, Body: []byte(`{"error_description":"AADSTS700082: expired"}`)}, true},
		{"interaction_required", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "interaction_required"}, true},
		{"server error is not a reauth", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 500}, Body: []byte("oops")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := graphapi.New(failingSource{tc.err})
			c.BaseURL = ts.URL
			_, err := c.Do(context.Background(), graphapi.Request{Path: "/me"})
			if errors.Is(err, provider.ErrAuth) != tc.auth {
				t.Errorf("err = %v, ErrAuth = %v want %v", err, errors.Is(err, provider.ErrAuth), tc.auth)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":     0,
		"7":    7 * time.Second,
		" 30 ": 30 * time.Second,
		"-5":   0,
		"soon": 0,
		now.Add(90 * time.Second).UTC().Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).UTC().Format(http.TimeFormat):     0,
	} {
		if got := graphapi.ParseRetryAfter(in, now); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestErrorDetailIsBounded(t *testing.T) {
	c := serve(t, 400, nil, `{"error":{"code":"x","message":"`+strings.Repeat("a", 5000)+`"}}`)
	_, err := c.Do(context.Background(), graphapi.Request{Path: "/me"})
	if err == nil || len(err.Error()) > 600 {
		t.Errorf("error length %d, want a truncated message", len(err.Error()))
	}
}
