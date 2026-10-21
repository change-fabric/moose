package audit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/change-fabric/moose/internal/store"
)

type memAppender struct {
	mu  sync.Mutex
	evs []store.AuditEvent
	err error
}

func (m *memAppender) AppendAudit(_ context.Context, e store.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.evs = append(m.evs, e)
	return nil
}

func TestMiddlewareRecordsEvent(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		ua          string
		handler     http.HandlerFunc
		wantStatus  int
		wantSurface string
		wantAction  string
		wantActor   string
	}{
		{
			name: "authenticated rest call",
			path: "/v1/search",
			handler: func(w http.ResponseWriter, r *http.Request) {
				e := FromContext(r.Context())
				e.SetActor("me@example.com", "abc")
				e.SetAction("search", "gdrive", "")
				w.WriteHeader(http.StatusOK)
			},
			wantStatus: 200, wantSurface: "rest", wantAction: "search", wantActor: "me@example.com",
		},
		{
			name: "unauthenticated 401 keeps empty actor",
			path: "/v1/whoami",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "no", http.StatusUnauthorized)
			},
			wantStatus: 401, wantSurface: "rest", wantAction: "GET /v1/whoami",
		},
		{
			name:        "mcp surface and implicit 200",
			path:        "/mcp",
			handler:     func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) },
			wantStatus:  200,
			wantSurface: "mcp",
			wantAction:  "GET /mcp",
		},
		{
			name:        "cli surface by user agent",
			path:        "/v1/sources",
			ua:          "moose/dev",
			handler:     func(w http.ResponseWriter, r *http.Request) {},
			wantStatus:  200,
			wantSurface: "cli",
			wantAction:  "GET /v1/sources",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := &memAppender{}
			h := Middleware(app, Options{}, tc.handler)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.ua != "" {
				req.Header.Set("User-Agent", tc.ua)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if len(app.evs) != 1 {
				t.Fatalf("got %d events, want 1", len(app.evs))
			}
			ev := app.evs[0]
			if ev.Status != tc.wantStatus || ev.Surface != tc.wantSurface || ev.Action != tc.wantAction || ev.Actor != tc.wantActor {
				t.Fatalf("event = %+v", ev)
			}
			if ev.RequestID == "" || rr.Header().Get(RequestIDHeader) != ev.RequestID {
				t.Fatalf("request id header %q vs event %q", rr.Header().Get(RequestIDHeader), ev.RequestID)
			}
		})
	}
}

func TestAppendFailureDoesNotFailRequest(t *testing.T) {
	app := &memAppender{err: errors.New("boom")}
	h := Middleware(app, Options{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestNilEntrySettersAreSafe(t *testing.T) {
	var e *Entry
	e.SetActor("a", "b")
	e.SetAction("x", "y", "z")
	e.SetDetail("d")
	if e.RequestID() != "" {
		t.Fatal("nil entry request id")
	}
}
