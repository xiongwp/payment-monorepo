.PHONY: help build build-mock run run-mock proto gen-sql test test-mock tidy clean install-tools

BINARY      = bin/payment-channel
BINARY_MOCK = bin/mockserver

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译二进制
	@mkdir -p bin
	@CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "built: $(BINARY)"

run: build ## 本地运行
	@./$(BINARY)

build-mock: ## 编译 PH 渠道 mockserver
	@mkdir -p bin
	@CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY_MOCK) ./cmd/mockserver
	@echo "built: $(BINARY_MOCK)"

run-mock: build-mock ## 启动 PH 渠道 mockserver（:9400）
	@./$(BINARY_MOCK)

test-mock: ## 只跑 mockserver e2e 测试
	@go test -race -v ./internal/mockserver/...

proto: ## 重新生成 gRPC stub
	@protoc --go_out=. --go_opt=paths=source_relative \
	        --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	        api/proto/channel/v1/channel.proto

install-tools: ## 安装 protoc 插件
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

gen-sql: ## 生成 10 库分片 SQL
	@bash database/paychandb/scripts/generate.sh

test: ## 单元测试
	@go test -race ./...

tidy: ## 整理依赖
	@go mod tidy

clean: ## 清理
	@rm -rf bin
