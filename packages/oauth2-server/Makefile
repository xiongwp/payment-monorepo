.PHONY: help build run test tidy vendor image image-push clean smoke loadtest schema

BINARY = bin/oauth2-server
IMAGE  = oauth2-server:local

help: ## 显示命令列表
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## 编译二进制
	@mkdir -p bin
	@CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "built: $(BINARY)"

run: build ## 本地运行 (dev 模式: in-memory + seed)
	@OAUTH2_DEV_SEED=1 OAUTH2_ADMIN_TOKEN=admintok ./$(BINARY)

test: ## 单元测试
	@go test -race ./...

tidy: ## go mod tidy (GOWORK=off — 不让 monorepo go.work 干扰)
	@GOWORK=off go mod tidy

vendor: tidy ## vendor 依赖 (Dockerfile 用; GOWORK=off 必须, 否则 modules.txt 缺 explicit 标记)
	@rm -rf vendor
	@GOWORK=off go mod vendor
	@echo "✓ vendor/ regenerated ($(shell ls vendor 2>/dev/null | wc -l | tr -d ' ') dirs)"

image: vendor ## docker build
	@docker build -t $(IMAGE) .
	@echo "built: $(IMAGE)"

image-push: image ## docker push (要先 docker login)
	@docker push $(IMAGE)

smoke: ## e2e 烟雾测试 (假设 server 在 :8087)
	@bash test/smoke.sh http://localhost:8087

loadtest: ## 压测 (需 vegeta / hey 装)
	@bash test/loadtest.sh

schema: ## 生成完整 100 表 schema SQL
	@bash database/init/generate.sh > database/init/02_schema_full.sql
	@wc -l database/init/02_schema_full.sql

clean: ## 清产物
	@rm -rf bin/ vendor/ database/init/02_schema_full.sql
