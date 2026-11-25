package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/attach"
	"github.com/change-fabric/moose/internal/email/attach/attachtest"
)

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunDeletesOldObjectsAndPublishesTheMetric(t *testing.T) {
	st := attachtest.NewMem(func() time.Time { return now })
	st.Seed("att/old", []byte("x"), now.Add(-2*time.Hour))
	st.Seed("att/new", []byte("y"), now.Add(-10*time.Minute))
	var err error
	out := captureStdout(t, func() { err = run(context.Background(), quiet(), st, func() time.Time { return now }) })
	if err != nil {
		t.Fatal(err)
	}
	if keys := strings.Join(st.Keys(), " "); keys != "att/new" {
		t.Fatalf("keys = %s", keys)
	}
	if !strings.Contains(out, `"OldestAttachmentAgeSeconds":600`) || !strings.Contains(out, `"Namespace":"Moose/Attachments"`) {
		t.Fatalf("metric line missing or wrong: %q", out)
	}
}

func TestRunFailsWhenADeleteFailsSoLambdaRetriesAndDeadLetters(t *testing.T) {
	st := attachtest.NewMem(func() time.Time { return now })
	st.Seed("att/stuck", []byte("x"), now.Add(-3*time.Hour))
	st.DeleteErr["att/stuck"] = errors.New("denied")
	var err error
	out := captureStdout(t, func() { err = run(context.Background(), quiet(), st, func() time.Time { return now }) })
	if err == nil {
		t.Fatal("want the failure returned")
	}
	// The stuck object still reaches the alarm as a 3 hour old object.
	if !strings.Contains(out, `"OldestAttachmentAgeSeconds":10800`) {
		t.Fatalf("metric %q", out)
	}
}

type brokenList struct{ attach.Store }

func (brokenList) List(context.Context, string, func(attach.Object) error) error {
	return errors.New("throttled")
}

func TestRunPublishesNoMetricWhenListingFails(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = run(context.Background(), quiet(), brokenList{attachtest.NewMem(time.Now)}, func() time.Time { return now })
	})
	if err == nil || strings.Contains(out, "OldestAttachmentAgeSeconds") {
		t.Fatalf("err=%v out=%q: a failed listing must not publish an age", err, out)
	}
}
