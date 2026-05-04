.PHONY: help gen gen-go clean install-tools

MODULE := github.com/xiongwp/accounting-grpc-api

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
	awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ─── 代码生成 ──────────────────────────────────────────────────────────────────
gen-go: ## 从 proto 生成 Go gRPC stubs
	@echo "Generating Go gRPC code..."
	@mkdir -p gen
	protoc \
		-I proto \
		-I /usr/include \
		--go_out=. \
		--go_opt=module=$(MODULE) \
		--go-grpc_out=. \
		--go-grpc_opt=module=$(MODULE) \
		proto/accounting.proto
	@echo "Generated: gen/accounting/v1/"

gen: gen-go ## 生成所有代码

# ─── 依赖 ─────────────────────────────────────────────────────────────────────
install-tools: ## 安装 protoc 插件
	@apt-get install -y protobuf-compiler protoc-gen-go protoc-gen-go-grpc 2>/dev/null || \
	 go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
	 go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "Tools installed"

# ─── 清理 ─────────────────────────────────────────────────────────────────────
clean: ## 删除生成的代码
	@rm -rf gen/accounting
	@echo "Cleaned"
