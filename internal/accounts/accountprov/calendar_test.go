package accountprov_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/change-fabric/moose/internal/accounts/scopes"
	"github.com/change-fabric/moose/internal/calendar/provider"
)

// The calendar provider of a Google account reads with the bearer token the
// shared refresh produced, and one refresh serves mail and calendar together.
func TestGoogleCalendarProviderSharesTheTokenCacheWithMail(t *testing.T) {
	f := newFixture(t, 200, goodToken)
	var mu sync.Mutex
	var auth []string
	cal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"a@example.com","summary":"A","primary":true,"accessRole":"owner","timeZone":"Europe/Berlin"}]}`))
	}))
	defer cal.Close()
	f.src.GoogleCalendarBaseURL = cal.URL

	ctx := context.Background()
	mail, err := f.src.Provider(ctx, f.acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mail.ListMailboxes(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := f.src.CalendarProvider(ctx, f.acct)
	if err != nil {
		t.Fatal(err)
	}
	cals, err := p.ListCalendars(ctx)
	if err != nil || len(cals) != 1 || !cals[0].Primary || cals[0].TimeZone != "Europe/Berlin" {
		t.Fatalf("calendars = %+v, %v", cals, err)
	}
	if n := f.refreshN.Load(); n != 1 {
		t.Errorf("token endpoint called %d times for mail and calendar, want once", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 1 || auth[0] != "Bearer at-1" {
		t.Errorf("calendar API saw %q", auth)
	}
	if caps := p.Capabilities(); !caps.FreeBusyOthers || !caps.Video || !caps.Recurrence {
		t.Errorf("capabilities = %+v", caps)
	}
}

// A Microsoft account's calendar provider knows whether it is personal from the
// tenant id in the stored identity, and reads with the refreshed token.
func TestMicrosoftCalendarProviderKnowsPersonalAccounts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid      string
		personal bool
	}{
		{"personal", scopes.MicrosoftPersonalTenant + ".oid1", true},
		{"work", "11111111-aaaa-bbbb-cccc-222222222222.oid2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMSFixture(t, grants(split(mailScopes), split(calScope)))
			f.ms.consent(mailScopes, calScope, baseScopes)
			f.acct.ProviderUID = tc.uid
			var auth, path string
			graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth, path = r.Header.Get("Authorization"), r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"value":[{"id":"AAcal=","name":"Calendar","isDefaultCalendar":true,"canEdit":true}]}`))
			}))
			defer graphSrv.Close()
			f.src.GraphBaseURL = graphSrv.URL

			p, err := f.src.CalendarProvider(context.Background(), f.acct)
			if err != nil {
				t.Fatal(err)
			}
			caps := p.Capabilities()
			if caps.FreeBusyOthers == tc.personal || caps.Video == tc.personal {
				t.Fatalf("capabilities = %+v for a personal=%v account", caps, tc.personal)
			}
			cals, err := p.ListCalendars(context.Background())
			if err != nil || len(cals) != 1 || !cals[0].Primary || cals[0].Role != provider.RoleOwner {
				t.Fatalf("calendars = %+v, %v", cals, err)
			}
			if !strings.HasPrefix(auth, "Bearer at-") || path != "/me/calendars" {
				t.Errorf("Graph saw %q on %q", auth, path)
			}
			// The refresh carried the calendar scope, so the token covers the calendar.
			f.ms.mu.Lock()
			defer f.ms.mu.Unlock()
			if len(f.ms.requests) == 0 || !strings.Contains(f.ms.requests[0].scope, calScope) {
				t.Errorf("refresh requests = %+v, want the calendar scope included", f.ms.requests)
			}
		})
	}
}

func TestCalendarProviderRefusesAnUnknownProvider(t *testing.T) {
	f := newFixture(t, 200, goodToken)
	f.acct.Provider = "yahoo"
	if _, err := f.src.CalendarProvider(context.Background(), f.acct); err == nil {
		t.Error("an unsupported provider was accepted")
	}
}
