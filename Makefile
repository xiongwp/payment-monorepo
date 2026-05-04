.PHONY: help build run proto gen-sql test tidy clean

BINARY = bin/order-core

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译二进制
	@mkdir -p bin
	@CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "built: $(BINARY)"

run: build ## 本地运行
	@./$(BINARY)

proto: ## 重新生成 gRPC stub
	@protoc --go_out=. --go_opt=paths=source_relative \
	        --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	        api/proto/order/v1/order.proto

install-tools: ## 安装 protoc 插件
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

gen-sql: ## 生成 10 库分片 SQL
	@bash database/orderdb/scripts/generate.sh

test: ## 单元测试
	@go test -race ./...

tidy: ## 整理依赖
	@go mod tidy

clean: ## 清理
	@rm -rf bin
