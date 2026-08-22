VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

# The dev checkout nests a worktree inside the bare store (family layout:
# autodb/.git bare + autodb/main worktree). Go's nested-VCS rule then points
# buildvcs at the bare dir and fails; version identity comes from LDFLAGS
# instead, so VCS stamping is disabled for all local targets. CI clones
# normally and is unaffected.
export GOFLAGS := -buildvcs=false

.PHONY: build test test-go test-lua vet clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/autodb ./cmd/autodb

# `test` is the honest signal: BOTH sides. The Lua suite drives a real daemon and
# the neovim UI, and was previously in neither this target nor CI — so a whole
# class of behaviour (the handshake, the identity-refusal security boundary, the
# three-pane UI) had no automated signal at all. `test-go` stays for the Go-only
# path (no neovim needed).
test: test-go test-lua

test-go:
	go test ./...

# test-lua builds bin/autodb and runs the neovim smoke suite through a wrapper
# that fails on an aborted driver — nvim exits 0 on an uncaught Lua error, so the
# suite's own exit code cannot be trusted alone (see tests/run-smoke.sh).
test-lua:
	./tests/run-smoke.sh

vet:
	go vet ./...

clean:
	rm -rf bin dist
