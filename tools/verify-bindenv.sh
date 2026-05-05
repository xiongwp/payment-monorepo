#!/usr/bin/env bash
# verify-bindenv.sh —— 防 viper BindEnv 漏键的 lint。
#
# 起因：cowork mode 改 main.go 时 sub-agent 一次重写 BindEnv 列表，把
# kms.insecure / https.dev_no_tls 等几个 key 漏了。viper 在 env 来源下
# 没 BindEnv 的 key 读不到，dev 模式静默失效到运行期才报错。
#
# 思路：扫每个 cmd/server/main.go：
#   - 收集所有 v.GetString/Bool/Int/Float64/Duration/StringSlice/StringMap...("key") 用过的 key
#   - 收集所有 v.BindEnv("key") 绑过的 key
#   - 用过但没绑 = 漏键，报错退 1
#
# 已知不需要 BindEnv 的特殊 key（在 yaml 默认值里且 env 不会覆盖）放白名单。
# 假设：每个服务的 viper 实例是 *viper.Viper 命名为 v；不绑通配（v.AutomaticEnv 不够稳）。

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# 服务清单：cmd/server/main.go 都扫
SERVICES=(
  card-center card-payment user-merchant-core
  kms-manage order-core payment-core payment-channel risk-manage
  accounting-system
)

# 白名单：本来就不该 BindEnv 的 key（yaml-only / 嵌套 mapstructure / 其他）
declare -A WHITELIST=(
  # 例子：["card-center"]="some.key|other.key"
  ["user-merchant-core"]="auth.allow_unauthenticated|auth.jwt_alg|auth.jwt_private_key_path|auth.jwt_public_key_path|cache.merchant.size|cache.secret.size|database.replicas|rate_limit.per_merchant.default_rps|timeouts.default"
  ["card-center"]="auth.client_cn|audit.topic"
  ["card-payment"]="card_center.rpc_timeout|env"
  ["kms-manage"]=""
)

fail=0
for svc in "${SERVICES[@]}"; do
  main="packages/$svc/cmd/server/main.go"
  reg="packages/$svc/cmd/server/registry.go"
  [ -f "$main" ] || { echo "SKIP $svc (no main.go)"; continue; }
  files="$main"
  [ -f "$reg" ] && files="$files $reg"

  # 用过的 key —— 各种 v.GetXxx("key") 都 grep
  used=$(grep -hE 'v\.Get(String|Bool|Int|Float64|Duration|StringSlice|StringMap[A-Za-z]*|Sub|UnmarshalKey|IsSet)\("[^"]+"\)' $files \
    | grep -oE '"[a-z][a-z0-9_.]+"' | tr -d '"' | sort -u)

  # 绑过的 key
  bound=$(grep -hE 'v\.BindEnv\("[^"]+"\)|_ = v\.BindEnv\("[^"]+"\)' $files \
    | grep -oE '"[a-z][a-z0-9_.]+"' | tr -d '"' | sort -u)

  # 也接受 BindEnv 字符串列表 (`for _, k := range []string{"a", "b"}` 风格)
  bound_list=$(awk '/for _, k := range \[\]string\{/,/} \{/' $files \
    | grep -oE '"[a-z][a-z0-9_.]+"' | tr -d '"' | sort -u)
  bound=$(printf '%s\n%s\n' "$bound" "$bound_list" | sort -u)

  # 也接受 fmt.Sprintf 拼出来的（database.shard_%d.dsn 等）—— 把它们的 prefix 当成已绑
  prefix_bound=$(grep -hE 'v\.BindEnv\(fmt\.Sprintf\("[^"]+"' $files \
    | grep -oE 'fmt\.Sprintf\("[a-z][a-z0-9_.%]+' | sed 's/.*"//' | sed 's/%d.*/%d/' | sort -u)

  # 白名单
  wl="${WHITELIST[$svc]:-}"

  missing=""
  while IFS= read -r k; do
    [ -z "$k" ] && continue
    # 直接绑过？
    if printf '%s\n' "$bound" | grep -qxF "$k"; then continue; fi
    # 白名单？
    if [ -n "$wl" ] && printf '%s' "$k" | grep -qE "^($wl)\$"; then continue; fi
    # 嵌套 fmt.Sprintf prefix 包含？(很粗略，只看是否以 "<prefix>." 开头到第一个 .)
    skip=0
    while IFS= read -r p; do
      [ -z "$p" ] && continue
      # 把 %d 替换成数字 0-9 看 k 是否能套
      pat="$(printf '%s' "$p" | sed 's/%d/[0-9]+/g')"
      if echo "$k" | grep -qE "^$pat\.|^$pat\$"; then skip=1; break; fi
    done < <(printf '%s\n' "$prefix_bound")
    [ $skip -eq 1 ] && continue
    missing="${missing}${missing:+ }${k}"
  done < <(printf '%s\n' "$used")

  if [ -n "$missing" ]; then
    echo "FAIL [$svc] 用了但没 BindEnv（env 读不到）："
    for k in $missing; do echo "    - $k"; done
    fail=$((fail+1))
  else
    echo "OK   [$svc]"
  fi
done

echo
if [ $fail -gt 0 ]; then
  echo "verify-bindenv: $fail service(s) FAILED"
  echo
  echo "修法：在对应 cmd/server/main.go 的 loadConfig() 里 BindEnv 列表加上漏的 key。"
  echo "确认是 yaml-only 不该 env 读时，加进 tools/verify-bindenv.sh WHITELIST。"
  exit 1
fi
echo "verify-bindenv: all services OK"
