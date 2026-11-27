# Calendar acceptance run

Status: NOT RUN. The owner has not applied the calendar Terraform, has not
authorized the acceptance run, and the Entra app is not registered. Tag
`calendar/v0.1.0` and enabling calendar in production depend on this file
showing every step passed. Until then `email.calendar.enabled` and
`email.calendar.invite.live` stay false.

Run against isolated test calendars (one work or school Microsoft tenant, one
Outlook.com, one Google), in a non-prod stage (`deploy/terraform/email` with
`env/dev.tfvars`) or locally with `make up`. The owner sets
`email.calendar.enabled=true` and `email.calendar.invite.live=true` for the run
only, then restores `email.calendar.invite.live=false`.

Record one block per run: date, accounts (redacted), and the result of each step.

| Step | Expected | Work/school | Outlook.com | Google |
| --- | --- | --- | --- | --- |
| 1. Grant calendar per account | Services records the granted scopes; mail still works | not run | not run | not run |
| 2. List calendars, events, free/busy, find times | each returns data; personal Microsoft reports `own_calendar_only` | not run | not run | not run |
| 3. Self-only create, update, delete | apply directly | not run | not run | not run |
| 4. Attendee create with invites disabled | rejected `invites_not_enabled` | not run | not run | not run |
| 5. Enable invites by passkey, set `email.calendar.invite.live=true` | switch on, flag on for the run | not run | not run | not run |
| 6. Attendee create, confirmed by passkey | ledger claimed, dispatching, applied; attendee inbox has the notification | not run | not run | not run |
| 7. Update removing one attendee, confirmed | ledger claimed, dispatching, applied; the removed attendee also notified | not run | not run | not run |
| 8. Cancel, confirmed | ledger claimed, dispatching, applied; attendees notified | not run | not run | not run |
| 9. Title and description each attendee received | match what the preview showed | not run | not run | not run |
| 10. RSVP from an attendee account | reaches the organizer | not run | not run | not run |
| 11. Restore `email.calendar.invite.live=false` | flag restored | not run | not run | not run |

The `calendar-preconditions` row in `docs/microsoft-graph-spike.md` must also be
VERIFIED on both Microsoft account types before any of this is enabled in
production.
