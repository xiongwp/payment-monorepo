#!/bin/sh
# ─── 批处理任务包装脚本 ──────────────────────────────────────────────────────────
# 由 crond 调用，从环境变量读取 grpc_addr（Docker 注入），传递给 binary。
# 使用方式：run-batchtask.sh <task-type> [额外参数...]
# 示例：run-batchtask.sh process_async_tasks --batch-size=100
#
# 注：crond 不继承 Docker 的 ENV，需通过 /etc/environment 注入（由 entrypoint 写入）。
# ─────────────────────────────────────────────────────────────────────────────
set -e

# 从 /etc/environment 读取环境变量（entrypoint.sh 在启动 crond 前写入）
[ -f /etc/environment ] && . /etc/environment

TASK_TYPE="${1:-}"
shift || true

exec /app/accounting-batchtask \
    --run-once \
    --task-type="${TASK_TYPE}" \
    "$@"
