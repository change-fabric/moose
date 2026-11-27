package tools_test

import (
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/tools"
)

func params(t *testing.T, zone string, from, to time.Time) tools.SlotParams {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return tools.SlotParams{
		Start: from, End: to, Duration: time.Hour, Loc: loc, DayStart: 9 * 60, DayEnd: 17 * 60,
		Days:        map[time.Weekday]bool{time.Monday: true, time.Tuesday: true, time.Wednesday: true, time.Thursday: true, time.Friday: true},
		Granularity: 30 * time.Minute, Max: 5,
	}
}

func at(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.UTC) }

func busyOf(day, h1, h2 int) provider.Interval {
	return provider.Interval{Start: at(day, h1, 0), End: at(day, h2, 0)}
}

// 23 November 2026 is a Monday.

func TestFindSlotsOneFreeDayFillsTheDayInOrder(t *testing.T) {
	p := params(t, "UTC", at(25, 0, 0), at(26, 0, 0))
	p.Max = 3
	got := tools.FindSlots(nil, p)
	if len(got) != 3 || !got[0].Start.Equal(at(25, 9, 0)) || !got[1].Start.Equal(at(25, 10, 0)) || !got[2].Start.Equal(at(25, 11, 0)) {
		t.Fatalf("slots = %+v, want 09:00, 10:00, 11:00 (distinct, non-overlapping, earliest first)", got)
	}
}

func TestFindSlotsSpreadsAcrossDaysBeforeDoublingUp(t *testing.T) {
	p := params(t, "UTC", at(23, 0, 0), at(28, 0, 0)) // Monday to Friday
	p.Max = 4
	got := tools.FindSlots(nil, p)
	want := []time.Time{at(23, 9, 0), at(24, 9, 0), at(25, 9, 0), at(26, 9, 0)}
	if len(got) != 4 {
		t.Fatalf("slots = %+v", got)
	}
	for i, w := range want {
		if !got[i].Start.Equal(w) {
			t.Errorf("slot %d = %v, want %v (one per day first)", i, got[i].Start, w)
		}
	}
	p.Max = 7 // five days, then two more
	got = tools.FindSlots(nil, p)
	if len(got) != 7 {
		t.Fatalf("slots = %d, want 7", len(got))
	}
	for i := 5; i < 7; i++ {
		if !got[i].Start.Equal(at(23, 10, 0)) && !got[i].Start.Equal(at(24, 10, 0)) {
			t.Errorf("extra slot %d = %v, want the next best on the earliest days", i, got[i].Start)
		}
	}
}

func TestFindSlotsRanksASlotThatTouchesACommitmentBehindOneThatDoesNot(t *testing.T) {
	p := params(t, "UTC", at(25, 0, 0), at(26, 0, 0))
	p.Max = 2
	got := tools.FindSlots([]provider.Interval{busyOf(25, 9, 11)}, p)
	// 11:00 is back to back with the meeting; 11:30 is the best slot that is not.
	if len(got) != 2 || !got[0].Start.Equal(at(25, 11, 30)) {
		t.Fatalf("slots = %+v, want 11:30 ranked first", got)
	}
	// A day that is free only back to back still yields the slot, ranked last.
	p2 := params(t, "UTC", at(25, 0, 0), at(27, 0, 0))
	p2.Max = 2
	p2.Granularity = time.Hour
	both := tools.FindSlots([]provider.Interval{busyOf(25, 9, 10), busyOf(25, 11, 17)}, p2)
	if len(both) != 2 || !both[0].Start.Equal(at(26, 9, 0)) || !both[1].Start.Equal(at(25, 10, 0)) {
		t.Errorf("slots = %+v, want Thursday 09:00 then the tight Wednesday 10:00", both)
	}
}

func TestFindSlotsNeverOverlapsBusyTimeOrLeavesWorkingHours(t *testing.T) {
	p := params(t, "UTC", at(23, 0, 0), at(30, 0, 0))
	p.Max = 20
	p.Granularity = 15 * time.Minute
	busy := []provider.Interval{busyOf(23, 8, 12), busyOf(24, 9, 10), busyOf(24, 13, 15), busyOf(25, 16, 20), busyOf(25, 7, 9)}
	got := tools.FindSlots(busy, p)
	if len(got) == 0 {
		t.Fatal("no slots")
	}
	for i, s := range got {
		if s.End.Sub(s.Start) != time.Hour {
			t.Errorf("slot %d is %v long", i, s.End.Sub(s.Start))
		}
		for _, b := range busy {
			if s.Start.Before(b.End) && b.Start.Before(s.End) {
				t.Errorf("slot %v-%v overlaps busy %v-%v", s.Start, s.End, b.Start, b.End)
			}
		}
		if h := s.Start.Hour(); h < 9 || s.End.After(time.Date(s.Start.Year(), s.Start.Month(), s.Start.Day(), 17, 0, 0, 0, time.UTC)) {
			t.Errorf("slot %v-%v leaves the working hours", s.Start, s.End)
		}
		if wd := s.Start.Weekday(); wd == time.Saturday || wd == time.Sunday {
			t.Errorf("slot on a %s", wd)
		}
		for j := range i {
			if s.Start.Before(got[j].End) && got[j].Start.Before(s.End) {
				t.Errorf("slots %d and %d overlap", j, i)
			}
		}
	}
}

func TestFindSlotsHonoursTheWindowAndTheGranularity(t *testing.T) {
	p := params(t, "UTC", at(25, 13, 20), at(25, 16, 0))
	p.Granularity = 15 * time.Minute
	p.Max = 20
	got := tools.FindSlots(nil, p)
	if len(got) == 0 || !got[0].Start.Equal(at(25, 13, 30)) {
		t.Fatalf("slots = %+v, want the first at 13:30, the next multiple of 15 minutes after the window opens", got)
	}
	for _, s := range got {
		if s.End.After(at(25, 16, 0)) || s.Start.Minute()%15 != 0 {
			t.Errorf("slot %v-%v outside the window or off the grid", s.Start, s.End)
		}
	}
}

func TestFindSlotsNothingFreeIsNoSlots(t *testing.T) {
	p := params(t, "UTC", at(25, 0, 0), at(26, 0, 0))
	if got := tools.FindSlots([]provider.Interval{busyOf(25, 8, 18)}, p); len(got) != 0 {
		t.Errorf("a day of back-to-back meetings gave %+v", got)
	}
	if got := tools.FindSlots([]provider.Interval{busyOf(25, 9, 10), busyOf(25, 10, 16)}, p); len(got) != 1 || !got[0].Start.Equal(at(25, 16, 0)) {
		t.Errorf("touching busy intervals gave %+v, want only 16:00", got)
	}
	// Gaps shorter than the meeting give nothing.
	p.Duration = 2 * time.Hour
	if got := tools.FindSlots([]provider.Interval{busyOf(25, 10, 11), busyOf(25, 12, 13), busyOf(25, 14, 15), busyOf(25, 16, 17)}, p); len(got) != 0 {
		t.Errorf("gaps of one hour for a two hour meeting gave %+v", got)
	}
	p.Max = 0
	if got := tools.FindSlots(nil, p); got != nil {
		t.Errorf("max 0 gave %+v", got)
	}
}

func TestFindSlotsFollowsTheZoneAcrossADaylightSavingChange(t *testing.T) {
	// Berlin springs forward at 02:00 on Sunday 29 March 2026. Monday the 30th
	// 09:00 local is 07:00 UTC; Friday the 27th 09:00 local is 08:00 UTC.
	from := time.Date(2026, 3, 27, 0, 0, 0, 0, time.UTC)
	p := params(t, "Europe/Berlin", from, from.AddDate(0, 0, 5))
	p.Max = 2
	got := tools.FindSlots(nil, p)
	if len(got) != 2 {
		t.Fatalf("slots = %+v", got)
	}
	fri, mon := got[0], got[1]
	if !fri.Start.Equal(time.Date(2026, 3, 27, 8, 0, 0, 0, time.UTC)) || !mon.Start.Equal(time.Date(2026, 3, 30, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("slots = %v and %v, want 09:00 Berlin on both days (08:00Z before the change, 07:00Z after)", fri.Start, mon.Start)
	}
	if h := mon.Start.In(p.Loc).Hour(); h != 9 {
		t.Errorf("Monday slot starts at %d local", h)
	}
}

func TestFindSlotsChosenDaysAndEndOfDay(t *testing.T) {
	p := params(t, "UTC", at(23, 0, 0), at(30, 0, 0))
	p.Days = map[time.Weekday]bool{time.Saturday: true}
	p.DayStart, p.DayEnd, p.Max = 10*60, 24*60, 3
	got := tools.FindSlots(nil, p)
	if len(got) == 0 || got[0].Start.Weekday() != time.Saturday || !got[0].Start.Equal(at(28, 10, 0)) {
		t.Fatalf("slots = %+v, want Saturday only", got)
	}
	// A working day that runs to midnight allows a slot ending at midnight.
	p.Max = 20
	p.Duration = 2 * time.Hour
	p.Granularity = time.Hour
	late := tools.FindSlots([]provider.Interval{busyOf(28, 10, 22)}, p)
	if len(late) != 1 || !late[0].Start.Equal(at(28, 22, 0)) || !late[0].End.Equal(at(29, 0, 0)) {
		t.Errorf("slots = %+v, want 22:00-24:00", late)
	}
}
