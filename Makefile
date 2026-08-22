# golang-lib-template — development tasks
GO ?= go

.PHONY: all fmt vet tidy lint test test-race cover build clean

all: fmt vet lint test

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping lint"; \
	fi

test:
	$(GO) test -count=1 ./...

test-race:
	$(GO) test -race -count=1 ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

build:
	$(GO) build ./...

clean:
	rm -f coverage.out
