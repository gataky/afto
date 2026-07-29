BIN     := bin/aftod
PLUGINS := bin/afto-make-targets
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet e2e bench clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./daemon/cmd/aftod
	go build -o bin/afto-make-targets ./daemon/cmd/afto-make-targets

test:
	go test ./...

vet:
	go vet ./...

e2e: build
	zsh tests/e2e/harness.zsh

bench: build
	zsh tests/e2e/latency.zsh

clean:
	rm -rf bin
