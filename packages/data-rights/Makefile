.PHONY: help build run test tidy image clean

BINARY = bin/data-rights
IMAGE  = data-rights:local

build:
	@mkdir -p bin
	@CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

run: build
	@DR_ADMIN_TOKEN=admintok ./$(BINARY)

test:
	@GOWORK=off go test -race ./...

tidy:
	@GOWORK=off go mod tidy

image:
	@docker build -t $(IMAGE) .

clean:
	@rm -rf bin/ vendor/
