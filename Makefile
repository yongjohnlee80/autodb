VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# The dev checkout nests a worktree inside the bare store (family layout:
# autodb/.git bare + autodb/main worktree). Go's nested-VCS rule then points
# buildvcs at the bare dir and fails; version identity comes from LDFLAGS
# instead, so VCS stamping is disabled for all local targets. CI clones
# normally and is unaffected.
export GOFLAGS := -buildvcs=false

.PHONY: build test vet clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/autodb ./cmd/autodb

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist
