# reconplatform — 开发与运维 Makefile.
#
# 用法: make help  打印全部 target 说明.

.PHONY: help deps run run-admin up down logs build build-service build-admin build-cli \
        fmt vet lint check test test-short cover cover-html bench \
        new-rule list-rules test-rule test-rules lint-rules watch-rules \
        catalog-export catalog-import catalog-diff fixture-gen clean dev ci all
.DEFAULT_GOAL := help

PKG       := github.com/xiongwp/reconplatform
GO        := go
GOFLAGS   ?= -trimpath
LDFLAGS   ?= -s -w
BUILD_DIR := ./bin
COVERAGE  := /tmp/recon-coverage.out

# Catalog / scripts 目录
CATALOG_DIR := ./internal/catalog/scripts
FIXTURE_DIR := ./internal/catalog/scripts/fixtures

# Admin web 默认地址 (本地 dev)
ADMIN_URL ?= http://localhost:8080

# ─── meta ─────────────────────────────────────────────────────

help:  ## 打印全部 target 说明
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST) | sort

# ─── 基础 ─────────────────────────────────────────────────────

deps:  ## 拉依赖 + tidy
	$(GO) mod tidy
	$(GO) mod download

run:  ## 本地直接跑 reconplatform service (cmd/main.go)
	$(GO) run ./cmd

run-admin:  ## 本地跑 admin web (cmd/recon-admin)
	RECON_HTTP_PORT=8080 RECON_ENV=dev \
		$(GO) run ./cmd/recon-admin

up:  ## docker compose 起依赖栈 (kafka/redis/clickhouse + service)
	docker compose up -d

down:  ## 停依赖栈
	docker compose down

logs:  ## tail 主服务日志
	docker compose logs -f recon-service recon-admin

# ─── 构建 ─────────────────────────────────────────────────────

build: build-service build-admin build-cli  ## 构建全部三个二进制
	@echo "✓ all binaries built into $(BUILD_DIR)/"

build-service:  ## 构建 reconplatform 主服务
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" \
		-o $(BUILD_DIR)/reconplatform ./cmd

build-admin:  ## 构建 recon-admin web
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" \
		-o $(BUILD_DIR)/recon-admin ./cmd/recon-admin

build-cli:  ## 构建 recon-cli (本地试跑规则 + 导入导出)
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" \
		-o $(BUILD_DIR)/recon-cli ./cmd/recon-cli

# ─── 代码质量 ─────────────────────────────────────────────────

fmt:  ## gofmt + goimports 整理代码
	gofmt -s -w .
	@command -v goimports >/dev/null && goimports -w . || echo "  (goimports not installed, skip)"

vet:  ## go vet
	$(GO) vet ./...

lint:  ## golangci-lint (若未装跳过)
	@command -v golangci-lint >/dev/null && golangci-lint run ./... \
		|| echo "  (golangci-lint not installed, run: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)"

check: fmt vet lint  ## 全套静态检查

# ─── 测试 ─────────────────────────────────────────────────────

test:  ## 跑所有单测
	$(GO) test -race -count=1 ./...

test-short:  ## 跳过长跑测试 (-short)
	$(GO) test -race -count=1 -short ./...

cover:  ## 跑测试 + 覆盖率
	$(GO) test -race -count=1 -coverprofile=$(COVERAGE) ./...
	$(GO) tool cover -func=$(COVERAGE) | tail -1
	@echo "  open report:  go tool cover -html=$(COVERAGE)"

cover-html: cover  ## 生成 HTML 覆盖率报告并打开
	$(GO) tool cover -html=$(COVERAGE)

bench:  ## 跑 benchmark
	$(GO) test -bench=. -benchmem -run=^$$ ./...

# ─── 规则开发 ─────────────────────────────────────────────────

new-rule:  ## 创建新规则: make new-rule NAME=my_rule [SEVERITY=critical]
	@if [ -z "$(NAME)" ]; then echo "ERROR: NAME required, e.g. make new-rule NAME=my_rule"; exit 1; fi
	bash scripts/new-rule.sh $(NAME) $(if $(SEVERITY),--severity=$(SEVERITY),)

list-rules:  ## 列出全部已注册 rules
	@ls $(CATALOG_DIR)/*.star | xargs -n1 basename | sed 's/\.star//'

test-rule:  ## 本地跑一条规则: make test-rule NAME=duplicate_charge [FIXTURE=happy]
	@if [ -z "$(NAME)" ]; then echo "ERROR: NAME required"; exit 1; fi
	$(BUILD_DIR)/recon-cli rule test \
		--script=$(CATALOG_DIR)/$(NAME).star \
		$(if $(FIXTURE),--fixture=$(FIXTURE_DIR)/$(NAME)_$(FIXTURE).yaml,--fixture=$(FIXTURE_DIR)/$(NAME)_happy.yaml)

test-rules:  ## 把全部规则跑一遍 happy + edge fixture
	$(BUILD_DIR)/recon-cli rule test-all --catalog=$(CATALOG_DIR) --fixtures=$(FIXTURE_DIR)

lint-rules:  ## 对全部 .star 规则跑 linter (语法 + symbol 检查)
	$(BUILD_DIR)/recon-cli rule lint --catalog=$(CATALOG_DIR)

watch-rules:  ## 监 .star 变更,改了自动 POST 到 admin reload
	bash scripts/watch-rules.sh $(CATALOG_DIR) $(ADMIN_URL)

# ─── 规则集迁移 (catalog import/export) ───────────────────────

catalog-export:  ## 从 ADMIN_URL dump 全部规则到 catalog-export.yaml
	$(BUILD_DIR)/recon-cli catalog export \
		--admin=$(ADMIN_URL) \
		--out=catalog-export-$$(date +%Y%m%d-%H%M%S).yaml

catalog-import:  ## 把 catalog YAML 导入 ADMIN_URL: make catalog-import FILE=xxx.yaml
	@if [ -z "$(FILE)" ]; then echo "ERROR: FILE required"; exit 1; fi
	$(BUILD_DIR)/recon-cli catalog import --admin=$(ADMIN_URL) --in=$(FILE)

catalog-diff:  ## 对比本地 catalog 与 ADMIN 上的规则集
	$(BUILD_DIR)/recon-cli catalog diff --admin=$(ADMIN_URL) --local=$(CATALOG_DIR)

# ─── fixture ──────────────────────────────────────────────────

fixture-gen:  ## 从 prod dump 生成 fixture: make fixture-gen RULE=duplicate_charge HOURS=24
	@if [ -z "$(RULE)" ]; then echo "ERROR: RULE required"; exit 1; fi
	$(BUILD_DIR)/recon-cli fixture gen \
		--rule=$(RULE) \
		--hours=$(or $(HOURS),24) \
		--admin=$(ADMIN_URL) \
		--out=$(FIXTURE_DIR)/$(RULE)_replay_$$(date +%Y%m%d).yaml

# ─── 清理 ─────────────────────────────────────────────────────

clean:  ## 清构建产物
	rm -rf $(BUILD_DIR) $(COVERAGE) /tmp/recon-*.out

# ─── 一站式 ───────────────────────────────────────────────────

dev: build watch-rules  ## 一键 dev (构建 cli + 启动 watch)

ci: deps check test cover  ## CI 完整流程

all: clean deps check test build  ## 全量重建
