# Go is never installed on the host: $(GO) defaults to a wrapper that runs the
# official golang image. CI overrides it with GO=go.
GO ?= ./scripts/go
# Each server is tagged on its own line: drive/vX.Y.Z and email/vX.Y.Z. The tag prefix is
# stripped, so tag drive/v0.2.0 builds as 0.2.0. Untagged builds fall back to the short sha.
VERSION_DRIVE ?= $(shell git describe --tags --match 'drive/v*' --always --dirty 2>/dev/null | sed 's|^drive/v||' | grep . || echo dev)
VERSION_EMAIL ?= $(shell git describe --tags --match 'email/v*' --always --dirty 2>/dev/null | sed 's|^email/v||' | grep . || echo dev)
# Shared tools (the moose CLI, relay proxy) ride the drive version line.
VERSION ?= $(VERSION_DRIVE)
LDFLAGS := -s -w -X main.version=$(VERSION)
LDFLAGS_DRIVE := -s -w -X main.version=$(VERSION_DRIVE)
LDFLAGS_EMAIL := -s -w -X main.version=$(VERSION_EMAIL)
# Server binaries get their own version; everything else under cmd/ uses LDFLAGS.
SERVER_CMDS := moose-drive moose-drive-lambda moose-email moose-email-lambda moose-approver
OTHER_CMDS := $(filter-out $(addprefix cmd/,$(SERVER_CMDS)),$(wildcard cmd/*))
GOLANGCI_IMAGE ?= golangci/golangci-lint:v2.15.0
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,MPL-2.0

.PHONY: build test vet lint license-check test-store clean link-secrets up down logs ps lambda-drive lambda-email lambda-approver approver-enroll a11y-approver relay-proxy tf-validate tf-plan e2e

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/ $(addprefix ./,$(OTHER_CMDS))
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS_DRIVE)" -o dist/ ./cmd/moose-drive ./cmd/moose-drive-lambda
	@if [ -d cmd/moose-email ]; then CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS_EMAIL)" -o dist/ ./cmd/moose-email; fi
	@if [ -d cmd/moose-approver ]; then CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS_EMAIL)" -o dist/ ./cmd/moose-approver; fi

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
	for i in $$(seq 1 60); do curl -s -o /dev/null http://localhost:$(DDB_PORT)/ && break; sleep 1; done && \
	MOOSE_DYNAMO_ENDPOINT=http://localhost:$(DDB_PORT) $(GO) test -count=1 ./internal/store/... ./internal/email/store/... ./internal/approver/store/... ; \
	rc=$$?; docker stop moose-ddb-test >/dev/null; exit $$rc

# modernc.org/mathutil is BSD-3-Clause, but its LICENSE wording defeats the go-licenses classifier.
license-check:
	$(GO) run github.com/google/go-licenses@v1.6.0 check ./... --allowed_licenses=$(ALLOWED_LICENSES) --ignore modernc.org/mathutil

# Mac Mini deployment (Docker Compose, no Terraform).
COMPOSE := docker compose -f deploy/compose/docker-compose.yml
SECRETS_DIR ?= $(HOME)/.config/moose/secrets

# Idempotent: symlinks secrets into the repo, skipping any source that does not exist.
link-secrets:
	@mkdir -p deploy/compose/secrets
	@for pair in .env.compose:deploy/compose/.env gdrive-sa.json:deploy/compose/secrets/gdrive-sa.json; do \
	  src="$(SECRETS_DIR)/$${pair%%:*}"; dst="$${pair#*:}"; \
	  if [ -e "$$src" ]; then ln -sfn "$$src" "$$dst" && echo "linked $$dst -> $$src"; \
	  else echo "skip $$dst: $$src not found"; fi; \
	done

up:
	$(COMPOSE) build --build-arg VERSION=$(VERSION_DRIVE)
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f moose-drive

ps:
	$(COMPOSE) ps

# AWS deployment (Terraform roots in deploy/terraform/drive and deploy/terraform/email).
TF ?= terraform
TF_DIRS := deploy/terraform/drive deploy/terraform/email
TF_PLAN_DIR ?= deploy/terraform/drive

# The zip step runs on the host and bundles deploy/lambda/moose.yaml.
lambda-drive:
	mkdir -p dist/lambda
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -tags lambda.norpc -ldflags "$(LDFLAGS_DRIVE)" -o dist/lambda/bootstrap ./cmd/moose-drive-lambda
	cp deploy/lambda/moose.yaml dist/lambda/moose.yaml
	rm -f dist/moose-drive-lambda.zip
	cd dist/lambda && zip -q -X ../moose-drive-lambda.zip bootstrap moose.yaml

lambda-email:
	mkdir -p dist/lambda-email
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -tags lambda.norpc -ldflags "$(LDFLAGS_EMAIL)" -o dist/lambda-email/bootstrap ./cmd/moose-email
	cp deploy/lambda/moose-email.yaml dist/lambda-email/moose.yaml
	rm -f dist/moose-email.zip
	cd dist/lambda-email && zip -q -X ../moose-email.zip bootstrap moose.yaml

lambda-approver:
	mkdir -p dist/lambda-approver
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -tags lambda.norpc -ldflags "$(LDFLAGS_EMAIL)" -o dist/lambda-approver/bootstrap ./cmd/moose-approver
	cp deploy/lambda/moose-approver.yaml dist/lambda-approver/moose.yaml
	rm -f dist/moose-approver.zip
	cd dist/lambda-approver && zip -q -X ../moose-approver.zip bootstrap moose.yaml

# Prints the single-use passkey enrollment link for a deployed moose-approver (needs the
# email root applied and dist/moose-approver from `make build`).
approver-enroll:
	./scripts/approver-enroll.sh

# Renders every approver page to HTML, then runs axe-core over them (desktop width and
# 320px) in the pinned browserless Chromium image. Needs Docker.
AXE_IMAGE ?= ghcr.io/browserless/chromium:v2.38.1
a11y-approver:
	rm -rf dist/axe && mkdir -p dist/axe
	MOOSE_AXE_DUMP=../../dist/axe $(GO) test -count=1 -run TestDumpPagesForAxe ./internal/approver/
	docker run --rm --entrypoint node -v "$(CURDIR)/dist/axe:/pages:ro" -v "$(CURDIR)/scripts/axe-pages.mjs:/axe-pages.mjs:ro" $(AXE_IMAGE) /axe-pages.mjs /pages

relay-proxy:
	mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/moose-relay-proxy ./cmd/moose-relay-proxy

# Validation initializes without a backend in its own data dir, so it never disturbs a root
# that was initialized against the real state bucket (a plan after tf-validate still works).
tf-validate:
	@for d in $(TF_DIRS); do \
	  echo "== $$d"; \
	  TF_DATA_DIR=.terraform-validate $(TF) -chdir=$$d init -backend=false -input=false >/dev/null && TF_DATA_DIR=.terraform-validate $(TF) -chdir=$$d validate || exit 1; \
	done

tf-plan:
	$(TF) -chdir=$(TF_PLAN_DIR) plan -var-file=env/dev.tfvars

clean:
	rm -rf dist

# Smoke test against a live deployment. Needs MOOSE_URL, MOOSE_PAT (admin),
# MOOSE_E2E_QUERY, and MOOSE_E2E_DOC_ID; builds the CLI first unless MOOSE_BIN is set.
e2e:
	@if [ -z "$$MOOSE_BIN" ]; then $(MAKE) build && MOOSE_BIN="$(CURDIR)/dist/moose" ./test/e2e/smoke.sh; else ./test/e2e/smoke.sh; fi
