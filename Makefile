# Go is never installed on the host: $(GO) defaults to a wrapper that runs the
# official golang image. CI overrides it with GO=go.
GO ?= ./scripts/go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOLANGCI_IMAGE ?= golangci/golangci-lint:v2.15.0
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,MPL-2.0

.PHONY: build test vet lint license-check test-store clean link-secrets up down logs ps

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ ./cmd/...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	docker run --rm -v "$(CURDIR)":/app -w /app $(GOLANGCI_IMAGE) golangci-lint run

DDB_IMAGE ?= amazon/dynamodb-local:2.5.2
DDB_PORT ?= 18000

# Runs the store conformance suite against SQLite and a throwaway dynamodb-local container.
test-store:
	docker run -d --rm --name moose-ddb-test -p $(DDB_PORT):8000 $(DDB_IMAGE) >/dev/null && \
	MOOSE_DYNAMO_ENDPOINT=http://localhost:$(DDB_PORT) $(GO) test -count=1 ./internal/store/... ; \
	rc=$$?; docker stop moose-ddb-test >/dev/null; exit $$rc

# modernc.org/mathutil is BSD-3-Clause, but its LICENSE wording defeats the go-licenses classifier.
license-check:
	$(GO) run github.com/google/go-licenses@v1.6.0 check ./... --allowed_licenses=$(ALLOWED_LICENSES) --ignore modernc.org/mathutil

# Mac Mini deployment (Docker Compose, no Terraform).
COMPOSE := docker compose -f deploy/compose/docker-compose.yml
SECRETS_DIR ?= $(HOME)/1-areas/pst/moose/secrets

# Idempotent: symlinks secrets into the repo, skipping any source that does not exist.
link-secrets:
	@mkdir -p deploy/compose/secrets
	@for pair in .env.compose:deploy/compose/.env gdrive-sa.json:deploy/compose/secrets/gdrive-sa.json; do \
	  src="$(SECRETS_DIR)/$${pair%%:*}"; dst="$${pair#*:}"; \
	  if [ -e "$$src" ]; then ln -sfn "$$src" "$$dst" && echo "linked $$dst -> $$src"; \
	  else echo "skip $$dst: $$src not found"; fi; \
	done

up:
	$(COMPOSE) build --build-arg VERSION=$(VERSION)
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f moosed

ps:
	$(COMPOSE) ps

clean:
	rm -rf dist
