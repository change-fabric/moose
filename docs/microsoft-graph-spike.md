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


## Phase 6 rows (connect and provisioning)

| Row | Probe | Status | What the code assumes | Where |
| --- | --- | --- | --- | --- |
| `msa-address` | `GET /me?$select=mail,userPrincipalName` for an outlook.com account, and the id_token claims for both account types | UNVERIFIED | `mail` can be null on a personal account, so the address is `mail`, falling back to `userPrincipalName`; no further fallback, a profile with neither is refused. The account identity is `tid.oid` from the id_token, which is present in a v2 token (`requestedAccessTokenVersion: 2`) for work, school and personal accounts alike. | `Profile` in `internal/accounts/connect/microsoft.go` |
| `id-token-unsigned-read` | id_token returned by the /common token endpoint | UNVERIFIED | The id_token arrives straight from Entra over TLS in the code exchange, so its claims are read without checking the signature (OIDC Core 3.1.3.7); only `aud` (this client), `tid` and `oid` are enforced. | `parseIDToken` |
| `scope-report` | `scope` in the token response, with and without the Graph prefix | UNVERIFIED | Entra reports granted scopes space separated, sometimes as `https://graph.microsoft.com/Mail.ReadWrite`; both spellings are accepted, compared case-insensitively. Mail.ReadWrite, Mail.Send and User.Read must all be present or nothing is saved. An omitted `scope` is taken as the full request. | `MissingScopes`, `Grant` |
| `consent-error-codes` | `error` and `error_description` on the callback when a tenant blocks user consent | UNVERIFIED | AADSTS65001 and AADSTS90094 (or `error=consent_required`) mean an administrator must approve; any other `access_denied` means the user declined. The code is read from `error_description`. | `CallbackError` |
| `admin-consent-link` | `https://login.microsoftonline.com/organizations/v2.0/adminconsent?client_id=...&scope=https://graph.microsoft.com/.default&redirect_uri=...` | UNVERIFIED | `organizations` is a valid generic tenant for an administrator whose tenant is not yet known, the redirect URI is the registered `/connect/microsoft/callback`, and the answer returns `admin_consent=True` with no `state`, which the callback acknowledges without a session. | `AdminConsentURL`, `adminConsentAnswer` |
| `prompt-select-account` | `prompt=select_account` with `/common` on first connect, `login_hint` on reconnect | UNVERIFIED | The picker lets the owner choose a work or a personal sign-in; `login_hint` steers a reconnect to the mailbox it expects. No `prompt=consent` is sent, so a returning user is not re-asked. | `AuthOptions` |

## Phase 7 rows (drafts and semantic approved send)

Phase 7 is built ahead of the spike. `send-correlation` is the gate row for it: the code stays behind `email.microsoft.enabled=false` and `email.send.live=false`, and it must not be enabled until `send-correlation` has passed on both a work or school mailbox and an outlook.com mailbox.

| Row | Probe | Status | What the code assumes | Where |
| --- | --- | --- | --- | --- |
| `send-correlation` | Create a staging draft, record its ImmutableId and `internetMessageId`, `POST /me/messages/{id}/send`, poll `GET /me/messages/{ImmutableId}` until `parentFolderId` is Sent Items; repeat with two identical messages back to back | UNVERIFIED | `POST .../send` answers 202 and delivers asynchronously. The draft is moved, not copied: the Sent Items message keeps the staging draft's ImmutableId and its `internetMessageId`. Until it moves, a GET by that id shows a draft (`isDraft` true, drafts folder), which is never evidence of failure. Two identical messages sent back to back each keep their own id, so they correlate independently. Nothing is matched by subject, recipients or time. If the Sent copy ever carries a different `internetMessageId` than the staging draft, the send is recorded as `sent_mismatch`. | `LookupSent`, `StageDraft`, `IssueSend` in `internal/email/graph/send.go`; `reconcileOnce`, `settle` in `internal/email/sendflow/dispatch_native.go` |
| `draft-value-headers` | `GET /me/messages/{id}/$value` of a draft, and the JSON fields of an unchanged draft read twice | UNVERIFIED | The snapshot is not built from `/$value` (noisy: X-MS-*, Thread-Index, X-Unsent). It is the canonical rebuild of the draft's JSON through `mailmsg.Build` (text body required, Date pinned to `lastModifiedDateTime`, no Message-ID), so it is deterministic as long as `lastModifiedDateTime` does not change while the draft is untouched (not on reads, not on mailbox sync). `Prefer: outlook.body-content-type="text"` returns the body as `text`, converting an HTML draft; a draft whose body still arrives as `html`, or that has attachments, is refused as not sendable. `bccRecipients` is returned on drafts. | `canonical`, `GetDraftRaw` in `internal/email/graph/drafts.go` |
| `draft-create-fields` | `POST /me/messages` with `from`, `toRecipients`, `ccRecipients`, `bccRecipients`, `subject`, text `body` | UNVERIFIED | A draft created with these fields gets a stable ImmutableId, an `internetMessageId` in the create answer (read back with a GET when absent), and `PATCH /me/messages/{id}` replaces the same fields without changing the id. `from` set to the mailbox's own address is accepted. | `CreateDraft`, `UpdateDraft` |
| `draft-reply-headers` | Setting `In-Reply-To` and `References` on a created draft | UNVERIFIED | Graph accepts only `x-` headers in `internetMessageHeaders` on create, so a Graph draft cannot carry reply headers and a reply drafted through moose starts a new conversation. `DraftInput.ThreadID` is ignored. A reply endpoint (`createReply`) would be the fix and is not used. | `draftJSON` |
| `draft-permanent-delete` | `POST /me/messages/{id}/permanentDelete` | UNVERIFIED | Exists in v1.0 on messages, answers 204, and removes the draft without leaving it in Deleted Items. `DeleteDraft` and `UpdateDraft` first read `isDraft` and answer `ErrNotFound` for anything that is not a draft, so a received or sent message is never edited or permanently deleted. `DeleteStaging` is the same call, so it never deletes a message that has moved to Sent Items. | `DeleteDraft`, `requireDraft` |
| `send-rejection-codes` | The error answers to `POST /me/messages/{id}/send` | UNVERIFIED | Only these prove a refusal and so move a send to `failed`: 400 `ErrorInvalidRecipients` and `ErrorInvalidRequest`; 403 `ErrorAccessDenied`, `ErrorSendAsDenied` and `ErrorSubmissionQuotaExceeded`; 413 `ErrorMessageSizeExceeded`; 422 `ErrorQuotaExceeded`. Anything else (transport error, timeout, 429, 401, 404, 5xx, an unlisted 4xx) is `unknown`, never retried. | `provablyRejected` in `internal/email/graph/send.go` |
| `sent-copy-fields` | `from`, recipients, `subject` and text `body` of the Sent Items copy | UNVERIFIED | The Sent copy's JSON keeps `bccRecipients`, the same addresses and subject, and a text body that equals the approved body once line endings and trailing whitespace are normalised. Display names, charset and transfer encoding are not compared. A difference in any compared field is `sent_mismatch`: terminal, owner alarm, Microsoft kill switch on. | `settle`, `verifySent`, `mailmsg.SemanticDiff` |

## Rows other phases depend on (not touched here)

| Row | Status | Needed by |
| --- | --- | --- |
| `calendar-preconditions` | UNVERIFIED, not yet coded | Phases 11 and 12 |
