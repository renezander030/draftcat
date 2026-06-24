GO := /usr/local/go/bin/go
GOLANGCI := golangci-lint

.PHONY: check lint lint-install fmt-check test test-fast build

check: fmt-check lint test-fast build

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: unformatted files:"; echo "$$unformatted"; \
		echo "Run: go fmt ./..."; exit 1; \
	fi

lint:
	$(GO) vet ./...
	$(GOLANGCI) run

lint-install:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0

test:
	$(GO) test ./... -v

test-fast:
	$(GO) test -count=1 -short -timeout 30s ./...

build:
	$(GO) build -o draftcat .
