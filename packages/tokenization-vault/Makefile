.PHONY: help build run test tidy image clean smoke

BINARY = bin/tokenization-vault
IMAGE  = tokenization-vault:local

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译
	@mkdir -p bin
	@CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

run: build ## 本地跑 (dev key)
	@VAULT_ADMIN_TOKEN=admintok VAULT_TOKEN_ENV=test ./$(BINARY)

test:
	@GOWORK=off go test -race ./...

tidy:
	@GOWORK=off go mod tidy

image:
	@docker build -t $(IMAGE) .

smoke:
	@bash test/smoke.sh http://localhost:8089

clean:
	@rm -rf bin/ vendor/
