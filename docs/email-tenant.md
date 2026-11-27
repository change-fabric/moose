# Deploying a moose-email tenant

moose-email is single-tenant: one AWS account, one owner, one domain, one
Google OAuth client. This is the whole path from nothing to a working
connector. Patrick's deployment and a second tenant (for example Conner's) use
the same steps with different values.

What you end up with, all from `deploy/terraform/email`:

- one API Gateway HTTP API on your domain, in front of two Lambdas:
  `moose-email` (MCP at `/email/mcp`, the OAuth 2.1 server, the Google connect
  page) and `moose-approver` (passkey pages under `/approve`);
- two DynamoDB tables, two KMS keys for data and previews plus one asymmetric
  key that signs approval receipts, Secrets Manager entries, a private
  attachments bucket with its sweeper, alarms, a budget and a GitHub deploy role.

Live sending ships off (`email.send.live: false`). You turn it on last, after
the phone check at the end.

## Prerequisites

- An AWS account you can apply Terraform into, an AWS CLI profile for it, and a
  Terraform state bucket that already exists (moose never creates it).
- A Route53 public hosted zone for your domain, and its zone id. moose creates
  only the records inside it (the domain alias and the certificate validation
  records), never the zone.
- The account's GitHub OIDC provider (`token.actions.githubusercontent.com`)
  must already exist. moose looks it up and never creates it.
- Docker (the repo builds Go through `./scripts/go`) and Terraform 1.10 or later.
- A phone with Face ID, a fingerprint reader or another passkey authenticator.

## 1. Create the Google OAuth client

Each tenant brings its own client; there is no shared one. The app stays in
publishing status "In production" and unverified, so the owner clicks through
Google's unverified-app warning once per account. Google caps an unverified
app at 100 users over its lifetime, which is far above one owner.

1. In Google Cloud, create or pick a project and enable the **Gmail API**.
2. Configure the OAuth consent screen: user type External, then publish it
   (**In production**). Leave verification unsubmitted.
3. Add the scopes `gmail.readonly` and `gmail.compose`. Nothing broader is
   requested, and sending is gated in moose's code, not by the scope.
4. Create an OAuth client of type **Web application**. Add this authorized
   redirect URI, with your own domain:

   ```
   https://<domain_name>/connect/google/callback
   ```

   After the apply, `terraform output google_redirect_uri` prints the exact
   value to check against.
5. Keep the client id and secret to hand for step 4 below.

## 2. Write the Terraform inputs

From the repo root:

```
cp deploy/terraform/email/backend.tf.example deploy/terraform/email/backend.tf
cp deploy/terraform/email/env/prod.tfvars.example deploy/terraform/email/env/prod.tfvars
```

Edit `backend.tf` for your state bucket, key and profile. Both files are
gitignored. In `prod.tfvars` set:

| Variable | Meaning |
| --- | --- |
| `domain_name` | The public hostname, for example `moose.example.org`. It is also the passkey rp id and the OAuth issuer, so it cannot change after passkeys are enrolled. |
| `route53_zone_id` | Id of the hosted zone that contains `domain_name`. |
| `owner` | The single owner name that tokens, passkeys and accounts belong to. |
| `github_repository_id` | `gh api repos/<owner>/<repo> --jq .id`. The deploy role trusts this immutable id, not the repository name. |
| `budget_emails`, `budget_limit_usd` | Who is told at 80 percent of actual spend or 100 percent of forecast, and the monthly limit. |
| `alarm_emails` | Optional. Defaults to `budget_emails`. Each address confirms an SNS subscription email once. |
| `env`, `region`, `aws_profile` | Environment name, region and CLI profile. |

`api_throttle_rate` and `api_throttle_burst` bound the API. No function sets
reserved concurrency, because the Lambda quota is shared across the account.

## 3. Build and plan

```
make lambda-email lambda-approver lambda-sweeper tf-validate
terraform -chdir=deploy/terraform/email init
terraform -chdir=deploy/terraform/email plan -var-file=env/prod.tfvars -out=tf.plan
```

Read the plan. Then apply exactly that file:

```
terraform -chdir=deploy/terraform/email apply tf.plan
```

The first apply waits on the ACM certificate, which validates through the DNS
records Terraform writes into your zone. It can take a few minutes. The plan
file is local and gitignored.

## 4. Store the Google client

Terraform creates the secret container but never holds the value, so the client
secret stays out of state. The `google_oauth_secret_put` output prints the
command; fill in the two values:

```
terraform -chdir=deploy/terraform/email output -raw google_oauth_secret_put
```

## 5. Enroll your passkey

```
make build
make approver-enroll
```

This prints a single-use link that expires in 15 minutes. Open it on the phone
that will hold the passkey and register it. It works once; run the target again
for a new link. Passkeys are the only approval method (no TOTP, no fallback).

## 6. Connect Google and add the connector

1. Open `terraform output -raw connect_url`. Sign-in shows a QR code; scan it with
   the phone and approve with the passkey. Then use **Connect Google account**
   for each mailbox. Click through the unverified-app warning. Connect as many
   accounts as you like; each gets a nickname and its own colour.
2. Claude.ai: add a custom connector with the URL from
   `terraform output -raw mcp_url`.
3. Claude Code:

   ```
   claude mcp add --transport http moose-email "$(terraform -chdir=deploy/terraform/email output -raw mcp_url)"
   ```

4. Ask for `email_list_accounts` in each client and confirm every account
   appears with its nickname, address and `send_enabled: false`.

Reading, searching, attachments, drafts and contact triage now work. Sending
does not yet.

## 7. Sending, in order

Each of these is yours to do. The agent never applies Terraform or flips live
sending.

1. **Enable send per account.** Sending is opt-in per account. Ask Claude to run
   `email_account_enable_send` for the account (it needs the exact from address),
   open the approval link on the phone, check the FROM banner names the right
   account, tap **Send from this account**, then approve with the passkey.
2. **Dry run.** With `email.send.live` still false, have Claude search, create a
   draft to yourself, call `email_request_send`, approve on the phone, then call
   `email_send_status`. The result is `would_send`: everything ran except the
   Gmail call.
3. **Go live.** Open a separate pull request that changes `send.live` to `true`
   in `deploy/lambda/moose-email.yaml`, merge it, rebuild with `make lambda-email`
   and deploy the new zip. The flag ships inside the zip.
4. **First real send.** Send one message to your own address and confirm it in
   the Sent folder.

## Operating it

- **Kill switch.** `moose-email kill-switch on [reason]`, `off` and `status`
  read and write the live table, and every send request and dispatch checks it,
  so it takes effect at once with no redeploy. It needs the same environment as
  the function (see `terraform output approver_env` for the approver's
  equivalent) and AWS credentials.
- **Hourly cap and per-account disable.** Each account has an hourly send cap.
  Turn an account's sending off from the connect page or with
  `email_account_enable_send` and `enable: false`; disabling needs no approval.
- **Alarms.** Function errors, throttles and long durations, API 5xx and a burst
  of 4xx, DynamoDB system errors and write throttles, and the attachment sweeper
  alarms all notify the `moose-<env>-alerts` topic.
- **What an approval guarantees.** The send path stores the approved message at
  request time, KMS-encrypted, and sends only that. For Gmail it is byte-exact:
  the stored RFC822 bytes go out through `messages.send`, with no re-read of the
  draft. For Microsoft it is a semantic match plus verify, because Exchange
  re-serialises a message when it sends it: moose creates a staging draft from
  the stored snapshot, records the draft's id and Message-ID in the send ledger,
  records the send intent, then sends that draft. After sending it finds the Sent
  Items copy by the draft's immutable id (never by subject or time) and checks
  that From, to, cc, bcc, subject and decoded text body equal the approved
  message. The approval page says "Sent content will match these fields" for
  Microsoft accounts. The source draft is deleted after a send only if it still
  matches the approval.
- **A send in state `unknown`** means the provider call may have been accepted
  but the outcome was not recorded. moose never retries it. On Gmail, check the
  Sent folder. On Microsoft, moose keeps looking for the Sent copy by the staging
  draft's id (five looks over about ten minutes from status calls, then the
  sweeper for 24 hours). After 24 hours the send is reported with
  `needs_operator`: check the Sent folder and mark it sent or not sent.
- **A send in state `submitted`** (Microsoft) was accepted by Exchange but the
  Sent copy has not been seen yet. Sending is asynchronous, so this is neither a
  failure nor a confirmation; call `email_send_status` again.
- **A send in state `sent_mismatch`** (Microsoft) means the Sent copy differs from
  the approved message and mail may have been delivered. It is never retried.
  moose logs an `ALARM sent_mismatch` line, switches the Microsoft send kill
  switch on (`moose-email kill-switch on provider=microsoft`; turn it off with
  `moose-email kill-switch off provider=microsoft` after investigating), and the
  owner decides what to do.
- **Staging drafts.** A Microsoft send briefly leaves a staging draft in Drafts.
  The sweeper deletes it only when the ledger says the send failed (never
  issued, or provably rejected) or was sent, and only while it is still a draft;
  it never deletes a staging draft whose ledger is `submitted` or `unknown`, and
  never a message that has moved to Sent Items.
- **Attachments** live in a private bucket for about an hour and are deleted by
  the sweeper every 15 minutes. Extracted text is never stored.
- **Teardown.** Set `deletion_protection = false` and apply before destroying
  the tables.

## A second tenant

Repeat the steps in a different AWS account with its own domain, hosted zone,
GCP OAuth client, state key, tfvars and passkey. Nothing is shared: the
rp id is the tenant's domain, so passkeys, receipts and tokens do not cross.
