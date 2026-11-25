package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// MaxAge is how long an attachment object lives. The sweeper deletes
	// anything older; the bucket's one-day lifecycle rule is only the backstop.
	MaxAge = time.Hour
	// MetricNamespace and MetricOldestAge name the metric the 2-hour alarm watches.
	MetricNamespace = "Moose/Attachments"
	MetricOldestAge = "OldestAttachmentAgeSeconds"
	// maxSweep bounds one run so a flood of objects cannot outlast the Lambda.
	maxSweep = 20000
)

var errSweepFull = errors.New("sweep full")

// SweepReport says what one sweep did.
type SweepReport struct {
	Listed int
	// Surveyed is true when the listing finished, so Remaining and
	// OldestRemaining describe the bucket and are safe to publish.
	Surveyed bool
	Deleted  int
	// Remaining counts objects still in the store after the sweep (younger
	// than maxAge, or whose delete failed).
	Remaining int
	// OldestRemaining is the age of the oldest remaining object, 0 if none.
	OldestRemaining time.Duration
}

// Sweep deletes every object under Prefix that is older than maxAge at now.
// An object exactly maxAge old stays. Failed deletes are reported in the error
// and counted in Remaining, so the age metric still sees them. It deletes only
// under Prefix and never anything else in the bucket.
func Sweep(ctx context.Context, st Store, now time.Time, maxAge time.Duration) (SweepReport, error) {
	var rep SweepReport
	var stale []Object
	var fresh []Object
	err := st.List(ctx, Prefix, func(o Object) error {
		if rep.Listed >= maxSweep {
			return errSweepFull
		}
		rep.Listed++
		if now.Sub(o.Modified) > maxAge {
			stale = append(stale, o)
		} else {
			fresh = append(fresh, o)
		}
		return nil
	})
	var errs []error
	switch {
	case errors.Is(err, errSweepFull):
		// Delete what was listed; the next run takes the rest.
		errs = append(errs, fmt.Errorf("attach: sweep: more than %d objects, the rest wait for the next run", maxSweep))
	case err != nil:
		return rep, fmt.Errorf("attach: sweep: list: %w", err)
	}
	rep.Surveyed = true
	remaining := fresh
	for _, o := range stale {
		if derr := st.Delete(ctx, o.Key); derr != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", o.Key, derr))
			remaining = append(remaining, o)
			continue
		}
		rep.Deleted++
	}
	rep.Remaining = len(remaining)
	for _, o := range remaining {
		rep.OldestRemaining = max(rep.OldestRemaining, now.Sub(o.Modified))
	}
	return rep, errors.Join(errs...)
}

// MetricLine renders the report as one CloudWatch Embedded Metric Format log
// line, which CloudWatch turns into the OldestAttachmentAgeSeconds metric with
// no PutMetricData call.
func MetricLine(rep SweepReport, now time.Time) string {
	m := map[string]any{
		"_aws": map[string]any{
			"Timestamp": now.UnixMilli(),
			"CloudWatchMetrics": []map[string]any{{
				"Namespace":  MetricNamespace,
				"Dimensions": [][]string{{}},
				"Metrics": []map[string]string{
					{"Name": MetricOldestAge, "Unit": "Seconds"},
					{"Name": "AttachmentsDeleted", "Unit": "Count"},
					{"Name": "AttachmentsRemaining", "Unit": "Count"},
				},
			}},
		},
		MetricOldestAge:        int64(rep.OldestRemaining / time.Second),
		"AttachmentsDeleted":   rep.Deleted,
		"AttachmentsRemaining": rep.Remaining,
	}
	b, _ := json.Marshal(m)
	return string(b)
}
