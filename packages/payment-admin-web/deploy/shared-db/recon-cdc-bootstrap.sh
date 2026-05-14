#!/usr/bin/env sh
# recon-cdc-bootstrap.sh — 启栈时强制保证每个 shared-shard 的 recon_cdc 用户存在 + 密码对.
#
# 背景:
#   /docker-entrypoint-initdb.d/*.sql 只在 MySQL data dir 为空时跑一次.
#   开发联栈反复 `docker compose up/down` 时 volume 持久化 → init 不再执行
#   → recon_cdc 用户可能不存在或密码漂移,canal 报 1045 access denied.
#
# 方案:
#   独立的 oneshot service, 启栈时跑一次, 显式 ALTER USER + GRANT.
#   只需 mysql client (镜像 mysql:8.0 自带), 用 MYSQL_ROOT_PASSWORD 连每个 shard.
#
# 安全:
#   - 仅 dev 联栈用 (root pwd 走 env).
#   - 失败仅 log,不阻塞栈启动 (canal 起来后会重试 dial,bootstrap 后下一轮就成功).
#
# 用法:
#   docker run --rm --network payment-stack \
#     -e MYSQL_ROOT_PASSWORD=xxx -e RECON_CDC_PASS=recon_cdc_pwd \
#     -v $(pwd)/recon-cdc-bootstrap.sh:/bootstrap.sh:ro \
#     mysql:8.0 sh /bootstrap.sh

set -u

ROOT_PWD="${MYSQL_ROOT_PASSWORD:-root}"
CDC_USER="${RECON_CDC_USER:-recon_cdc}"
CDC_PWD="${RECON_CDC_PASS:-recon_cdc_pwd}"
# 等 MySQL 完全 ready 的总超时 (sec)
WAIT_TIMEOUT="${BOOTSTRAP_WAIT_TIMEOUT:-120}"

SHARDS="shared-meta shared-shard-0 shared-shard-1 shared-shard-2 shared-shard-3 shared-shard-4 shared-shard-5 shared-shard-6 shared-shard-7 shared-shard-8 shared-shard-9"

echo "[recon-cdc-bootstrap] start: user=${CDC_USER} shards=${SHARDS}"

ensure_user() {
  host="$1"
  # mysql --connect-timeout 跑探活, 失败 retry 10s 间隔, 总不超 WAIT_TIMEOUT
  end=$(( $(date +%s) + WAIT_TIMEOUT ))
  while [ "$(date +%s)" -lt "$end" ]; do
    if mysql --connect-timeout=3 -h "$host" -uroot -p"$ROOT_PWD" -e "SELECT 1" >/dev/null 2>&1; then
      break
    fi
    sleep 3
  done
  if ! mysql --connect-timeout=3 -h "$host" -uroot -p"$ROOT_PWD" -e "SELECT 1" >/dev/null 2>&1; then
    echo "[recon-cdc-bootstrap] WARN: ${host} unreachable, skipping"
    return 1
  fi
  mysql -h "$host" -uroot -p"$ROOT_PWD" <<SQL >/dev/null 2>&1
CREATE USER IF NOT EXISTS '${CDC_USER}'@'%' IDENTIFIED WITH mysql_native_password BY '${CDC_PWD}';
ALTER USER '${CDC_USER}'@'%' IDENTIFIED WITH mysql_native_password BY '${CDC_PWD}';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '${CDC_USER}'@'%';
GRANT SELECT ON *.* TO '${CDC_USER}'@'%';
FLUSH PRIVILEGES;
SQL
  if [ $? -eq 0 ]; then
    echo "[recon-cdc-bootstrap] OK: ${host} (${CDC_USER} ensured)"
  else
    echo "[recon-cdc-bootstrap] WARN: ${host} grant failed (continuing)"
  fi
}

for h in $SHARDS; do
  ensure_user "$h"
done

echo "[recon-cdc-bootstrap] done"
exit 0
