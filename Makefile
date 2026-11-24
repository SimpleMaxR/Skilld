GO ?= /opt/homebrew/bin/go
ifeq ($(wildcard $(GO)),)
GO := go
endif

BINARY := bin/skillvault-server
BUILD_TAGS := sqlite_omit_load_extension
LDFLAGS := -s -w

.PHONY: build build-linux-static test test-go test-js bench run clean

build:
	@mkdir -p bin
	CGO_ENABLED=1 $(GO) build -mod=vendor -trimpath -tags "$(BUILD_TAGS)" -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/skillvault-server

build-linux-static:
	@mkdir -p bin
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC=x86_64-linux-musl-gcc $(GO) build -mod=vendor -trimpath -tags "$(BUILD_TAGS)" -ldflags "$(LDFLAGS) -linkmode external -extldflags '-static'" -o $(BINARY)-linux-amd64 ./cmd/skillvault-server

test: test-go test-js

test-go:
	CGO_ENABLED=1 $(GO) test -mod=vendor -race -v ./...

test-js:
	node scripts/embed-web.mjs
	node --test tests/shared.test.mjs tests/upstream.test.mjs tests/cf-worker.test.mjs tests/e2e.test.mjs

bench:
	CGO_ENABLED=1 $(GO) test -mod=vendor -bench=. -benchmem ./internal/hotstate/... ./internal/asyncwrite/...

run: build
	./$(BINARY)

clean:
	rm -rf bin/
