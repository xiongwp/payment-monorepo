.PHONY: help build run test tidy clean

BINARY = bin/api-gateway

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译二进制
	@mkdir -p bin
	@CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "built: $(BINARY)"

run: build ## 本地运行
	@./$(BINARY)

test: ## 单元测试（race 检查）
	@go test -race ./...

tidy: ## 整理依赖
	@go mod tidy

clean: ## 删除产物
	@rm -rf bin
