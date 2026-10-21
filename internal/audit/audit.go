// Package audit records one store.AuditEvent per request. The middleware sits
// outermost so failed and unauthenticated requests are recorded too; inner
// layers (auth, handlers) fill in the actor, action, and target through the
// *Entry carried in the request context.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/store"
)

// RequestIDHeader is echoed on every response so callers can quote it.
const RequestIDHeader = "X-Request-ID"

// Appender is the slice of store.Store the audit log needs.
type Appender interface {
	AppendAudit(ctx context.Context, e store.AuditEvent) error
}

// Entry is the mutable audit record for one in-flight request.
type Entry struct {
	mu sync.Mutex
	ev store.AuditEvent
}

type ctxKey struct{}

// FromContext returns the request's entry, or nil outside the middleware.
func FromContext(ctx context.Context) *Entry {
	e, _ := ctx.Value(ctxKey{}).(*Entry)
	return e
}

// WithEntry attaches e to ctx. Exposed for tests and non-HTTP callers.
func WithEntry(ctx context.Context, e *Entry) context.Context {
	return context.WithValue(ctx, ctxKey{}, e)
}

// SetActor records who made the request. Safe on a nil entry.
func (e *Entry) SetActor(actor, tokenID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.ev.Actor, e.ev.TokenID = actor, tokenID
	e.mu.Unlock()
}

// SetAction records what was done and to which document. Empty values leave
// earlier ones in place.
func (e *Entry) SetAction(action, source, docID string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if action != "" {
		e.ev.Action = action
	}
	if source != "" {
		e.ev.Source = source
	}
	if docID != "" {
		e.ev.DocID = docID
	}
	e.mu.Unlock()
}

// SetDetail records a short, secret-free note (an error code, for example).
func (e *Entry) SetDetail(detail string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.ev.Detail = detail
	e.mu.Unlock()
}

// RequestID returns the request's id ("" on a nil entry).
func (e *Entry) RequestID() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ev.RequestID
}

// Event returns a copy of the current record.
func (e *Entry) Event() store.AuditEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ev
}

// Options configures the middleware.
type Options struct {
	// Surface classifies a request as rest, mcp, or cli. Default: SurfaceOf.
	Surface func(*http.Request) string
	// Logger receives audit write failures. Default: slog.Default().
	Logger *slog.Logger
	// Now is the clock. Default: time.Now.
	Now func() time.Time
}

// SurfaceOf is the default classifier: /mcp is mcp, a request carrying the
// moose CLI user agent is cli, everything else is rest.
func SurfaceOf(r *http.Request) string {
	switch {
	case r.URL.Path == "/mcp" || len(r.URL.Path) > 4 && r.URL.Path[:5] == "/mcp/":
		return "mcp"
	case len(r.UserAgent()) >= 6 && r.UserAgent()[:6] == "moose/":
		return "cli"
	default:
		return "rest"
	}
}

// Middleware wraps next so every request appends one audit event. A failed
// write is logged and never fails the request (pre-alpha choice).
func Middleware(dst Appender, opts Options, next http.Handler) http.Handler {
	if opts.Surface == nil {
		opts.Surface = SurfaceOf
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := opts.Now()
		e := &Entry{ev: store.AuditEvent{
			At:        start.UTC(),
			RequestID: newRequestID(),
			Surface:   opts.Surface(r),
			Action:    r.Method + " " + r.URL.Path,
		}}
		w.Header().Set(RequestIDHeader, e.ev.RequestID)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(WithEntry(r.Context(), e)))

		ev := e.Event()
		ev.Status = rec.status()
		ev.LatencyMS = opts.Now().Sub(start).Milliseconds()
		if err := dst.AppendAudit(context.WithoutCancel(r.Context()), ev); err != nil {
			opts.Logger.Error("audit append failed", "request_id", ev.RequestID, "err", err)
		}
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// Flush keeps streaming responses (MCP) working through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
