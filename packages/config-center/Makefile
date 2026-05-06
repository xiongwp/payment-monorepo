# config-center Makefile

.PHONY: all build proto test docker run tidy

GO ?= go
DOCKER ?= docker
IMAGE ?= config-center:dev

all: build

build:
	$(GO) build -trimpath -ldflags="-s -w" -o bin/config-center ./cmd/server

# proto 生成（需 protoc + protoc-gen-go + protoc-gen-go-grpc）。
# v1.0 SDK 走 HTTP；proto 留给后续 gRPC handler 接入。
proto:
	@if ! command -v protoc >/dev/null 2>&1; then echo "protoc not installed; skip"; exit 0; fi
	protoc --proto_path=api/proto \
	  --go_out=api/proto --go_opt=paths=source_relative \
	  --go-grpc_out=api/proto --go-grpc_opt=paths=source_relative \
	  api/proto/configcenter/v1/configcenter.proto

test:
	$(GO) test ./... -count=1

tidy:
	$(GO) mod tidy

docker:
	$(DOCKER) build \
	  --build-context payment-util=../payment-util \
	  --build-context user-merchant-core-src=../user-merchant-core \
	  --build-context kms-manage-src=../kms-manage \
	  -t $(IMAGE) .

run:
	$(GO) run ./cmd/server
