#!/bin/sh
set -e

# gRPC backend address for the Go HTTP server
export GRPC_ADDR="${GRPC_ADDR:-accounting-system:50051}"
export PORT="${PORT:-9090}"

echo "[entrypoint] Starting Go API server (gRPC backend: ${GRPC_ADDR})"

# Start Go HTTP backend in background
GRPC_ADDR="${GRPC_ADDR}" PORT="${PORT}" /usr/local/bin/api-server &

# Validate nginx config
nginx -t

echo "[entrypoint] Starting nginx"
exec "$@"
