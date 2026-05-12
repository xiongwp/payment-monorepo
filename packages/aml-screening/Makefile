.PHONY: help build run test tidy image clean smoke

BINARY = bin/aml-screening
IMAGE  = aml-screening:local

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译二进制
	@mkdir -p bin
	@CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

run: build ## 本地运行 dev 模式 (in-memory + seed)
	@AML_DEV_SEED=1 AML_ADMIN_TOKEN=admintok ./$(BINARY)

test: ## 单元测试
	@GOWORK=off go test -race ./...

tidy:
	@GOWORK=off go mod tidy

image: ## docker build
	@docker build -t $(IMAGE) .
	@echo "built: $(IMAGE)"

smoke: ## smoke test (假设 server 已在 :8088)
	@bash test/smoke.sh http://localhost:8088

clean:
	@rm -rf bin/ vendor/
