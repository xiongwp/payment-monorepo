.PHONY: help build build-batchtask test e2e-test \
        docker-build docker-build-all \
        docker-up docker-up-dev docker-down \
        docker-restart docker-restart-batchtask \
        docker-status docker-logs docker-logs-batchtask \
        init-db gen-kitex clean lint fmt mod-tidy install-tools benchmark all

COMPOSE           := $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo "docker-compose")
# 单一 image：service 和 batchtask 共用 accounting-system:latest，避免发版漂移。
IMAGE_NAME        := accounting-system:latest
BINARY            := bin/accounting-system
BINARY_BATCHTASK  := bin/accounting-batchtask

help: ## 显示帮助信息
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
	awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-26s\033[0m %s\n", $$1, $$2}'

# ─── 构建 ─────────────────────────────────────────────────────────────────────
build: ## 编译 accounting-system binary
	@echo "Building $(BINARY)..."
	@mkdir -p bin
	@go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "Done: $(BINARY)"

build-batchtask: ## 编译 accounting-batchtask binary
	@echo "Building $(BINARY_BATCHTASK)..."
	@mkdir -p bin
	@go build -trimpath -ldflags="-s -w" -o $(BINARY_BATCHTASK) ./cmd/batchtask
	@echo "Done: $(BINARY_BATCHTASK)"

docker-build: ## 构建 accounting-system Docker 镜像（含 service + batchtask 两个 binary）
	@echo "Building Docker image $(IMAGE_NAME)..."
	@# 直接走 docker buildx build，context = packages/。
	@# compose v2 / bake 对 build.context='..' 处理有 bug（实测 191B context
	@# 不传文件），改 buildx 直接调，最朴素也最稳。
	@docker buildx build \
		-f $(CURDIR)/Dockerfile \
		-t $(IMAGE_NAME) \
		--load \
		$(CURDIR)/..
	@echo "Done: $(IMAGE_NAME) (binaries: /app/accounting-system + /app/accounting-batchtask)"

# 单 image 设计后 docker-build-all 等同于 docker-build；保留 alias 兼容旧脚本。
docker-build-all: docker-build ## 构建所有 Docker 镜像（与 docker-build 等价；保留 alias）

# ─── 运行 ─────────────────────────────────────────────────────────────────────
run: ## 本地运行 accounting-system（需先启动依赖服务）
	@go run ./cmd/server

run-batchtask: ## 本地守护模式运行 batchtask（需 accounting-service 已在运行）
	@go run ./cmd/batchtask

# ─── Docker 操作 ──────────────────────────────────────────────────────────────
docker-up: ## 完整部署 (10库 + Kafka + etcd + batchtask crond)
	@bash deploy.sh full

docker-up-dev: ## 开发模式部署 (3库 + Redis + batchtask crond)
	@bash deploy.sh dev

docker-down: ## 停止并删除所有容器和 volume
	@bash deploy.sh down

docker-restart: ## 重启 accounting-service
	@bash deploy.sh restart

docker-restart-batchtask: ## 重启 accounting-batchtask（重新加载 crontab）
	@bash deploy.sh restart-batchtask

docker-status: ## 查看所有服务状态
	@bash deploy.sh status

docker-logs: ## 追踪 accounting-service 日志
	@$(COMPOSE) -f docker-compose.yml logs -f accounting-service

docker-logs-batchtask: ## 追踪 accounting-batchtask（crond）日志
	@bash deploy.sh logs-batchtask

# ─── 数据库 ────────────────────────────────────────────────────────────────────
init-db: ## 手动初始化数据库（在 docker 外执行，适用于本地 MySQL）
	@echo "Initializing account_meta (metadata database)..."
	@mysql -h127.0.0.1 -P3306 -uroot -ppassword -e "CREATE DATABASE IF NOT EXISTS account_meta DEFAULT CHARACTER SET utf8mb4;" 2>/dev/null || true
	@mysql -h127.0.0.1 -P3306 -uroot -ppassword account_meta < database/init/02_schema_meta.sql

	@echo "Initializing accounting_db_0 ~ accounting_db_9 (sharded databases)..."
	@for i in 0 1 2 3 4 5 6 7 8 9; do \
		echo "  -> accounting_db_$$i"; \
		mysql -h127.0.0.1 -P3306 -uroot -ppassword -e \
		  "CREATE DATABASE IF NOT EXISTS accounting_db_$$i DEFAULT CHARACTER SET utf8mb4;" 2>/dev/null || true; \
		mysql -h127.0.0.1 -P3306 -uroot -ppassword accounting_db_$$i \
		    < database/init/01-schema.sql 2>/dev/null || true; \
		mysql -h127.0.0.1 -P3306 -uroot -ppassword accounting_db_$$i \
		    < database/init/02-create-shards_0$$i.sql 2>/dev/null || true; \
	done
	@echo "Databases initialized!"

# ─── 测试 ─────────────────────────────────────────────────────────────────────
test: ## 运行单元测试
	@go test -v -race -coverprofile=coverage.out ./internal/...

e2e-test: ## 运行 E2E 测试（需要数据库就绪）
	@echo "Running E2E tests..."
	@go test -v -timeout 120s ./tests/e2e/...

coverage: test ## 生成测试覆盖率报告
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

benchmark: ## 运行性能测试
	@go test -bench=. -benchmem ./...

# ─── 代码质量 ──────────────────────────────────────────────────────────────────
lint: ## 代码静态检查
	@golangci-lint run ./...

fmt: ## 格式化代码
	@go fmt ./...
	@command -v goimports >/dev/null && goimports -w . || true

mod-tidy: ## 整理依赖
	@go mod tidy

# ─── 代码生成 ──────────────────────────────────────────────────────────────────
gen-kitex: ## 生成 Kitex 代码
	@kitex -module github.com/accounting-system -service accounting ./idl/accounting.thrift

# ─── 工具安装 ──────────────────────────────────────────────────────────────────
install-tools: ## 安装开发工具
	@go install github.com/cloudwego/kitex/tool/cmd/kitex@latest
	@go install github.com/cloudwego/thriftgo@latest
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@go install golang.org/x/tools/cmd/goimports@latest

# ─── 清理 ─────────────────────────────────────────────────────────────────────
clean: ## 清理构建产物
	@rm -rf bin/ coverage.out coverage.html
	@go clean

all: clean fmt lint test build ## 完整构建流程 (lint + test + build)
