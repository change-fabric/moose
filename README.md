<p align="center"><img src="docs/hero.gif" width="480" alt="moose: a lilac crayon scribble draws in and settles on the moose logo"></p>

# moose

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Moose is an open source, self-hostable framework for federated data access. It gives coding agents such as Claude Code, opencode, and Pi read access to your data where it already lives, through pluggable connectors, an MCP server, a REST API with personal access tokens, and a local CLI, reachable only inside your Tailscale tailnet. Content passes through on request; moose keeps no copy. Google Drive is the first connector.

Status: pre-alpha. Nothing here is production ready.

## Deployment targets

- AWS (Terraform)
- Self-hosted Mac Mini (Docker Compose)

## Overview

moose is a Go service with three surfaces over one core:

- An MCP server over streamable HTTP at `/mcp`.
- A REST API at `/v1/...`, authenticated with personal access tokens that are cross-checked against the caller's tailnet identity.
- A `moose` CLI that calls the REST API (used by Pi, which has no native MCP).

Content is pass-through: every search and fetch goes live to the source, and moose can see exactly what has been shared with its Google service account. Nothing is cached or indexed.

## Development

Go is not installed on the host. `./scripts/go` runs the Go toolchain in the official `golang` Docker image with the repo mounted, so use it wherever you would type `go`:

```sh
./scripts/go test ./...
make build test vet        # Makefile recipes use $(GO), default ./scripts/go
make license-check         # every dependency must be MIT-compatible
```

Run a local dev hub on plain HTTP with a fixed identity (refused when `MOOSE_ENV=prod`), then get a first token with the CLI. `MOOSE_CREDENTIALS` keeps the CLI's credentials in the mounted repo, since each `./scripts/go` run is a fresh container:

```sh
MOOSE_ENV=dev MOOSE_LISTEN=127.0.0.1:8787 MOOSE_DEV_IDENTITY=me@example.com \
  MOOSE_BOOTSTRAP_ADMIN=me@example.com ./scripts/go run ./cmd/moose-drive
export MOOSE_CREDENTIALS=/src/dist/dev-credentials.json
./scripts/go run ./cmd/moose token bootstrap --url http://127.0.0.1:8787 --name dev
./scripts/go run ./cmd/moose whoami
```

The REST API is described in [`api/openapi.yaml`](api/openapi.yaml). CLI exit codes: 0 ok, 1 usage, 2 auth (401/403), 3 not found, 4 upstream or server error.

## Package map

Drive-specific code lives under `internal/drive`; shared packages stay at the top of `internal`.

| Package | Role |
| --- | --- |
| `internal/drive/connectors` | Connector interface, the Google Drive connector (`gdrive`), and a fake for tests |
| `internal/drive/core` | Drive service layer over connectors |
| `internal/drive/mcp` | Drive MCP server and tools |
| `internal/drive/api` | Drive REST API router |
| `internal/server` | Auth and audit around a connector's mounts |
| `internal/mcpkit` | Shared MCP server construction, tool annotations, untrusted-content wrapper |
| `internal/email/provider` | Provider-neutral mailbox interface, search `Query`, `Capabilities` |
| `internal/email/gmail` | Gmail provider: query compiler and REST client |
| `internal/email/fake` | In-memory provider for tests |
| `internal/email/store` | Email store: accounts, OAuth state, approvals, send ledger, per-account contacts, audit. Business rules once on a small `Backend`; `dynamo` and `sqlite` backends, `storetest` conformance suites |
| `internal/email/envelope` | KMS envelope sealing (encryption context `{owner, account_id}`) with a local dev-key mode |
| `internal/email/connect` | The `/connect/google` page: Google OAuth web flow, account rows, send enable and disable |
| `internal/email/oauthas` | Hand-rolled OAuth 2.1 authorization server for the public `/mcp`: DCR, authorization code with PKCE S256, opaque tokens, rotating refresh. Login is a passkey sign-in on a phone (QR interstitial); `/mcp` sits behind `auth.RequireBearerToken` |
| `internal/email/palette` | The 12 curated account colours (every pair at CIE76 deltaE 20 or more under protan, deutan and tritan) |
| `internal/approver/api` | moose-approver service API: HMAC-signed create (idempotent by key), status, consume; payload and preview types; the client moose-email uses |
| `internal/approver/receipt` | KMS P-256 approval receipts and `Verify(pubkey, receipt, want)` |
| `internal/approver/webauthn` | Passkey ceremonies: user verification required, single-use challenges bound to request and payload digest, hashed enrollment tokens; `webauthntest` is a virtual authenticator |
| `internal/approver/store` | Approval requests, sealed previews (deleted on deny, expiry and consume), challenges, credentials |
| `internal/approver/pages` | Server-rendered approval pages: FROM banner with look-alike warning, batch approval, queue, enrollment |
| `internal/bootstrap`, `internal/config` | Startup wiring, configuration |
| `internal/auth`, `internal/store`, `internal/audit` | Identity and tokens, storage backends, audit log |
| `internal/cli` | The `moose` CLI |

## Mac Mini

Docker Compose plus the Makefile; no Terraform and no published ports. `moose-drive` joins the tailnet with tsnet as node `moose` and serves `https://moose.<tailnet>.ts.net`.

1. Create a GCP project, enable the Drive API, and create a service account. Download its JSON key to `~/.config/moose/secrets/gdrive-sa.json`.
2. Share the Drive folders moose should see with the service account's email. That sharing is the only access control on content.
3. Paste [`deploy/tailscale/acl-snippet.hujson`](deploy/tailscale/acl-snippet.hujson) into the tailnet policy file, then create an auth key: not reusable, pre-authorized, tag `tag:moose-hub`.
4. Copy [`deploy/compose/.env.example`](deploy/compose/.env.example) to `~/.config/moose/secrets/.env.compose` and fill in `TS_AUTHKEY` and `MOOSE_BOOTSTRAP_ADMIN` (your tailnet login).
5. Copy [`moose.example.yaml`](moose.example.yaml) to `deploy/compose/moose.yaml` and set `env: prod` and `tailnet`.
6. Link the secrets and start the hub:

   ```sh
   make link-secrets   # idempotent; symlinks .env and secrets/gdrive-sa.json; set SECRETS_DIR to use another folder
   make up && make ps
   make logs
   ```

7. From another tailnet device, check `curl -fsS https://moose.<tailnet>.ts.net/healthz`, then mint the first token with `moose token bootstrap --url https://moose.<tailnet>.ts.net --name laptop`.

tsnet state lives in the `moose-data` volume, so only the first boot needs the auth key. `make down` stops the hub and keeps the volume.

## AWS

Terraform root in [`deploy/terraform/drive`](deploy/terraform/drive): a Go Lambda (`provided.al2023`, arm64, outside any VPC) behind a private REST API Gateway that accepts traffic only from one execute-api VPC endpoint, DynamoDB on demand, Secrets Manager, and a t4g.nano relay EC2. The relay runs Tailscale as a subnet router (`tag:moose-relay`) and `tailscale serve` in front of `moose-relay-proxy`, which forwards to the API with a shared secret. The Lambda trusts the `Tailscale-User-Login` header only alongside that secret. Clients use the same `https://moose.<tailnet>.ts.net/mcp` URL as on the Mac Mini (set `relay_hostname = "moose-aws"` if both run in one tailnet).

1. Create an S3 bucket for state, copy `backend.tf.example` to `backend.tf` (gitignored), and set the bucket name. Copy `env/dev.tfvars.example` to `env/dev.tfvars` and set `tailnet` and `bootstrap_admin`.
2. Paste the ACL snippet (step 3 of the Mac Mini section), including `tagOwners` for `tag:moose-relay`. Export Tailscale API credentials as `TAILSCALE_OAUTH_CLIENT_ID` and `TAILSCALE_OAUTH_CLIENT_SECRET` (or `TAILSCALE_API_KEY`), plus your AWS credentials.
3. Build the artifacts and apply. The bundled Lambda config is [`deploy/lambda/moose.yaml`](deploy/lambda/moose.yaml).

   ```sh
   make lambda-drive relay-proxy
   terraform -chdir=deploy/terraform/drive init
   make tf-plan
   terraform -chdir=deploy/terraform/drive apply -var-file=env/dev.tfvars
   ```

4. Put the secret values. Terraform creates only the empty secrets, so key material never enters state:

   ```sh
   aws secretsmanager put-secret-value --secret-id moose/dev/gdrive-sa-key --secret-string file://$HOME/.config/moose/secrets/gdrive-sa.json
   aws secretsmanager put-secret-value --secret-id moose/dev/relay-secret --secret-string "$(openssl rand -base64 48)"
   ```

   The relay proxy service retries every 30 seconds until the relay secret exists. The Lambda reads both secrets at cold start: until the relay secret exists its init fails and is retried on the next request, but a warm instance keeps a missing Drive key until it is recycled (`aws lambda update-function-configuration --function-name moose-dev --description reload` forces that).
5. If the apply timed out waiting for the relay to join the tailnet, run `apply` again; the second run approves the VPC route.
6. From a tailnet device, mint the first token with `moose token bootstrap --url https://moose.<tailnet>.ts.net --name laptop`, then check `curl -fsS -H "Authorization: Bearer $MOOSE_PAT" https://moose.<tailnet>.ts.net/v1/whoami`.

Idle cost is roughly $11-12 a month: the relay about $3, one-AZ interface endpoint about $7-8, secrets $0.80; Lambda, API Gateway, and DynamoDB are near zero at pre-alpha volume. The one-hour Tailscale auth key is stored in Terraform state, so keep the state bucket private.

## Clients

Every client needs a PAT (`moose token create --name <laptop>`) and must be on the tailnet. Export it as `MOOSE_PAT`.

### Claude Code

```sh
claude mcp add --transport http moose https://moose.<tailnet>.ts.net/mcp \
  --header "Authorization: Bearer $MOOSE_PAT"
claude mcp list   # moose should show as connected
```

Then ask, for example, "use moose to search for <term>".

### opencode

In `opencode.json`:

```json
{
  "mcp": {
    "moose": {
      "type": "remote",
      "url": "https://moose.<tailnet>.ts.net/mcp",
      "headers": { "Authorization": "Bearer {env:MOOSE_PAT}" }
    }
  }
}
```

### Pi

Pi has no native MCP support, so it uses the `moose` CLI through its shell tool. Install the CLI (`make build` puts it in `dist/moose`), log in once, and Pi can call it:

```sh
moose login --url https://moose.<tailnet>.ts.net   # reads the PAT from stdin
moose search "quarterly plan" --json
moose get <id>   # prints content to stdout
```

### Smoke test

```sh
MOOSE_URL=https://moose.<tailnet>.ts.net MOOSE_PAT=<admin PAT> \
MOOSE_E2E_QUERY=<term> MOOSE_E2E_DOC_ID=<shared file id> make e2e
```

It checks health, auth (including the 401 challenge), sources, search, get, the not-found exit code, MCP `initialize` and `tools/list`, and the audit log.

## Releases and versions

Each server is versioned independently with a prefixed git tag: `drive/vX.Y.Z` for `moose-drive` and `moose-drive-lambda`, `email/vX.Y.Z` for `moose-email`. The Makefile derives `VERSION_DRIVE` and `VERSION_EMAIL` from `git describe --match 'drive/v*'` and `--match 'email/v*'` and strips the prefix, so the tag `drive/v0.2.0` builds as version `0.2.0` (`dist/moose-drive --version`). Untagged commits report the short sha, plus `-dirty` for a modified tree.

```sh
git tag drive/v0.2.0 && git push origin drive/v0.2.0
git tag email/v0.1.0 && git push origin email/v0.1.0
```

CI checks out full history and tags so these versions resolve. Override a version with `make build VERSION_DRIVE=1.2.3`.

## moose-approver

`moose-approver` is the passkey approval service that `moose-email` asks before anything irreversible: sending mail, enabling send on an account, trusting contacts, signing in to the dashboard. It rides the `email/v*` version line and has its own table, its own KMS keys and its own rpId.

- `moose-approver enroll` prints a link that enrolls the first passkey. The token is stored only as a sha256, works once and expires in 15 minutes.
- `moose-approver pubkey` prints the receipt public key to pin in moose-email. `moose-approver sweep` expires overdue requests and deletes their previews; on Lambda an EventBridge schedule runs it.
- Config: the `approver:` section (`rp_id`, `origin`, `owner`, `signing_key_id`, `kms_key_id`) plus `MOOSE_APPROVER_HMAC_SECRET_ARN`. For local runs set `MOOSE_APPROVER_DEV_KEY` (base64, 32 bytes) and `MOOSE_APPROVER_HMAC_SECRET` (32+ characters); without a signing key the dev server makes an ephemeral one and logs its public key.
- `make lambda-approver` builds the Lambda zip. `make a11y-approver` renders every page and runs axe-core over them in a browserless Chromium container (needs Docker).
- Routes for API Gateway: `/approve/r/{proxy+}`, `/approve/register/{proxy+}`, `/approve/api/{proxy+}` and `/approve/queue` with `/approve/queue/{proxy+}`.

## License

Moose is open source software released under the [MIT License](LICENSE).
