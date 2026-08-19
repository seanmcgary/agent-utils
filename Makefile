.PHONY: all build clean test lint fmt fmtcheck vet check deps install version release

GO = $(shell which go)
BIN = ./bin

PKG = github.com/seanmcgary/agent-utils
RELEASE = ./release

# Stamp the version and commit into the binary. A build that skips the Makefile
# reports "unknown" for both, which is accurate rather than misleading.
#
# VERSION is read from the file, which scripts/version.sh rewrites to
# "<version>+<sha>" on any build that is not a tagged release. So a local or
# CI build identifies its exact commit, and only a release carries a bare
# semantic version.
VERSION = $(shell cat VERSION | tr -d '[:space:]')
COMMIT = $(shell git rev-parse --short HEAD 2>/dev/null || echo 'unknown')

LDFLAGS = -X '$(PKG)/internal/version.Version=$(VERSION)' -X '$(PKG)/internal/version.Commit=$(COMMIT)'
GO_FLAGS = -ldflags "$(LDFLAGS)"

# NOTE: the static flags are ONE -ldflags, not two. Passing -ldflags twice makes
# the last one win, which silently drops the -X stamps and produces a binary
# reporting version "unknown". Verified: `go build -ldflags "-X main.V=x"
# -ldflags="-s -w"` yields an unstamped binary.
GO_FLAGS_STATIC = -ldflags "$(LDFLAGS) -s -w"

all: deps/go build/cmd

# -----------------------------------------------------------------------------
# Dependencies
# -----------------------------------------------------------------------------
deps: deps/go

.PHONY: version
version:
	@echo $(VERSION)

# Rewrites VERSION to "<version>+<sha>" for a non-release build, or verifies it
# matches the tag for a release. CI runs this before any build.
.PHONY: version/set
version/set:
	./scripts/version.sh $(REF)

.PHONY: deps/go
deps/go:
	$(GO) mod download
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0
	$(GO) install honnef.co/go/tools/cmd/staticcheck@v0.7.0

# -----------------------------------------------------------------------------
# Build binaries
# -----------------------------------------------------------------------------

.PHONY: build/cmd/agent-utils
build/cmd/agent-utils:
	$(GO) build $(GO_FLAGS) -o $(BIN)/agent-utils ./cmd/agent-utils

.PHONY: build/cmd
build/cmd: build/cmd/agent-utils

build: build/cmd

# Installs into GOBIN, stamped the same way. This is what a cron entry should
# point at; `go install` on its own leaves the version "unknown".
install:
	$(GO) install $(GO_FLAGS) ./cmd/agent-utils

# -----------------------------------------------------------------------------
# Release binaries
# -----------------------------------------------------------------------------

# CGO_ENABLED=0 is safe because every dependency is pure Go -- notably
# modernc.org/sqlite, which is why that driver was chosen. It makes each binary
# a single static file with no libc dependency, so one linux/amd64 build runs on
# any distribution.
define build_release
	CGO_ENABLED=0 GOOS=$(1) GOARCH=$(2) $(GO) build \
		$(GO_FLAGS_STATIC) \
		-trimpath -buildvcs=false \
		-o $(RELEASE)/$(1)-$(2)/agent-utils ./cmd/agent-utils
endef

.PHONY: release/linux-amd64
release/linux-amd64:
	$(call build_release,linux,amd64)

.PHONY: release/linux-arm64
release/linux-arm64:
	$(call build_release,linux,arm64)

.PHONY: release/darwin-amd64
release/darwin-amd64:
	$(call build_release,darwin,amd64)

.PHONY: release/darwin-arm64
release/darwin-arm64:
	$(call build_release,darwin,arm64)

.PHONY: release/binaries
release/binaries: release/linux-amd64 release/linux-arm64 release/darwin-amd64 release/darwin-arm64

# Tars each platform directory into release/agent-utils-<os>-<arch>-<version>.tar.gz
.PHONY: release/bundle
release/bundle:
	./scripts/bundleReleases.sh $(VERSION)

.PHONY: release
release: release/binaries release/bundle

# -----------------------------------------------------------------------------
# Tests and linting
# -----------------------------------------------------------------------------

# -count=1 disables the test cache. A cached PASS is not evidence that the code
# in the working tree passes, and this suite is fast enough that caching buys
# nothing worth that ambiguity.
#
# The worktree package shells out to real git and the runner package spawns real
# processes, so tests are not run in parallel across packages: -p 1.
.PHONY: test
test:
	GOFLAGS="-count=1" $(GO) test -p 1 ./...

.PHONY: test/verbose
test/verbose:
	GOFLAGS="-count=1" $(GO) test -v -p 1 ./...

# Race detection is a separate target because it roughly triples the runtime.
# Worth running before a release and in CI, not on every save.
.PHONY: test/race
test/race:
	GOFLAGS="-count=1" $(GO) test -race -p 1 ./...

.PHONY: cover
cover:
	GOFLAGS="-count=1" $(GO) test -coverprofile=coverage.out -p 1 ./...
	$(GO) tool cover -func=coverage.out | tail -1
	@echo "html report: $(GO) tool cover -html=coverage.out"

.PHONY: lint
lint:
	golangci-lint run --timeout "5m"

.PHONY: vet
vet:
	$(GO) vet ./...
	# internal/service/service_darwin.go only compiles under GOOS=darwin, and
	# CI runs entirely on ubuntu-latest (see .github/workflows/main.yml), so
	# without this the darwin-only service code is built by the cross-compile
	# `build` job but never vetted or linted.
	GOOS=darwin $(GO) vet ./...

.PHONY: fmt
fmt:
	gofmt -w .

# gofmt -l exits 0 even when it lists files, so a bare `gofmt -l . && ...` can
# never fail. The output has to be captured and tested.
.PHONY: fmtcheck
fmtcheck:
	@unformatted_files=$$(gofmt -l .); \
	if [ -n "$$unformatted_files" ]; then \
		echo "The following files are not properly formatted:"; \
		echo "$$unformatted_files"; \
		echo "Please run 'make fmt' to format them."; \
		exit 1; \
	fi

# Everything that must pass before pushing. Ordered cheapest first, so a
# formatting slip fails in a second rather than after the full suite.
.PHONY: check
check: fmtcheck vet lint test

# -----------------------------------------------------------------------------
# Housekeeping
# -----------------------------------------------------------------------------

clean:
	rm -rf $(BIN) $(RELEASE) coverage.out

# Removes worktrees and state for a loop. DESTRUCTIVE, and deliberately not
# wired into `clean`: it deletes agent working trees that may hold unpushed
# commits. Pass the config explicitly.
#
#   make loop/reset CONFIG=examples/planning.yaml ISSUE=42
.PHONY: loop/reset
loop/reset:
	@test -n "$(CONFIG)" || { echo "usage: make loop/reset CONFIG=<file> ISSUE=<n>"; exit 1; }
	@test -n "$(ISSUE)" || { echo "usage: make loop/reset CONFIG=<file> ISSUE=<n>"; exit 1; }
	$(BIN)/agent-utils loop reset --config $(CONFIG) --issue $(ISSUE)
