package scopes_test

import (
	"reflect"
	"testing"

	"github.com/change-fabric/moose/internal/accounts/scopes"
)

const (
	workUID     = "11111111-aaaa-bbbb-cccc-222222222222.33333333-dddd-eeee-ffff-444444444444"
	personalUID = scopes.MicrosoftPersonalTenant + ".33333333-dddd-eeee-ffff-444444444444"
)

func TestRequired(t *testing.T) {
	for name, tc := range map[string]struct {
		provider, service, uid string
		want                   []string
	}{
		"google calendar":         {scopes.Google, scopes.Calendar, "", []string{scopes.GoogleCalendarEvents, scopes.GoogleCalendarReadonly}},
		"microsoft work calendar": {scopes.Microsoft, scopes.Calendar, workUID, []string{"Calendars.ReadWrite", "Calendars.Read.Shared"}},
		"microsoft personal":      {scopes.Microsoft, scopes.Calendar, personalUID, []string{"Calendars.ReadWrite"}},
		"mail is not listed":      {scopes.Google, scopes.Mail, "", nil},
		"unknown provider":        {"yahoo", scopes.Calendar, "", nil},
	} {
		if got := scopes.Required(tc.provider, tc.service, tc.uid); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Required = %v, want %v", name, got, tc.want)
		}
	}
}

func TestIsPersonalMicrosoft(t *testing.T) {
	if !scopes.IsPersonalMicrosoft(personalUID) || !scopes.IsPersonalMicrosoft("9188040D-6C67-4C5B-B112-36A304B66DAD.abc") {
		t.Error("the consumer tenant is personal")
	}
	if scopes.IsPersonalMicrosoft(workUID) || scopes.IsPersonalMicrosoft("") {
		t.Error("a work tenant or an empty uid is not personal")
	}
}

func TestMissingAndOf(t *testing.T) {
	granted := []string{"https://graph.microsoft.com/Mail.ReadWrite", "https://graph.microsoft.com/Calendars.ReadWrite", "openid"}
	if got := scopes.Missing(scopes.Microsoft, scopes.Calendar, workUID, granted); !reflect.DeepEqual(got, []string{"Calendars.Read.Shared"}) {
		t.Errorf("Missing = %v", got)
	}
	if got := scopes.Missing(scopes.Microsoft, scopes.Calendar, personalUID, granted); got != nil {
		t.Errorf("a personal account holding ReadWrite is missing %v", got)
	}
	if got := scopes.Missing(scopes.Google, scopes.Calendar, "", nil); !reflect.DeepEqual(got, []string{"calendar.events", "calendar.readonly"}) {
		t.Errorf("nothing granted: Missing = %v", got)
	}
	g := []string{scopes.GoogleCalendarReadonly, "https://www.googleapis.com/auth/gmail.readonly"}
	if got := scopes.Of(scopes.Google, scopes.Calendar, g); !reflect.DeepEqual(got, []string{scopes.GoogleCalendarReadonly}) {
		t.Errorf("Of calendar = %v", got)
	}
	if got := scopes.Of(scopes.Google, scopes.Mail, g); !reflect.DeepEqual(got, []string{"https://www.googleapis.com/auth/gmail.readonly"}) {
		t.Errorf("Of mail = %v", got)
	}
	if got := scopes.Of(scopes.Microsoft, scopes.Mail, granted); !reflect.DeepEqual(got, []string{"https://graph.microsoft.com/Mail.ReadWrite"}) {
		t.Errorf("Of microsoft mail = %v", got)
	}
}

func TestUnionIgnoresThePrefixAndCase(t *testing.T) {
	got := scopes.Union([]string{"https://graph.microsoft.com/Mail.ReadWrite", "openid"}, []string{"mail.readwrite", "User.Read", "openid"})
	want := []string{"https://graph.microsoft.com/Mail.ReadWrite", "openid", "User.Read"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Union = %v, want %v", got, want)
	}
}
