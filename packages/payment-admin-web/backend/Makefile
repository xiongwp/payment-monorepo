.PHONY: build test run tidy sync-assets

GO ?= go

# MF-2: build 前自动 sync moneyflow-designer.html 到 assets/ 给 go:embed 用.
build: sync-assets
	$(GO) build ./...

test: sync-assets
	$(GO) test ./...

run: sync-assets
	$(GO) run ./cmd/server

tidy:
	$(GO) mod tidy

# sync-assets — 把 frontend HTML 复制到 backend/internal/handler/assets/.
# go:embed 路径只能在 package 目录下, 所以 frontend/ 是源, assets/ 是 build-time 副本.
sync-assets:
	@mkdir -p internal/handler/assets
	@cp -f ../frontend/moneyflow-designer.html     internal/handler/assets/moneyflow-designer.html
	@cp -f ../frontend/moneyflow-designer-v2.html  internal/handler/assets/moneyflow-designer-v2.html
	@cp -f ../frontend/moneyflow-resources.html    internal/handler/assets/moneyflow-resources.html
	@echo "synced moneyflow-{designer,designer-v2,resources}.html → internal/handler/assets/"
