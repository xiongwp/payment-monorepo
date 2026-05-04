.PHONY: build test tidy proto docker run

GO ?= go

build:
	$(GO) build ./...

test:
	$(GO) test ./...

tidy:
	$(GO) mod tidy

proto:
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		-I . api/proto/kms/v1/kms.proto

docker:
	docker build -t kms-manage:local .

run:
	KMS_LOG_DEV=true $(GO) run ./cmd/server
