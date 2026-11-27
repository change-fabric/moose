# Microsoft mail acceptance run

Status: NOT RUN. The owner has not authorized the acceptance run and the Entra
app is not registered. Production enablement of `email.microsoft.enabled`
depends on this file showing every step passed. Until then it stays false.

Run against isolated test mailboxes (one work or school Microsoft tenant, one
Outlook.com, one Google as control), in a non-prod stage or locally with
`make up`. The owner sets `email.microsoft.enabled=true` and
`email.send.live=true` for the run only, then restores `email.send.live=false`.

Record one block per run: date, accounts (redacted), and the result of each step.

| Step | Expected | Work/school | Outlook.com | Google (control) |
| --- | --- | --- | --- | --- |
| 1. Enable flags for the run | `enabled=true`, `live=true` | not run | not run | not run |
| 2. Connect; `email_search`, `email_get_thread`, `email_get_attachment` | each returns data | not run | not run | not run |
| 3. `email_create_draft`, `email_update_draft` (id unchanged), `email_request_send` to a second test mailbox, passkey approval | ledger claimed, dispatching, submitted, sent; recipient copy has the approved From, recipients, subject and body | not run | not run | not run |
| 4. Send with send disabled | rejected with `send_not_enabled` | not run | not run | not run |
| 5. Restore `email.send.live=false` | flag restored | not run | not run | not run |

The `send-correlation` row in `docs/microsoft-graph-spike.md` must also be
VERIFIED on both account types before any of this is enabled in production.
