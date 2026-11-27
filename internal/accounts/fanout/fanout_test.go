package fanout_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/email/provider"
)

type sleeps struct{ got []time.Duration }

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

func TestRetry429HonoursRetryAfterWithinBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var s sleeps
	calls := 0
	err := fanout.Retry429(ctx, s.sleep, func() error {
		calls++
		if calls == 1 {
			return &provider.RateLimitError{RetryAfter: 2 * time.Second}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want success on the second call", err, calls)
	}
	if len(s.got) != 1 || s.got[0] != 2*time.Second {
		t.Fatalf("slept %v, want exactly the Retry-After of 2s", s.got)
	}
}

func TestRetry429GivesUpWhenRetryAfterExceedsBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var s sleeps
	calls := 0
	err := fanout.Retry429(ctx, s.sleep, func() error {
		calls++
		return fmt.Errorf("wrapped: %w", &provider.RateLimitError{RetryAfter: 30 * time.Second})
	})
	if calls != 1 || len(s.got) != 0 {
		t.Fatalf("calls=%d sleeps=%v, want one call and no sleep", calls, s.got)
	}
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	status, reason, keep := fanout.Classify(err)
	if status != fanout.StatusError || reason != "rate_limited" || !keep {
		t.Fatalf("classify = %s %s %v, want error rate_limited keep", status, reason, keep)
	}
}

func TestRetry429FallsBackToBackoffWithoutHint(t *testing.T) {
	var s sleeps
	calls := 0
	err := fanout.Retry429(context.Background(), s.sleep, func() error {
		calls++
		return provider.ErrRateLimited
	})
	if !errors.Is(err, provider.ErrRateLimited) || calls != 3 {
		t.Fatalf("err=%v calls=%d, want 3 attempts", err, calls)
	}
	if len(s.got) != 2 || s.got[0] != 200*time.Millisecond || s.got[1] != 400*time.Millisecond {
		t.Fatalf("slept %v, want 200ms then 400ms", s.got)
	}
}

func TestRetry429PassesOtherErrorsThrough(t *testing.T) {
	var s sleeps
	want := provider.ErrAuth
	if err := fanout.Retry429(context.Background(), s.sleep, func() error { return want }); err != want || len(s.got) != 0 {
		t.Fatalf("err=%v sleeps=%v", err, s.got)
	}
}

func TestRateLimitErrorWrapsSentinel(t *testing.T) {
	e := &provider.RateLimitError{RetryAfter: time.Second}
	if !errors.Is(e, provider.ErrRateLimited) {
		t.Fatal("RateLimitError must wrap ErrRateLimited")
	}
}
