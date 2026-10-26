BINARY_NAME=otter
MAIN_PATH=cmd/otter
BUILD_DIR=bin

GO?=go
LDFLAGS?=

.PHONY: build test test-race vet fmt fmt-check check clean help

## build: compile the binary into bin/
build:
	@mkdir -p "$(BUILD_DIR)"
	@$(GO) build -ldflags "$(LDFLAGS)" -o "$(BUILD_DIR)/$(BINARY_NAME)" ./$(MAIN_PATH)

## test: run the tests
test:
	@$(GO) test ./...

## test-race: run the tests with the race detector
test-race:
	@$(GO) test -race ./...

## vet: run go vet
vet:
	@$(GO) vet ./...

## fmt: format the code
fmt:
	@gofmt -w .

## fmt-check: fail if any file needs formatting
fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

## check: everything CI checks that runs locally
check: fmt-check vet test

## clean: remove build output
clean:
	@rm -rf "$(BUILD_DIR)"

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
