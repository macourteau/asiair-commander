# Build, test, and packaging targets for asiair-commander.

# Version metadata baked into the binary via -ldflags (cmd/asiair-commander's
# main.version / main.commit / main.buildTime). Guarded so `make build` works
# in a checkout with no tags yet: git describe falls back to the short commit
# hash, and each var stays overridable from the environment or the command
# line (`make build VERSION=1.2.3`), which CI uses to pin an exact release.
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

# The NSIS installer's VIProductVersion needs a strict numeric x.y.z (no "v"
# prefix, no git-describe dev suffix like "-3-gabcdef"). Fall back to 0.0.0
# when VERSION isn't a clean tag, so `make installer` works untagged too.
INSTALLER_VERSION := $(shell echo '$(VERSION)' | sed -n 's/^v\{0,1\}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$$/\1/p')
ifeq ($(INSTALLER_VERSION),)
INSTALLER_VERSION := 0.0.0
endif

# The touch restores the tracked, empty .gitkeep the rm took, so the tree stays clean.
.PHONY: spa
spa: ## Build the React/Vite SPA and stage it into the Go embed dir (internal/web/dist)
	cd web && npm ci && ASIAIR_COMMANDER_VERSION=$(VERSION) npm run build
	rm -rf internal/web/dist
	mkdir -p internal/web/dist
	cp -R web/dist/. internal/web/dist/
	touch internal/web/dist/.gitkeep

.PHONY: build
build: spa ## Build the asiair-commander binary for the host OS/arch
	go build -ldflags "$(LDFLAGS)" -o bin/asiair-commander ./cmd/asiair-commander

.PHONY: build-windows
build-windows: spa ## Cross-compile the Windows amd64 binary the installer packages
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/asiair-commander.exe ./cmd/asiair-commander

.PHONY: astap-windows
astap-windows: ## Fetch + verify the pinned Windows astap_cli.exe into the installer payload (bin/)
	go run ./tools/fetch-astap fetch windows/amd64 bin

# Authenticode signing is opt-in via `make installer SIGN=1`, which the release
# workflow sets once an OIDC session exists. Unset — a laptop build, or the CI
# installer job — the recipe signs nothing, so neither needs Azure
# credentials. scripts/sign-windows.sh documents the env it wants.
# The path is absolutised here because NSIS runs !finalize through system()
# against its own working directory rather than the .nsi's.
SIGN_SH := $(abspath scripts/sign-windows.sh)
ifdef SIGN
NSIS_SIGN_FLAGS := -DSIGN=1 -DSIGNSH=$(SIGN_SH)
# Signs the packaged binary before makensis compresses it: a file inside the
# installer's LZMA blob is no longer a PE and can never be signed afterwards.
SIGN_PAYLOAD := $(SIGN_SH) bin/asiair-commander.exe
else
NSIS_SIGN_FLAGS :=
SIGN_PAYLOAD := :
endif

# THIRD_PARTY_NOTICES/ ships beside every binary distribution: the Windows
# installer, the bare .exe attached to a release (as a zip), and the container
# image. It is generated at build time and gitignored rather than committed,
# because it is derived from go.sum, web/package-lock.json and the ASTAP pin: a
# committed copy would go stale on every dependency bump, while a generated one
# always describes the exact bytes it ships with. For a given commit and target
# the content is deterministic: go-licenses copies licence files out of the
# module cache, and the index template uses nothing it looks up over the network.
#
# Layout:
#   README.txt     what follows, plus an index of the Go libraries with versions
#   LICENSE.txt    this project's own licence
#   astap_cli.txt  ASTAP's astap_cli, bundled unmodified (third_party/notices/)
#   go/<library>/  the licence and NOTICE files of every Go library linked into
#                  cmd/asiair-commander for NOTICES_GOOS/NOTICES_GOARCH, saved by
#                  go-licenses (pinned by the `tool` directive in go.mod). Its
#                  `save` also copies the full source of any reciprocal-licensed
#                  module, which is what such a licence asks of a distributor.
#   web.txt        the npm packages bundled into the SPA, written by the Vite
#                  build (rollup-plugin-license, web/vite.config.ts) to
#                  internal/web/dist/third-party-notices.txt; `make spa` first.
#
# The dependency set differs per OS, so each distribution is generated for its
# own target. `go-licenses check` runs first and fails the build on a licence it
# cannot classify, or one that is forbidden or restricted, so no module is ever
# dropped silently. This module itself is --ignore'd, as LICENSE.txt already
# carries its licence; its dependencies are still checked. go-licenses is built
# for the host and handed the target through GOOS/GOARCH: `go tool` or `go run`
# would cross-compile the tool itself for the target instead.
MODULE := github.com/macourteau/asiair-commander
NOTICES ?= bin/THIRD_PARTY_NOTICES
NOTICES_GOOS ?= $(shell go env GOOS)
NOTICES_GOARCH ?= $(shell go env GOARCH)
SPA_NOTICES := internal/web/dist/third-party-notices.txt
GO_LICENSES := bin/go-licenses
GO_LICENSES_RUN := CGO_ENABLED=0 GOOS=$(NOTICES_GOOS) GOARCH=$(NOTICES_GOARCH) $(GO_LICENSES)
ASTAP_VERSION = $(shell sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' deploy/astap/astap-cli.json)

.PHONY: notices
notices: ## Generate THIRD_PARTY_NOTICES/ for NOTICES_GOOS/NOTICES_GOARCH (run `make spa` first)
	@test -f $(SPA_NOTICES) || { echo "notices: $(SPA_NOTICES) is missing; run 'make spa' first" >&2; exit 1; }
	rm -rf $(NOTICES)
	mkdir -p $(NOTICES) $(dir $(GO_LICENSES))
	go build -o $(GO_LICENSES) github.com/google/go-licenses/v2
	$(GO_LICENSES_RUN) check --disallowed_types=forbidden,restricted,unknown --ignore $(MODULE) ./cmd/asiair-commander
	$(GO_LICENSES_RUN) save --save_path $(NOTICES)/go --ignore $(MODULE) ./cmd/asiair-commander
	$(GO_LICENSES_RUN) report --template third_party/notices/go-index.tpl --ignore $(MODULE) ./cmd/asiair-commander > $(NOTICES)/go-index.tmp
	cp LICENSE $(NOTICES)/LICENSE.txt
	cp third_party/notices/astap_cli.txt $(NOTICES)/astap_cli.txt
	cp $(SPA_NOTICES) $(NOTICES)/web.txt
	{ \
	  echo "ASIAIR Commander $(VERSION) for $(NOTICES_GOOS)/$(NOTICES_GOARCH): licence and third-party notices"; \
	  echo; \
	  echo "  LICENSE.txt    ASIAIR Commander itself (MIT)"; \
	  echo "  astap_cli.txt  the bundled ASTAP plate solver, version $(ASTAP_VERSION) (MPL-2.0)"; \
	  echo "  go/            licence and notice files of the Go libraries compiled into"; \
	  echo "                 the asiair-commander binary, by import path"; \
	  echo "  web.txt        the npm packages bundled into the web interface"; \
	  echo; \
	  echo "Go libraries compiled into the binary, by import path, each with the version"; \
	  echo "of the Go module that provides it. That module's source is available from"; \
	  echo "https://proxy.golang.org at that version."; \
	  cat $(NOTICES)/go-index.tmp; \
	} > $(NOTICES)/README.txt
	rm -f $(NOTICES)/go-index.tmp

# notices runs from the recipe rather than as a prerequisite: it reads the SPA
# notices that build-windows (via spa) writes, and `make -j` would otherwise be
# free to run it first against a stale internal/web/dist. The notices describe
# the windows/amd64 binary the installer ships.
.PHONY: installer
installer: build-windows astap-windows ## Build the NSIS Windows installer (requires makensis; SIGN=1 to sign)
	$(MAKE) notices NOTICES_GOOS=windows NOTICES_GOARCH=amd64
	mkdir -p dist
	$(SIGN_PAYLOAD)
	makensis -DVERSION=$(INSTALLER_VERSION) $(NSIS_SIGN_FLAGS) deploy/windows/installer.nsi

.PHONY: test
test: ## Build, vet, and test the Go module
	go build ./...
	go vet ./...
	go test ./...

.PHONY: test-race
test-race: ## Run the test suite with the race detector
	go test -race ./...

# Mirrors CI's required build-test job (.github/workflows/ci.yml) step for step,
# so a green `make check` predicts a green required check; keep the two in sync.
# Like CI it builds the SPA into web/dist only, so the Go steps embed no SPA
# and serve internal/web/placeholder.html. The transcript-immutability step diffs against the
# merge base with CHECK_BASE (CI uses the PR's base) and says so when that ref
# is absent rather than passing quietly.
CHECK_BASE ?= origin/main
TRANSCRIPT_DIRS := internal/remote/proto/testdata/transcripts/ internal/remote/tunnel/testdata/env/

.PHONY: check
check: export CGO_ENABLED := 0
check: ## Run the same checks as CI's required build-test job
	cd web && npm ci && npm run build
	cd web && npm test
	@out=$$(gofmt -l $$(find . -name '*.go')); \
	  if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	@if git rev-parse --verify -q $(CHECK_BASE) >/dev/null; then \
	  base=$$(git merge-base $(CHECK_BASE) HEAD) || { echo "check: $(CHECK_BASE) shares no history with HEAD (set CHECK_BASE=<ref>)" >&2; exit 1; }; \
	  changed=$$(git diff --no-renames --diff-filter=MD --name-only "$$base" HEAD -- $(TRANSCRIPT_DIRS)) || exit 1; \
	  if [ -n "$$changed" ]; then \
	    echo "recorded protocol transcripts and env values must not be modified or deleted (add a new one for a protocol or env format change):"; \
	    echo "$$changed"; exit 1; \
	  fi; \
	else \
	  echo "check: $(CHECK_BASE) not found; skipping the transcript immutability step (set CHECK_BASE=<ref>)"; \
	fi
	go build ./...
	go vet ./...
	go vet -tags remotetest ./...
	go vet -tags "integration remotetest" ./...
	go vet -tags live ./...
	go vet -tags demo ./...
	CGO_ENABLED=1 go test -race ./...
	GOOS=windows GOARCH=amd64 go build ./...

.PHONY: integration
integration: ## Stand up the Samba/NT1 container and run the real-SMB tests, direct and through a real tunnel (needs Docker)
	test/integration/run.sh

# The curated README screenshots: a live -tags demo run over synthetic sky
# (tools/starfield), captured and written to docs/screenshots/ as WebP. See
# scripts/readme-screenshots.sh for what it needs and caches.
.PHONY: screenshots
screenshots: ## Regenerate docs/screenshots/ from a live demo run (needs Playwright, sqlite3, cwebp)
	scripts/readme-screenshots.sh

# A local server for UI passes, run in the background and stopped again through
# the PID it writes itself (scripts/demo-server.sh). DEMO_DATA is per checkout
# and gitignored; SEED=<seeder> reseeds it from tools/<seeder> first (SEED_ARGS
# for extra arguments). TAGS defaults to demo, except for the seeders that
# stage offline or GUID-less rigs: a demo build answers every rig with an
# in-memory device, which would bring their rigs online and change the state
# they stage. PORT rewrites the seeded web_port; MAX_LIFETIME overrides a demo
# build's 16h cap.
DEMO_DATA ?= tmp/demo
PLAIN_SEEDERS := seedack seedmosaic seednight seedreal seedscan
TAGS ?= $(if $(filter $(PLAIN_SEEDERS),$(SEED)),,demo)
DEMO_ENV = DEMO_DATA="$(DEMO_DATA)" SEED="$(SEED)" SEED_ARGS="$(SEED_ARGS)" TAGS="$(TAGS)" \
	PORT="$(PORT)" MAX_LIFETIME="$(MAX_LIFETIME)" LDFLAGS="$(LDFLAGS)"

.PHONY: demo-up
demo-up: spa ## Build and start a background server for a UI pass (SEED=, TAGS=, PORT=, DEMO_DATA=, MAX_LIFETIME=)
	$(DEMO_ENV) scripts/demo-server.sh up

.PHONY: demo-down
demo-down: ## Stop the demo-up server and verify it is gone
	$(DEMO_ENV) scripts/demo-server.sh down

.PHONY: demo-status
demo-status: ## Report whether the demo-up server is running
	$(DEMO_ENV) scripts/demo-server.sh status

.PHONY: dev
dev: ## Run the backend and the SPA dev server (two terminals, run these separately)
	@echo "Terminal 1: go run ./cmd/asiair-commander run"
	@echo "Terminal 2: cd web && npm run dev"
	@echo "Vite (http://localhost:5173) proxies /api, /events, and /healthz to the Go backend on :8686 — see web/vite.config.ts."

.PHONY: help
help: ## List the documented targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
