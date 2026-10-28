BINARY_NAME=otter
MAIN_PATH=cmd/otter
BUILD_DIR=bin

GO?=go
LDFLAGS?=

# A throwaway PostgreSQL for the database tests (internal/dbtest).
CONTAINER?=docker
TEST_DB_NAME=otter-test-pg
TEST_DB_PORT?=55432
TEST_DB_URL=postgres://otter:otter@127.0.0.1:$(TEST_DB_PORT)/otter?sslmode=disable

.PHONY: build test test-race vet fmt fmt-check lint check clean help test-db test-db-stop

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

## lint: staticcheck and govulncheck, pinned as tool dependencies in go.mod
lint:
	@$(GO) tool staticcheck ./...
	@$(GO) tool govulncheck ./...

## check: format, vet and tests (CI also runs lint and the race detector)
check: fmt-check vet test

## test-db: start a throwaway PostgreSQL 18 for the database tests
test-db:
	@$(CONTAINER) run -d --rm --name $(TEST_DB_NAME) -e POSTGRES_USER=otter -e POSTGRES_PASSWORD=otter \
		-e POSTGRES_DB=otter -p 127.0.0.1:$(TEST_DB_PORT):5432 postgres:18 >/dev/null
	@echo "export OTTER_TEST_DATABASE_URL='$(TEST_DB_URL)'"

## test-db-stop: remove it
test-db-stop:
	@$(CONTAINER) rm -f $(TEST_DB_NAME) >/dev/null

## clean: remove build output
clean:
	@rm -rf "$(BUILD_DIR)"

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
