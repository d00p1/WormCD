BINARY  := bin/wormcd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/d00p1/wormcd/internal/version.Version=$(VERSION) \
	-X github.com/d00p1/wormcd/internal/version.Commit=$(COMMIT) \
	-X github.com/d00p1/wormcd/internal/version.Date=$(DATE)

.PHONY: build test lint fmt check fix tidy clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/wormcd

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -s -l -w .

check:
	mise run check

fix:
	mise run fix

tidy:
	go mod tidy

clean:
	rm -rf bin
