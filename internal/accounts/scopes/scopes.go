// Package scopes names the provider permissions each service (mail, calendar)
// needs and tells which of them an account actually holds. The connect page
// asks for them and the tools report what is missing, from this one list.
package scopes

import "strings"

// Provider names, equal to store.ProviderGoogle and store.ProviderMicrosoft.
const (
	Google    = "google"
	Microsoft = "microsoft"
)

// Services, equal to store.ServiceMail and store.ServiceCalendar.
const (
	Mail     = "mail"
	Calendar = "calendar"
)

// Calendar scopes.
const (
	GoogleCalendarEvents   = "https://www.googleapis.com/auth/calendar.events"
	GoogleCalendarReadonly = "https://www.googleapis.com/auth/calendar.readonly"
	// MSCalendarsReadWrite reads and writes the signed-in user's calendars.
	MSCalendarsReadWrite = "Calendars.ReadWrite"
	// MSCalendarsReadShared reads calendars other people shared with the user,
	// which is what free/busy of others needs. Work and school accounts only.
	MSCalendarsReadShared = "Calendars.Read.Shared"
)

// MicrosoftPersonalTenant is the tid of every personal Microsoft account
// (Outlook.com, Hotmail, Live).
const MicrosoftPersonalTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

const (
	googlePrefix = "https://www.googleapis.com/auth/"
	graphPrefix  = "https://graph.microsoft.com/"
)

// IsPersonalMicrosoft reports whether a Microsoft ProviderUID ("tid.oid") is a
// personal account.
func IsPersonalMicrosoft(providerUID string) bool {
	tid, _, _ := strings.Cut(strings.ToLower(providerUID), ".")
	return tid == MicrosoftPersonalTenant
}

// Required lists the permissions a service needs from a provider, in the form
// the provider's consent request names them. providerUID matters only for
// Microsoft calendar, where a personal account cannot hold the shared-calendar
// scope. An unknown provider or service has none.
func Required(provider, service, providerUID string) []string {
	switch {
	case provider == Google && service == Calendar:
		return []string{GoogleCalendarEvents, GoogleCalendarReadonly}
	case provider == Microsoft && service == Calendar:
		if IsPersonalMicrosoft(providerUID) {
			return []string{MSCalendarsReadWrite}
		}
		return []string{MSCalendarsReadWrite, MSCalendarsReadShared}
	}
	return nil
}

// Short is a scope without its provider prefix: "calendar.events",
// "Calendars.ReadWrite".
func Short(scope string) string {
	scope = strings.TrimPrefix(scope, googlePrefix)
	return strings.TrimPrefix(scope, graphPrefix)
}

func key(scope string) string { return strings.ToLower(Short(scope)) }

// Owns reports whether scope belongs to the service on that provider.
func Owns(provider, service, scope string) bool {
	k := key(scope)
	switch {
	case provider == Google && service == Calendar:
		return strings.HasPrefix(k, "calendar")
	case provider == Microsoft && service == Calendar:
		return strings.HasPrefix(k, "calendars.")
	case provider == Microsoft && service == Mail:
		return strings.HasPrefix(k, "mail.")
	case provider == Google && service == Mail:
		return strings.HasPrefix(k, "gmail.")
	}
	return false
}

// Of returns the scopes in granted that belong to the service.
func Of(provider, service string, granted []string) []string {
	var out []string
	for _, sc := range granted {
		if Owns(provider, service, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// Missing returns the short names of the required scopes absent from granted.
// A nil granted list means nothing is held, so every required scope is missing.
func Missing(provider, service, providerUID string, granted []string) []string {
	have := map[string]bool{}
	for _, sc := range granted {
		have[key(sc)] = true
	}
	var out []string
	for _, want := range Required(provider, service, providerUID) {
		if !have[key(want)] {
			out = append(out, Short(want))
		}
	}
	return out
}

// Union returns a and b together, without duplicates, in first-seen order.
func Union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, sc := range list {
			if k := key(sc); !seen[k] {
				seen[k] = true
				out = append(out, sc)
			}
		}
	}
	return out
}
