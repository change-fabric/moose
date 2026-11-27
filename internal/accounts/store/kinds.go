package store

// Approval kinds (the approver's payload kinds, Phase 9).
const (
	KindAccountSendEnable = "account-send-enable"
	KindEmailSend         = "email-send"
	KindEmailSendBatch    = "email-send-batch"
	KindContactTrustBatch = "contact-trust-batch"
	KindDashboardSession  = "dashboard-session"
	// KindCalendarInvite is one create, update or cancel of a calendar event
	// that notifies someone other than the account. KindAccountInviteEnable
	// turns the per-account invite opt-in on.
	KindCalendarInvite      = "calendar-invite"
	KindAccountInviteEnable = "account-invite-enable"
)
