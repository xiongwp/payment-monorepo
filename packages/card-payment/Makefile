PROTO_DIR := api/proto/cardpayment/v1
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
	CARDPAYMENT_LOG_DEV=true go run ./cmd/server
