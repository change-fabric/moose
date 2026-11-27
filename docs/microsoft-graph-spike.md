# Microsoft Graph spike findings

Status: the Phase 2 spike has not run. The owner has not registered the Entra
app yet, so the Microsoft code is being built ahead against documented Graph
behaviour and synthetic fixtures (`internal/email/graph/testdata/synthetic/`).
Every row below is UNVERIFIED until the spike probes run against a work or
school mailbox and an outlook.com mailbox and replace the "assumed" column with
a recorded result. All Microsoft paths stay behind `email.microsoft.enabled=false`
until then.

When a row is settled, record the work/school result, the MSA result and the
decision it settles, commit the redacted fixture under
`internal/email/graph/testdata/spike/`, and change the status to VERIFIED.

## Findings

| Row | Probe | Status | What the code assumes | Where |
| --- | --- | --- | --- | --- |
| `search-filter` | `$search` plus `$filter` in one `/messages` request, and unread plus text | UNVERIFIED | The two cannot be combined, so text terms compile to KQL `$search` (from, to, subject, date bounds and attachment flag as KQL properties) and everything else to `$filter`. Unread plus text is refused with `ErrInvalidQuery` (decision 13 allows a post-filter instead; a post-filter would return short pages, so the refusal is the default until the spike shows which is workable). | `internal/email/graph/query.go` |
| `inefficient-filter` | `$filter isRead eq false` with `$orderby receivedDateTime desc`, and `conversationId` filter with `$orderby` | UNVERIFIED | Exchange rejects a filter whose sort property is not first (`InefficientFilter`). Every filter query opens with `receivedDateTime ge <after or epoch>`. Thread lookup filters on `conversationId` with no `$orderby` and sorts client side. A residual `InefficientFilter` maps to `ErrInvalidQuery`. | `query.go`, `GetThread` in `client.go`, `internal/graphapi` |
| `immutable-id` | `Prefer: IdType="ImmutableId"` on MSA; ids stable across a move to deleteditems | UNVERIFIED | Supported on both account types, so every request carries the header and ids stored in approvals and ledgers survive moves. | `internal/graphapi` |
| `kql-quoting` | Phrase quoting inside `$search="..."` | UNVERIFIED | A cleaned phrase is wrapped in inner double quotes, escaped as `\"` inside the outer quotes `$search` requires. Dates use `received>=YYYY-MM-DD` (day granularity). | `internal/email/graph/client.go` (`Search`) |
| `wellknown-folders` | Resolving system folders | UNVERIFIED | `GET /me/mailFolders/{inbox,sentitems,drafts,deleteditems,junkemail}` returns the folder id, used to map `parentFolderId` back to the neutral INBOX, SENT, DRAFT, TRASH, SPAM ids. v1.0 `mailFolder` has no `wellKnownName` field, so aliases are resolved by request. | `systemIDs` in `client.go` |
| `paging` | `@odata.nextLink` shape | UNVERIFIED | Carries `$skip` (with `$filter`) or `$skiptoken` (with `$search`). Only those parameters are lifted into the opaque cursor; the host and the rest of the query are never taken from the answer. | `pagingParams` in `client.go` |
| `message-references` | `internetMessageHeaders` on a single-message GET | UNVERIFIED | `$select=internetMessageHeaders` returns `References` for reply threading, so `GetMessage` needs no `/$value` call. `RawMessage` (`/$value`) exists for the draft and send phases and is size capped. | `GetMessage`, `RawMessage` |
| `attachment-value` | `/attachments/{id}/$value` | UNVERIFIED | Streams the bytes of a `fileAttachment`. A `referenceAttachment` has no content and is rejected. Item attachments are not special-cased. | `OpenAttachment` |

## Rows other phases depend on (not touched here)

| Row | Status | Needed by |
| --- | --- | --- |
| `send-correlation` | UNVERIFIED, not yet coded | Phase 7 does not start unless it passes on both account types |
| `calendar-preconditions` | UNVERIFIED, not yet coded | Phases 11 and 12 |
| `draft-value-headers` | UNVERIFIED, not yet coded | Phase 7 |
| `msa-address` | UNVERIFIED, not yet coded | Phase 6 |
