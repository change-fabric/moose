package fake_test

import (
	"fmt"
	"testing"

	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/provider/providertest"
)

// seeder arranges a fake.Provider for the conformance suites.
type seeder struct{ p *fake.Provider }

func (s seeder) SeedCalendar(t *testing.T, c provider.Calendar) { s.p.AddCalendar(c) }

func (s seeder) SeedEvent(t *testing.T, calendarID string, ev provider.Event) provider.Event {
	return s.p.AddEvent(calendarID, ev)
}

func (s seeder) SeedBusy(t *testing.T, email string, in provider.Interval) { s.p.AddBusy(email, in) }

func (s seeder) ExternalAddAttendee(t *testing.T, ref provider.EventRef, a provider.Attendee) {
	s.p.ExternalAddAttendee(ref, a)
}

func (s seeder) Fail(t *testing.T, f providertest.Failure) {
	t.Helper()
	switch f {
	case providertest.FailNone:
		s.p.Err = nil
	case providertest.FailAuth:
		s.p.Err = fmt.Errorf("%w: fake credential revoked", provider.ErrAuth)
	case providertest.FailRateLimit:
		s.p.Err = &provider.RateLimitError{RetryAfter: providertest.RetryAfter}
	default:
		t.Fatalf("unknown failure %d", f)
	}
}

func (s seeder) Notified(t *testing.T) []providertest.Notification {
	var out []providertest.Notification
	for _, n := range s.p.Notifications() {
		out = append(out, providertest.Notification{Kind: n.Kind, To: n.To})
	}
	return out
}

func factory(caps provider.Capabilities) providertest.Factory {
	return func(t *testing.T) (provider.Provider, providertest.Seeder) {
		p := fake.New(providertest.SelfAddress)
		p.SetCapabilities(caps)
		return p, seeder{p}
	}
}

var (
	workAccount     = provider.Capabilities{FreeBusyOthers: true, Video: true, Recurrence: true}
	personalAccount = provider.Capabilities{FreeBusyOthers: false, Video: false, Recurrence: true}
)

func TestConformanceFullAccount(t *testing.T) {
	t.Run("Read", func(t *testing.T) { providertest.RunRead(t, factory(workAccount)) })
	t.Run("Write", func(t *testing.T) { providertest.RunWrite(t, factory(workAccount)) })
}

// A personal Microsoft account cannot see other people's free/busy and has no
// Teams: the same suites pass with those capabilities off, expecting
// ErrUnsupportedHere.
func TestConformanceLimitedAccount(t *testing.T) {
	t.Run("Read", func(t *testing.T) { providertest.RunRead(t, factory(personalAccount)) })
	t.Run("Write", func(t *testing.T) { providertest.RunWrite(t, factory(personalAccount)) })
}
