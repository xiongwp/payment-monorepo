.PHONY: build test run tidy

GO ?= go

build:
	$(GO) build ./...

test:
	$(GO) test ./...

run:
	$(GO) run ./cmd/server

tidy:
	$(GO) mod tidy
