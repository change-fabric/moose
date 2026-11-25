package attach_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/attach"
	"github.com/change-fabric/moose/internal/email/attach/attachtest"
)

func TestSweepDeletesOnlyObjectsOlderThanOneHour(t *testing.T) {
	st := attachtest.NewMem(func() time.Time { return t0 })
	age := func(d time.Duration) time.Time { return t0.Add(-d) }
	st.Seed("att/fresh", []byte("a"), age(5*time.Minute))
	st.Seed("att/just-under", []byte("b"), age(59*time.Minute+59*time.Second))
	st.Seed("att/exactly-an-hour", []byte("c"), age(time.Hour))
	st.Seed("att/just-over", []byte("d"), age(time.Hour+time.Second))
	st.Seed("att/old", []byte("e"), age(3*time.Hour))
	// Not under att/: the sweeper must never list or touch it.
	st.Seed("other/ancient", []byte("f"), age(100*time.Hour))
	st.Seed("attic/ancient", []byte("g"), age(100*time.Hour))

	rep, err := attach.Sweep(context.Background(), st, t0, attach.MaxAge)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.Keys(), " "); got != "att/exactly-an-hour att/fresh att/just-under attic/ancient other/ancient" {
		t.Fatalf("left behind: %s", got)
	}
	if got := strings.Join(st.Deleted(), " "); got != "att/just-over att/old" {
		t.Fatalf("deleted: %s", got)
	}
	if rep.Listed != 5 || rep.Deleted != 2 || rep.Remaining != 3 || rep.OldestRemaining != time.Hour || !rep.Surveyed {
		t.Fatalf("report %+v", rep)
	}
}

func TestSweepReportsObjectsItCouldNotDelete(t *testing.T) {
	st := attachtest.NewMem(func() time.Time { return t0 })
	st.Seed("att/stuck", []byte("a"), t0.Add(-3*time.Hour))
	st.Seed("att/ok", []byte("b"), t0.Add(-2*time.Hour))
	st.DeleteErr["att/stuck"] = errors.New("access denied")

	rep, err := attach.Sweep(context.Background(), st, t0, attach.MaxAge)
	if err == nil || !strings.Contains(err.Error(), "att/stuck") {
		t.Fatalf("err = %v, want the failed key named", err)
	}
	// The object that could not be deleted still counts toward the age alarm.
	if rep.Deleted != 1 || rep.Remaining != 1 || rep.OldestRemaining != 3*time.Hour {
		t.Fatalf("report %+v", rep)
	}
}

func TestSweepOnAnEmptyBucket(t *testing.T) {
	st := attachtest.NewMem(func() time.Time { return t0 })
	rep, err := attach.Sweep(context.Background(), st, t0, attach.MaxAge)
	if err != nil || rep.OldestRemaining != 0 || !rep.Surveyed {
		t.Fatalf("%+v, %v", rep, err)
	}
}

type listFails struct{ attach.Store }

func (listFails) List(context.Context, string, func(attach.Object) error) error {
	return errors.New("throttled")
}

func TestSweepListFailureIsNotSurveyed(t *testing.T) {
	rep, err := attach.Sweep(context.Background(), listFails{attachtest.NewMem(time.Now)}, t0, attach.MaxAge)
	if err == nil || rep.Surveyed {
		t.Fatalf("%+v, %v: a failed listing must not publish an age", rep, err)
	}
}

func TestMetricLineIsEmbeddedMetricFormat(t *testing.T) {
	line := attach.MetricLine(attach.SweepReport{Deleted: 2, Remaining: 1, OldestRemaining: 90 * time.Minute}, t0)
	var m struct {
		AWS struct {
			Timestamp         int64 `json:"Timestamp"`
			CloudWatchMetrics []struct {
				Namespace  string     `json:"Namespace"`
				Dimensions [][]string `json:"Dimensions"`
				Metrics    []struct{ Name, Unit string }
			} `json:"CloudWatchMetrics"`
		} `json:"_aws"`
		Oldest int `json:"OldestAttachmentAgeSeconds"`
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	cw := m.AWS.CloudWatchMetrics
	if m.Oldest != 5400 || m.AWS.Timestamp != t0.UnixMilli() || len(cw) != 1 || cw[0].Namespace != "Moose/Attachments" || cw[0].Metrics[0].Name != "OldestAttachmentAgeSeconds" {
		t.Fatalf("metric line %s", line)
	}
	if strings.Contains(line, "\n") {
		t.Fatal("an EMF record must be one line")
	}
}
