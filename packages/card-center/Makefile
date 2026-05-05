# card-center Makefile
#
# 工具要求：
#   protoc >= 3.20
#   protoc-gen-go >= 1.32      go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   protoc-gen-go-grpc >= 1.3   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

PROTO_DIR := api/proto/cardcenter/v1
PROTO_FILES := $(wildcard $(PROTO_DIR)/*.proto)

.PHONY: proto build test clean run-dev

proto:
	@echo "Generating gRPC stubs from $(PROTO_FILES)"
	protoc \
	  --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  $(PROTO_FILES)

build: proto
	go build ./...

test: proto
	go test ./... -race -cover

clean:
	rm -f $(PROTO_DIR)/*.pb.go

run-dev:
	CARDCENTER_LOG_DEV=true go run ./cmd/server
