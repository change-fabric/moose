# moose

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Moose is an open source, self-hostable context hub. It exposes proprietary data sources (starting with Google Drive) to coding agents such as Claude Code, opencode, and Pi through an MCP server, a REST API with personal access tokens, and a local CLI, reachable only inside your Tailscale tailnet.

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
  MOOSE_BOOTSTRAP_ADMIN=me@example.com ./scripts/go run ./cmd/moosed
export MOOSE_CREDENTIALS=/src/dist/dev-credentials.json
./scripts/go run ./cmd/moose token bootstrap --url http://127.0.0.1:8787 --name dev
./scripts/go run ./cmd/moose whoami
```

The REST API is described in [`api/openapi.yaml`](api/openapi.yaml). CLI exit codes: 0 ok, 1 usage, 2 auth (401/403), 3 not found, 4 upstream or server error.

## License

Moose is open source software released under the [MIT License](LICENSE).
