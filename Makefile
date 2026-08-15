VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test vet clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/autodb ./cmd/autodb

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist
