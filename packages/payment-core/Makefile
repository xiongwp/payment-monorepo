.PHONY: help build run proto install-tools test tidy clean

BINARY = bin/payment-core

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译二进制
	@mkdir -p bin
	@CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "built: $(BINARY)"

run: build ## 本地运行
	@./$(BINARY)

proto: ## 重新生成本仓的 gRPC stub（channel.v1 的 stub 由 payment-channel 提供，见 replace 指向）
	@protoc --go_out=. --go_opt=paths=source_relative \
	        --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	        api/proto/paymentcore/v1/paymentcore.proto

install-tools: ## 安装 protoc 插件
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

test: ## 单元测试
	@go test -race ./...

tidy: ## 整理依赖
	@go mod tidy

clean: ## 清理
	@rm -rf bin
