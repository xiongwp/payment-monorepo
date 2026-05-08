#!/usr/bin/env bash

# ─── 支付平台全栈 - 部署脚本 ─────────────────────────────────────────────
# 启动顺序：
#   1. payment-stack 共享 docker network
#   2. kms-manage         (需要先 init-kms 产一把 master key)
#   3. shared-db          (11 MySQL：装 paychan/order/user_merchant 三套 schema)
#   4. risk-manage        (无状态)
#   5. payment-channel    (连 shared-db + kms)
#   6. order-core         (连 shared-db + payment-core)
#   7. user-merchant-core (连 shared-db + kms)
#   8. payment-core       (无状态，连 payment-channel + kms-manage + risk-manage)
#   9. payment-admin-web  (前端 + Go BFF，连 order/payment-core/kms/risk/user-merchant)
#
# 用法：
#   ./deploy.sh init-kms         # 首次部署前产一把 kms master key
#   ./deploy.sh up               # 拉起全栈
#   ./deploy.sh up payment-core  # 只起某个子集
#   ./deploy.sh down             # 全部清理（保留数据卷）
#   ./deploy.sh down --volumes   # 连数据卷一起清
#   ./deploy.sh status           # 看各服务 ps
#   ./deploy.sh logs <svc>       # 追 <svc> 的日志
#   ./deploy.sh check            # 对每个 gRPC 端口发 tcp 探活
#
# 约定目录：
#   <repo-root>/
#     ├── order-core/
#     ├── payment-channel/
#     ├── payment-core/
#     ├── kms-manage/
#     └── payment-admin-web/
#         ├── deploy.sh         （本脚本）
#         ├── deploy/
#         │   ├── kms-keys/     （生成后的 keystore）
#         │   └── overrides/    （每个服务的 network + env 合并文件）
set -euo pipefail

# ─── colors ─────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; NC='\033[0m'
info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
ok()      { echo -e "${GREEN}[OK]${NC}    $*"; }
fatal()   { echo -e "${RED}[FATAL]${NC} $*" >&2; exit 1; }

# ─── paths ──────────────────────────────────────────────────────
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
OVR="$HERE/deploy/overrides"
KEYS_DIR="$HERE/deploy/kms-keys"

# svc 名称 → 该 svc 的 compose files 组合
compose_files() {
  # 给每个 service 一个独立的 -p project name，否则多个 override 文件都在
  # deploy/overrides/ 下，compose 默认 project name 都会取目录 basename "overrides"，
  # 导致 up 一个 service 时  / 重建会把同 project 别的 service 容器
  # 当成孤儿删掉（risk-stack 起来 → up accounting-system 时 risk-stack 全没了）。
  case "$1" in
    shared-db)            echo "-p shared-db          -f $HERE/deploy/shared-db/docker-compose.yml" ;;
    config-center)        echo "-p config-center      -f $ROOT/config-center/docker-compose.yml" ;;
    kms-manage)           echo "-p kms-manage         -f $ROOT/kms-manage/docker-compose.yml          -f $OVR/kms-manage.yml" ;;
    # risk-stack: risk-manage 依赖 (Redis + Kafka + ClickHouse + Nebula +
    # Prometheus + Grafana + etcd)。在 risk-manage 之前起。
    risk-stack)           echo "-p risk-stack         -f $OVR/risk-stack.yml" ;;
    risk-manage)          echo "-p risk-manage        -f $ROOT/risk-manage/docker-compose.yml         -f $OVR/risk-manage.yml" ;;
    # 联栈下不再用 accounting-system/docker-compose.yml（它会另起 11 个 MySQL +
    # redis + kafka + zk + etcd 全套副本）。这里的 override 自包含定义
    # accounting-service + accounting-batchtask，直接复用 shared-db (1 meta + 10 shard) +
    # risk-stack (risk-redis + etcd)。
    accounting-system)    echo "-p accounting-system  -f $OVR/accounting-system.yml" ;;
    payment-channel)      echo "-p payment-channel    -f $ROOT/payment-channel/docker-compose.yml     -f $OVR/payment-channel.yml" ;;
    order-core)           echo "-p order-core         -f $ROOT/order-core/docker-compose.yml          -f $OVR/order-core.yml" ;;
    user-merchant-core)   echo "-p user-merchant-core -f $ROOT/user-merchant-core/docker-compose.yml  -f $OVR/user-merchant-core.yml" ;;
    payment-core)         echo "-p payment-core       -f $ROOT/payment-core/docker-compose.yml        -f $OVR/payment-core.yml" ;;
    api-gateway)          echo "-p api-gateway        -f $ROOT/api-gateway/docker-compose.yml         -f $OVR/api-gateway.yml" ;;
    # PCI 卡支付（联栈 dev 自包含 override，不引 base compose 的独立 MySQL fleet）。
    # 生产 SAQ-D 部署用 ../../<svc>/docker-compose.yml 自带的独立栈。
    card-center)          echo "-p card-center        -f $OVR/card-center.yml" ;;
    card-payment)         echo "-p card-payment       -f $OVR/card-payment.yml" ;;
    accounting-admin-web) echo "-p accounting-admin-web -f $ROOT/accounting-admin-web/docker-compose.yml -f $OVR/accounting-admin-web.yml" ;;
    payment-admin-web)    echo "-p payment-admin-web  -f $HERE/docker-compose.yml                     -f $OVR/payment-admin-web.yml" ;;
    *) fatal "unknown service: $1" ;;
  esac
}

# 联栈模式下：shared-db 先起（11 MySQL），再按依赖顺序起应用容器：
#   kms-manage / risk-manage     无下游
#   payment-channel / order-core / user-merchant-core   依赖 kms + db
#   payment-core                依赖 channel + kms + risk
#   api-gateway                 依赖 user-merchant-core + risk-manage + kms（用户面 BFF）
#   payment-admin-web           依赖前面所有（运营 admin BFF）
# 启动顺序：
#   shared-db          1 meta + 10 shard MySQL（含 paychan/order/user-merchant/accounting 全部 schema）
#   kms-manage         无下游
#   risk-stack         risk-redis + risk-kafka + clickhouse + nebula + 监控（accounting 复用 risk-redis）
#   accounting-system  accounting-service + accounting-batchtask（复用 shared-db + risk-redis）
#   risk-manage / payment-channel / order-core / user-merchant-core / payment-core / api-gateway / *-admin-web
ALL_SERVICES=(shared-db config-center kms-manage risk-stack accounting-system risk-manage payment-channel order-core user-merchant-core payment-core card-center card-payment api-gateway accounting-admin-web payment-admin-web)

# scale_args_of 返回 --scale a=N --scale b=M ... 用来起多副本。前提：override
# 文件里该 service 没有 container_name，端口用 range，否则会撞名 / 撞端口。
# DEPLOY_REPLICAS_<SVC> 环境变量可以在脚本外覆盖默认值（如 DEPLOY_REPLICAS_ACCOUNTING_SERVICE=3）。
scale_args_of() {
  # 默认每个服务 2 副本；DEPLOY_REPLICAS_<UPPER_SVC>=N 临时覆盖。
  # accounting-batchtask 例外：cron runner 用 leader election 卡单 pod 跑任务，
  # 物理上可以多副本但只有 leader 真的工作。这里也给 2 验证 leader 切换。
  case "$1" in
    accounting-system)
      local n="${DEPLOY_REPLICAS_ACCOUNTING_SERVICE:-2}"
      local b="${DEPLOY_REPLICAS_ACCOUNTING_BATCHTASK:-2}"
      echo "--scale accounting-service=$n --scale accounting-batchtask=$b"
      ;;
    payment-core)
      local n="${DEPLOY_REPLICAS_PAYMENT_CORE:-2}"
      echo "--scale payment-core=$n"
      ;;
    order-core)
      local n="${DEPLOY_REPLICAS_ORDER_CORE:-2}"
      echo "--scale order-core=$n"
      ;;
    payment-channel)
      # 钉死 1 副本：容器端口 gRPC 9092 / metrics 9093 相邻，多副本会撞宿主端口。
      # 要扩到多副本需先在应用层把 metrics 从 9093 挪到 19093。
      local n="${DEPLOY_REPLICAS_PAYMENT_CHANNEL:-1}"
      echo "--scale payment-channel=$n"
      ;;
    user-merchant-core)
      local n="${DEPLOY_REPLICAS_USER_MERCHANT_CORE:-2}"
      echo "--scale user-merchant-core=$n"
      ;;
    risk-manage)
      local n="${DEPLOY_REPLICAS_RISK_MANAGE:-2}"
      echo "--scale risk-manage=$n"
      ;;
    kms-manage)
      local n="${DEPLOY_REPLICAS_KMS_MANAGE:-2}"
      echo "--scale kms=$n"
      ;;
    *)
      echo ""
      ;;
  esac
}

# 每个 per-repo compose 里哪个 service 是"真应用"（需要被 up 的唯一一个）
app_service_of() {
  case "$1" in
    shared-db)          echo "" ;;   # 整个 compose 都是 MySQL，不用限定
    kms-manage)         echo "kms" ;;
    risk-stack)         echo "" ;;   # 整个 stack 都要起
    risk-manage)        echo "risk-manage" ;;
    # 联栈下只起 accounting-service + accounting-batchtask；
    # MySQL/Redis 都复用 shared-db / risk-stack，不再每服务一套。
    accounting-system)  echo "accounting-service accounting-batchtask" ;;
    # mockserver 是 payment-channel 的开发期外部渠道模拟器，跟它一起起。
    # base compose 里 payment-channel 原本 depends_on mockserver；联栈下我们清了
    # depends_on（避开 11 MySQL），所以这里显式列上 mockserver 让它一同 up。
    payment-channel)    echo "payment-channel mockserver" ;;
    order-core)         echo "order-core" ;;
    user-merchant-core) echo "user-merchant-core" ;;
    payment-core)       echo "payment-core" ;;
    api-gateway)        echo "api-gateway" ;;
    card-center)        echo "card-center" ;;
    card-payment)       echo "card-payment" ;;
    accounting-admin-web) echo "accounting-admin-web" ;;
    payment-admin-web)  echo "" ;;   # 两个 app 都要起
    *) echo "" ;;
  esac
}

COMPOSE="docker compose"
docker compose version >/dev/null 2>&1 || COMPOSE="docker-compose"

# 供 override 里的 volumes 用（容器内 config 替换）
export KMS_KEYS_DIR="$KEYS_DIR"
export PAYCHAN_DOCKER_CONFIG="$ROOT/payment-channel/config/config.docker.yaml"
export ORDER_DOCKER_CONFIG="$ROOT/order-core/config/config.docker.yaml"
export USER_MERCHANT_DOCKER_CONFIG="$ROOT/user-merchant-core/config/config.docker.yaml"

ensure_network() {
  if ! docker network inspect payment-stack >/dev/null 2>&1; then
    info "创建 docker network: payment-stack"
    docker network create --driver bridge payment-stack >/dev/null
  fi
  # accounting-system 自带 accounting-network；attach 用 (admin-web override)
  if ! docker network inspect accounting-network >/dev/null 2>&1; then
    info "创建 docker network: accounting-network (accounting-system 内部用)"
    docker network create --driver bridge accounting-network >/dev/null
  fi
}

ensure_kms_keys() {
  if [[ ! -f "$KEYS_DIR/ACTIVE" ]] || ! ls "$KEYS_DIR"/*.key >/dev/null 2>&1; then
    fatal "KMS master key 还没生成，请先跑: ./deploy.sh init-kms"
  fi
}

cmd_init_kms() {
  mkdir -p "$KEYS_DIR"
  if [[ -f "$KEYS_DIR/ACTIVE" ]]; then
    warn "${KEYS_DIR}/ACTIVE 已存在；跳过（如需重置请手动 rm -rf ${KEYS_DIR}）"
    return 0
  fi
  info "用 kmsctl 容器生成 256-bit master key → $KEYS_DIR/main.key"
  # 临时起一个 kms-manage 镜像只跑 kmsctl init-key
  (cd "$ROOT/kms-manage" && $COMPOSE build kms >/dev/null)
  docker run --rm \
    -v "$KEYS_DIR":/var/lib/kms-manage/keys \
    --entrypoint /usr/local/bin/kmsctl \
    kms-manage:local init-key /var/lib/kms-manage/keys main
  ok "KMS keystore 初始化完成: $KEYS_DIR"
}

ensure_card_dbs() {
  # 幂等：CREATE * IF NOT EXISTS。
  #
  # MySQL 的 /docker-entrypoint-initdb.d/ 只在数据卷**首次创建**时跑一次；
  # 老 shared-db volume（card-center / card-payment 还没加进来时建的）
  # 不会自动补 card_* 的库。这里在 shared-db healthy 之后无条件 exec 进每个
  # MySQL 重灌一遍 card-center / card-payment 的 init SQL，缺失才会建出来，
  # 已经存在则全部 IF NOT EXISTS 跳过 → 重跑无害。
  local cc_meta="$ROOT/card-center/database/metadb/init/init.sql"
  local cp_meta="$ROOT/card-payment/database/metadb/init/init.sql"

  if [[ ! -s "$cc_meta" && ! -s "$cp_meta" ]]; then
    return 0
  fi

  info "  ensure card_center / card_payment databases on shared-db"

  if [[ -s "$cc_meta" ]]; then
    docker exec -i shared-meta mysql -uroot -ppassword < "$cc_meta" 2>/dev/null \
      || warn "    apply card-center meta failed"
  fi
  if [[ -s "$cp_meta" ]]; then
    docker exec -i shared-meta mysql -uroot -ppassword < "$cp_meta" 2>/dev/null \
      || warn "    apply card-payment meta failed"
  fi

  for i in 0 1 2 3 4 5 6 7 8 9; do
    local cc_shard="$ROOT/card-center/database/userdb/init/${i}_init.sql"
    local cp_shard="$ROOT/card-payment/database/cardpaymentdb/init/${i}_init.sql"
    if [[ -s "$cc_shard" ]]; then
      docker exec -i "shared-shard-$i" mysql -uroot -ppassword < "$cc_shard" 2>/dev/null \
        || warn "    apply card-center shard $i failed"
    fi
    if [[ -s "$cp_shard" ]]; then
      docker exec -i "shared-shard-$i" mysql -uroot -ppassword < "$cp_shard" 2>/dev/null \
        || warn "    apply card-payment shard $i failed"
    fi
  done

  # 验证
  local got
  got=$(docker exec shared-meta mysql -uroot -ppassword -N -e \
        "SHOW DATABASES LIKE 'card_%'" 2>/dev/null | tr '\n' ' ')
  ok "  card meta DBs: ${got:-<none>}"
  got=$(docker exec shared-shard-0 mysql -uroot -ppassword -N -e \
        "SHOW DATABASES LIKE 'card_%'" 2>/dev/null | tr '\n' ' ')
  ok "  card shard-0 DBs: ${got:-<none>}"
}

verify_shared_dbs() {
  # shared-shard-0 应该同时有 paychan_db_0 / order_db_0 / accounting_db_0 /
  # user_merchant_db_0（user-merchant-core 已分库后）。
  local container="shared-shard-0"
  local got
  got=$(docker exec "$container" mysql -uroot -ppassword -N -e \
        "SHOW DATABASES LIKE '%_db_%'" 2>/dev/null | sort -u | tr '\n' ' ')
  for need in paychan_db_0 order_db_0 accounting_db_0 user_merchant_db_0; do
    if [[ "$got" != *"$need"* ]]; then
      warn "${container} 缺 ${need}；init SQL 加载失败"
      warn "  当前库: ${got}"
      return 1
    fi
  done
  ok "  ${container}: ${got}"

  # shared-meta 应该有 paychan_meta + order_meta + user_merchant_meta + account_meta。
  local meta_got
  meta_got=$(docker exec shared-meta mysql -uroot -ppassword -N -e \
        "SHOW DATABASES" 2>/dev/null | sort -u | tr '\n' ' ')
  if [[ "$meta_got" != *"user_merchant_meta"* \
      || "$meta_got" != *"order_meta"* \
      || "$meta_got" != *"paychan_meta"* \
      || "$meta_got" != *"account_meta"* ]]; then
    warn "shared-meta 缺某个 meta 库；init SQL 加载失败"
    warn "  当前库: $meta_got"
    return 1
  fi
  ok "  shared-meta: $meta_got"

  # 注：分库分表后 merchants 已挪到 user_merchant_db_0..9 的 merchants_NN 分片表，
  # 不再在 user_merchant_meta 里。templates/schema.sql 已经内置 deleted_at + 索引，
  # 旧 schema 自愈 ALTER 步骤连带删除（meta.merchants 已不存在，原 ALTER 必报 1146）。
}

ensure_order_init_sql() {
  # order-core 的 shard init 已 commit 在 git 里；共享栈模式下由
  # generate-shared-init.sh 合并进 deploy/shared-db/init/ 一起用，
  # 这里只做存在性兜底（如果被 Docker 弄坏了就 git checkout 恢复）。
  local dir="$ROOT/order-core/database/orderdb/init"
  for i in 0 1 2 3 4 5 6 7 8 9; do
    local f="$dir/${i}_init.sql"
    if [[ -d "$f" || ! -s "$f" ]]; then
      warn "$f 损坏；尝试 git checkout 恢复"
      (cd "$ROOT/order-core" && rm -rf database/orderdb/init && git checkout database/orderdb/init) \
        || fatal "order-core init SQL 恢复失败，请手动 git checkout"
      [[ -s "$f" ]] || fatal "$f 仍缺失"
    fi
  done
}

ensure_shared_init_sql() {
  # 每次都重新生成。合并三仓 SQL 就几个 cat，零成本，省得担心陈旧。
  local dir="$HERE/deploy/shared-db/init"
  rm -rf "$dir"
  info "生成共享 MySQL init SQL（paychan + order + user-merchant）"
  bash "$HERE/deploy/shared-db/generate-shared-init.sh" >/dev/null
  grep -q 'user_merchant_meta' "$dir/meta_init.sql" \
    || fatal "generate-shared-init.sh 漏拼 user-merchant-core 段"
}

# ensure_card_dbs 幂等补灌 card_center / card_payment 数据库 ——
# shared-meta + shared-shard-0..9 容器首次启动时会跑 init SQL，但已经在跑的容器
# 不会重 source，所以新加 card_* 服务时容器侧 mysql 没这些 db。
# 调度规则：
#   - up 包含 card-center 或 card-payment 时调一次（CREATE IF NOT EXISTS 没副作用）
#   - shared-db 没起的话静默跳过（cmd_up 会先起 shared-db 再回头）
ensure_card_dbs() {
  local need_card=false
  for t in "$@"; do
    case "$t" in card-center|card-payment) need_card=true ;; esac
  done
  [[ "$need_card" == "false" ]] && return 0
  # shared-meta 没起？跳过 —— cmd_up 流程会先把 shared-db 起来再回头时再调
  if ! docker ps --format '{{.Names}}' | grep -qx 'shared-meta'; then
    return 0
  fi
  info "ensure_card_dbs: 幂等补灌 card_center_* / card_payment_* DB"
  docker exec -i shared-meta mysql -uroot -ppassword <<'SQL_META' >/dev/null 2>&1
CREATE DATABASE IF NOT EXISTS card_center_meta  DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE DATABASE IF NOT EXISTS card_payment_meta DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
SQL_META
  for i in 0 1 2 3 4 5 6 7 8 9; do
    docker exec -i "shared-shard-$i" mysql -uroot -ppassword <<SQL_SHARD >/dev/null 2>&1
CREATE DATABASE IF NOT EXISTS \`card_center_db_$i\`  DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
CREATE DATABASE IF NOT EXISTS \`card_payment_db_$i\` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
SQL_SHARD
  done
}

cmd_up() {
  ensure_network
  ensure_kms_keys
  ensure_order_init_sql
  ensure_shared_init_sql

  local targets=("$@")
  [[ ${#targets[@]} -eq 0 ]] && targets=("${ALL_SERVICES[@]}")

  # 幂等补灌 card_*_db / card_*_meta —— 只在 targets 含 card-center/card-payment
  # 且 shared-meta 已经在跑（即 shared-db 是先于 card-* 起来的，或本次 up 顺序
  # 就把 shared-db 排前）时才执行；早调一次没坏处。
  ensure_card_dbs "${targets[@]}"

  # 不论用户传啥 service，shared-db + risk-stack + config-center 是所有 app 的
  # 基础设施。没起来 app 会连不上 MySQL / Redis / Kafka / etcd / 全平台动态配置。
  # 用户显式只传基础设施本身时不再前置（避免无限循环）。
  ensure_infra_first() {
    local infra=("shared-db" "risk-stack" "config-center")
    local need_prepend=false
    for t in "${targets[@]}"; do
      case "$t" in
        shared-db|risk-stack|config-center) ;;
        *) need_prepend=true ;;
      esac
    done
    [[ "$need_prepend" == "false" ]] && return
    local final=()
    for i in "${infra[@]}"; do
      local skip=false
      for t in "${targets[@]}"; do
        [[ "$t" == "$i" ]] && skip=true
      done
      [[ "$skip" == "false" ]] && final+=("$i")
    done
    final+=("${targets[@]}")
    targets=("${final[@]}")
    info "自动前置基础设施: ${infra[*]}（不在你的 targets 里，但 app service 需要它们）"
  }
  ensure_infra_first

  for svc in "${targets[@]}"; do
    info "启动 $svc …"
    # ── pre-up 钩子：先把 DB 准备好，再起依赖 DB 的服务 ──
    case "$svc" in
      card-center|card-payment)
        # shared-db 在 ALL_SERVICES 里更靠前，已经先 up 过；这里幂等补灌一次
        # card_* 库，确保 card-center / card-payment 容器一启动就连得上 DB。
        ensure_card_dbs
        ;;
    esac
    local app
    app="$(app_service_of "$svc")"
    # 多副本：app_service_of 之外的 service 默认 1 副本，scale_of_app 返回的
    # 服务用 --scale name=N 起多副本（前提：override 已去 container_name + 用端口 range）。
    local scale_args
    scale_args="$(scale_args_of "$svc")"
    # shellcheck disable=SC2086
    # --force-recreate：override 文件里 volumes / env 有变化时保证容器重建。
    # 对 per-repo compose 只指定真应用 service 名，避免顺带把 11 个 MySQL 也拉起来。
    if [[ -n "$app" ]]; then
      # 有意不加引号：app 可能是 "svc1 svc2"（如 accounting-service accounting-batchtask），
      # 需要 word splitting 让 compose 收到多个 service 参数。
      # ：override 文件改名 / scale 改了之后清掉历史孤儿容器，
      # 避免 "WARN: orphan containers" 一直刷屏。
      # shellcheck disable=SC2086
      $COMPOSE $(compose_files "$svc") up -d --build --force-recreate  $scale_args $app
    else
      # shellcheck disable=SC2086
      $COMPOSE $(compose_files "$svc") up -d --build --force-recreate  $scale_args
    fi
    case "$svc" in
      shared-db)
        info "  等共享 MySQL 栈 healthy（最多 180s）"
        local deadline=$((SECONDS + 180))
        until [[ $(docker ps --filter "name=^shared-" --filter "health=healthy" --format '{{.Names}}' | wc -l) -ge 11 ]]; do
          (( SECONDS > deadline )) && { warn "等超时，继续（MySQL 可能还在初始化）"; break; }
          sleep 4
        done
        verify_shared_dbs
        # 补灌 card_* 库（如果之前 shared-db 已起、init 没含这俩，这里 idempotent 补建）
        ensure_card_dbs
        ;;
      config-center)
        info "  等 config-center /healthz（最多 60s）"
        local cc_deadline=$((SECONDS + 60))
        until curl -fs http://localhost:9691/healthz >/dev/null 2>&1; do
          (( SECONDS > cc_deadline )) && { warn "config-center healthz 超时"; break; }
          sleep 2
        done
        if curl -fs http://localhost:9691/healthz >/dev/null 2>&1; then
          # 自动 seed：写 12 namespace 默认 key（PUT 是幂等，重复跑无害）
          info "  自动 seed config-center 12 namespace 默认 key"
          bash "$ROOT/config-center/deploy.sh" seed 2>&1 | sed 's/^/    /' || warn "seed 失败（可手动跑 deploy.sh seed 重试）"
        fi
        ;;
    esac
    ok "$svc 启动完成"
  done

  echo
  ok "全栈启动完成。"
  echo "  - 管理后台 UI        →  http://localhost:8080"
  echo "  - admin BFF          →  http://localhost:19190/api/..."
  echo "  - api-gateway 用户面 →  http://localhost:18080/signup  /login  /me"
  echo "  - api-gateway admin  →  http://localhost:18081"
  echo "  - api-gateway metrics→  http://localhost:19092/metrics"
  echo "  - order-core         →  grpc  127.0.0.1:9091"
  echo "  - user-merchant-core →  grpc  127.0.0.1:9191  metrics 127.0.0.1:9291"
  echo "  - payment-core       →  grpc  127.0.0.1:9090"
  echo "  - payment-chan       →  grpc  127.0.0.1:9092 | webhook 127.0.0.1:9192"
  echo "  - kms-manage         →  grpc  127.0.0.1:9290"
  echo "  - risk-manage        →  grpc  127.0.0.1:9490"
  echo "  - shared MySQL       →  meta :3400 | shard0..9 :3410..3419（paychan_db_N + order_db_N + user_merchant_meta 同节点）"
}

cmd_down() {
  local vol_flag=""
  [[ "${1:-}" == "--volumes" ]] && vol_flag="-v"

  # 反向顺序，admin 最先停，shared-db 最后停
  for svc in payment-admin-web accounting-admin-web api-gateway payment-core user-merchant-core order-core payment-channel risk-manage accounting-system risk-stack kms-manage shared-db; do
    info "停止 $svc …"
    # shellcheck disable=SC2086
    $COMPOSE $(compose_files "$svc") down $vol_flag 2>/dev/null || true
  done

  if [[ -n "$vol_flag" ]]; then
    # compose down -v 只清 compose 文件里声明的 volumes；compose 项目名前缀
    # 在不同环境（不同 cwd / COMPOSE_PROJECT_NAME）会变。这里按"卷名后缀"
    # 模式硬删一遍，保证 MySQL 下次 up 会重跑 init SQL。
    info "强删残留 MySQL 数据卷（任意 project 前缀）"
    # 匹配名字以 _meta-data / _shard<N>-data 结尾的所有 volume
    local matched
    matched=$(docker volume ls --format '{{.Name}}' 2>/dev/null | \
              grep -E '(_meta-data$|_shard[0-9]-data$)' || true)
    if [[ -n "$matched" ]]; then
      while IFS= read -r v; do
        [[ -n "$v" ]] || continue
        info "  rm volume $v"
        docker volume rm -f "$v" >/dev/null 2>&1 || true
      done <<< "$matched"
    else
      info "  (无匹配 volume)"
    fi
    warn "数据卷已清；keystore (deploy/kms-keys) 仍保留，不受影响"
  fi
  ok "停止完成"
}

cmd_nuke() {
  # 核选项：停所有容器 + 删所有卷 + 删 docker network，让下次 up 像首次一样。
  warn "nuke 会删除所有栈容器 + 所有 MySQL 数据卷 + payment-stack network"
  cmd_down --volumes
  docker network rm payment-stack >/dev/null 2>&1 || true
  ok "nuke 完成；下次 up 会是全新环境"
}

cmd_status() {
  for svc in "${ALL_SERVICES[@]}"; do
    echo -e "${BLUE}=== $svc ===${NC}"
    # shellcheck disable=SC2086
    $COMPOSE $(compose_files "$svc") ps 2>/dev/null || true
  done
}

cmd_logs() {
  [[ $# -ge 1 ]] || fatal "用法: $0 logs <service>"
  # shellcheck disable=SC2086
  $COMPOSE $(compose_files "$1") logs -f "${@:2}"
}

cmd_seed() {
  # 把全平台动态配置写入 config-center（12 namespace 默认 key）。
  # 部署完业务栈后跑一次；admin 后续在 config-center admin web 统一管理。
  info "调 ../config-center/deploy.sh seed 写 12 namespace 默认 key..."
  bash "$ROOT/config-center/deploy.sh" seed
  ok "seed 完成；改 key 走 http://localhost:9691/admin/"
}

cmd_check() {
  local ports=(
    "kms-manage:9290"
    "risk-manage:9490"
    "payment-channel:9092"
    "payment-core:9090"
    "order-core:9091"
    "user-merchant-core:9191"
    "card-center:9443"
    "card-payment:9444"
    "api-gateway-public:18080"
    "api-gateway-admin:18081"
    "admin-backend:19190"
    "admin-web:8080"
  )
  for p in "${ports[@]}"; do
    local name=${p%:*} port=${p#*:}
    if nc -z -w 2 127.0.0.1 "$port" 2>/dev/null; then
      ok "$name :$port OK"
    else
      warn "$name :$port not listening"
    fi
  done
}

cmd_debug_mysql() {
  echo -e "${BLUE}=== 共享 init SQL 文件 ===${NC}"
  local idir="$HERE/deploy/shared-db/init"
  for i in 0 1 2 3 4 5 6 7 8 9; do
    local f="$idir/${i}_init.sql"
    if [[ -s "$f" ]]; then
      ok "$(basename "$f") $(wc -c < "$f") bytes"
    else
      warn "$(basename "$f") 缺失或 0B"
    fi
  done
  [[ -s "$idir/meta_init.sql" ]] && ok "meta_init.sql $(wc -c < "$idir/meta_init.sql") bytes" \
                                 || warn "meta_init.sql 缺失"

  echo -e "\n${BLUE}=== docker volume ===${NC}"
  docker volume ls --format '{{.Name}}' 2>/dev/null | grep -iE 'shard|meta' || echo "  (无)"

  echo -e "\n${BLUE}=== shared-shard-0 的数据库列表 ===${NC}"
  docker exec shared-shard-0 mysql -uroot -ppassword -e "SHOW DATABASES" 2>&1 | grep -v Warning \
    || echo "  (shared-shard-0 没在运行)"

  echo -e "\n${BLUE}=== shared-shard-0 启动日志的 init 痕迹 ===${NC}"
  docker logs shared-shard-0 2>&1 | grep -iE 'init|entrypoint|running' | head -10 || echo "  (没日志)"
}

# ─── dispatch ───────────────────────────────────────────────────
case "${1:-}" in
  init-kms)  shift; cmd_init_kms "$@" ;;
  up)        shift; cmd_up "$@" ;;
  down)      shift; cmd_down "$@" ;;
  nuke)      shift; cmd_nuke ;;
  restart)   shift; cmd_down; cmd_up "$@" ;;
  status)    shift; cmd_status "$@" ;;
  logs)      shift; cmd_logs "$@" ;;
  check)     shift; cmd_check ;;
  seed)      shift; cmd_seed ;;
  debug-mysql) shift; cmd_debug_mysql ;;
  *)
    cat <<'EOF'
用法: deploy.sh <命令> [参数]

命令：
  init-kms                产生 KMS master key（全栈首次启动前必须跑一次）
  up [svc…]               启动全栈；可指定子集，如 `up kms-manage payment-channel`
  down [--volumes]        停止全栈；带 --volumes 连数据卷一起清
  nuke                    核选项：停+清卷+删 network，彻底从零开始
  restart                 等价 down && up
  status                  逐服务查看 docker compose ps
  logs <svc> [args…]      追某服务日志
  check                   TCP 探活各端口
  debug-mysql             诊断 MySQL：init SQL / volume / 数据库是否建出来

服务名（固定）：
  shared-db  kms-manage  risk-manage  payment-channel  order-core  user-merchant-core
  payment-core  payment-admin-web
EOF
    exit 1
    ;;
esac
