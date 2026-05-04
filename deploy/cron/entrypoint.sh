#!/bin/sh
# ─── accounting-batchtask 容器入口 ───────────────────────────────────────────
# 将 Docker ENV 写入 /etc/environment 供 crond 子进程继承，然后启动 crond。
# ─────────────────────────────────────────────────────────────────────────────
set -e

# 将关键环境变量持久化，crond 调用子进程时会 source /etc/environment
{
  echo "GRPC_ADDR=${GRPC_ADDR:-accounting-service:50051}"
  echo "TZ=${TZ:-Asia/Shanghai}"
} > /etc/environment

echo "[batchtask-entrypoint] grpc_addr=${GRPC_ADDR:-accounting-service:50051}, starting crond..."

# 前台运行 crond（-f），日志输出到 stdout（-d 8 = debug level）
exec /usr/sbin/crond -f -d 8
