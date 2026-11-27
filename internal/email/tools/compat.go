package tools

import (
	"context"

	"github.com/change-fabric/moose/internal/accounts/fanout"
)

// retry429 retries fn on a provider rate limit using the configured sleep.
func (d Deps) retry429(ctx context.Context, fn func() error) error {
	return fanout.Retry429(ctx, d.Sleep, fn)
}

func (d Deps) sealCursor(c fanout.CursorBody) string {
	return fanout.SealCursor(d.CursorKey, c)
}

func (d Deps) openCursor(s, owner, query string) (fanout.CursorBody, error) {
	return fanout.OpenCursor(d.CursorKey, s, owner, query)
}
