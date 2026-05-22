GO := /usr/local/go/bin/go

.PHONY: check lint fmt-check test test-fast build

check: fmt-check lint test-fast build

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: unformatted files:"; echo "$$unformatted"; \
		echo "Run: go fmt ./..."; exit 1; \
	fi

lint:
	$(GO) vet ./...

test:
	$(GO) test ./... -v

test-fast:
	$(GO) test -count=1 -short -timeout 30s ./...

build:
	$(GO) build -o fixclaw .
