.PHONY: all build clean test lint fmt fmtcheck vet check deps install

GO = $(shell which go)
BIN = ./bin

# Stamp the commit into the binary so `agent-utils --version` identifies the
# build. A build that skips the Makefile reports "unknown", which is accurate
# rather than misleading.
GO_FLAGS = -ldflags "-X 'github.com/seanmcgary/agent-utils/internal/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo 'unknown')'"

all: deps/go build/cmd

# -----------------------------------------------------------------------------
# Dependencies
# -----------------------------------------------------------------------------
deps: deps/go

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
	rm -rf $(BIN) coverage.out

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
