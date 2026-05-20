package cache

// BalanceCache Redis 余额热缓存
//
// 存储结构：
//   Key:    balance:{accountNo}
//   Type:   Hash
//   Fields: balance, available, frozen, version, category, status
//
// 设计原则：
//   - Redis 是权威余额（热路径写入），MySQL 是审计/快照层（异步落库）
//   - 所有多账户原子操作通过 Lua 脚本保证，避免 TOCTOU 竞争
//   - 缓存未命中时从 MySQL 加载（由调用方负责预热）

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/xiongwp/accounting-system/internal/metrics"
	"github.com/redis/go-redis/v9"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
)

const (
	balanceKeyPrefix = "balance:"
	balanceTTL       = 24 * time.Hour // 无操作后24小时过期，防止内存泄漏
)

// balanceKey 按 ctx 决定主流量 key（balance:acct123）还是影子 key（balance:acct123_shadow）。
// 影子流量数据不污染主余额；同一 Lua 脚本在两个 key 空间各自原子。
func balanceKey(ctx context.Context, accountNo string) string {
	return shadow.RedisKey(ctx, balanceKeyPrefix+accountNo)
}

// BalanceInfo 账户余额快照
type BalanceInfo struct {
	AccountNo string
	Balance   string
	Available string
	Frozen    string
	Version   int64
	Category  int // 1=Asset/Expense  2=Liability/Equity/Revenue
	Status    int // 1=active
}

// ErrInsufficientBalance 可用余额不足
var ErrInsufficientBalance = errors.New("insufficient available balance")

// ErrAccountNotInCache 账户不在缓存中（需要预热）
var ErrAccountNotInCache = errors.New("account not in balance cache")

// ErrAccountFrozen 账户已冻结或禁用
var ErrAccountFrozen = errors.New("account is frozen or disabled")

// BalanceCache 余额缓存
type BalanceCache struct {
	rdb    redis.UniversalClient
	logger *zap.Logger

	// 预编译的 Lua 脚本（EVALSHA，减少网络传输）
	scriptTransfer *redis.Script
}

// NewBalanceCache 创建余额缓存
func NewBalanceCache(rdb redis.UniversalClient, logger *zap.Logger) *BalanceCache {
	bc := &BalanceCache{rdb: rdb, logger: logger}
	bc.scriptTransfer = redis.NewScript(luaTransfer)
	return bc
}

// ─── Lua 脚本 ─────────────────────────────────────────────────────────────────

// luaTransfer 多账户原子转账脚本
//
// 参数：
//
//	KEYS[1..N]  = "balance:{accountNo}" （每个分录一个 key）
//	ARGV[1..N]  = delta（正=增加 负=减少，字符串形式的浮点数）
//	ARGV[N+1]   = 操作描述（日志用，不影响逻辑）
//
// 返回：
//
//	{1, "ok"}                               成功
//	{-1, "account not found: {key}"}        账户不在缓存
//	{-2, "account frozen: {key}"}           账户冻结
//	{-3, "insufficient balance: {key}"}     余额不足
//
// 实现说明：
//
//	分两遍扫描：第一遍校验所有前置条件（余额、状态），全部通过后第二遍执行变更，
//	保证要么全部成功要么全部不变（Lua 脚本在 Redis 中原子执行）。
//
// luaTransfer 原子多账户转账 + voucher_no 幂等保护。
//
// 参数布局:
//
//	KEYS[1]        幂等哨兵 key（例如 "idem:transfer:<voucher_no>"），SET NX EX 保证同一 voucher
//	               的 Transfer 只生效一次。recovery 场景下进程崩溃重试时，Redis 已应用过的 delta
//	               不会被再次累加（否则 double-count = 凭空多钱）。
//	KEYS[2..n+1]   账户 balance keys
//	ARGV[1]        幂等 TTL（秒，例如 86400 = 1 天；足以覆盖 Recovery Worker 的 PENDING 超时）
//	ARGV[2..n+1]   对应账户的 delta
//
// 返回码:
//
//	{1, 'ok'}                    首次应用成功
//	{1, 'already_applied'}       幂等命中，Redis 已是该 voucher 应用后状态（视为成功，无副作用）
//	{-1, msg}                    某账户不在缓存
//	{-2, msg}                    某账户冻结
//	{-3, msg}                    余额不足
//	{-10, msg}                   delta 格式非法
//
// 注：不在此处校验 sum(delta)==0。computeBalanceDelta 返回的是每个账户的
// "余额变化量"（语义层，ASSET 和 LIAB 充值时同号+X），这种 delta 不会 sum
// 到 0。真正的双分录平衡约束（sum(debits) == sum(credits)）由调用方的
// validateEntries 在进脚本之前保证；Lua 只负责原子应用 deltas 和账户级校验。
const luaTransfer = `
-- KEYS[1] = idemKey
-- KEYS[2..n] = account keys
-- ARGV[1] = ttl (seconds)
-- ARGV[2..n] = delta list (string number)

local idemKey = KEYS[1]
local ttl = tonumber(ARGV[1])
local n = #KEYS - 1

-- =========================
-- 1. 幂等检查（只检查，不写）
-- =========================
if redis.call('EXISTS', idemKey) == 1 then
  return {1, 'already_applied'}
end

-- =========================
-- 2. 校验 delta 格式
-- =========================
for i = 1, n do
  if not tonumber(ARGV[i+1]) then
    return {-10, 'invalid delta'}
  end
end

-- =========================
-- 3. Pass 1: validate
-- =========================
for i = 1, n do
  local key = KEYS[i+1]

  -- 账户存在
  if redis.call('EXISTS', key) == 0 then
    return {-1, 'account not found: ' .. key}
  end

  -- 状态校验
  local status = tonumber(redis.call('HGET', key, 'status'))
  if status ~= 1 then
    return {-2, 'account frozen: ' .. key}
  end

  local delta = math.floor(tonumber(ARGV[i+1]))

  -- 扣款校验
  if delta < 0 then
    local avail = tonumber(redis.call('HGET', key, 'available'))
    if not avail or avail + delta < 0 then
      return {-3, 'insufficient balance: ' .. key}
    end
  end
end

-- =========================
-- 4. Pass 2: apply
-- =========================
for i = 1, n do
  local key = KEYS[i+1]
  local delta = math.floor(tonumber(ARGV[i+1]))

  redis.call('HINCRBY', key, 'balance', delta)
  redis.call('HINCRBY', key, 'available', delta)
  redis.call('HINCRBY', key, 'version', 1)
end

-- =========================
-- 5. 写幂等（最后一步）
-- =========================
redis.call('SET', idemKey, 'success', 'EX', ttl)

return {1, 'ok'}
`

// transferIdempotencyTTL 幂等哨兵存活时间。需覆盖 OutboxWorker 的 PENDING-recovery 最长窗口
// （默认几分钟 — 几小时），同时不要太长浪费内存。1 天足矣。
const transferIdempotencyTTL = 86400

// transferIdempotencyKeyPrefix 幂等哨兵 key 前缀。
const transferIdempotencyKeyPrefix = "idem:transfer:"

// ─── 公共方法 ─────────────────────────────────────────────────────────────────

// luaWarmIfMissing 仅在 key 不存在时写入；用 Lua 保证 EXISTS-then-HSET 原子化。
//
// 防 race：如果某账户已在缓存且有 in-flight Transfer，普通 WarmAccount（Pipeline
// HSET）会把 Transfer 已经 HINCRBY 应用过的 delta 直接覆盖回 MySQL 值 → 凭空丢钱。
// 用 Lua 把"key 已存在则跳过"这层保护原子化，杜绝该 race。
//
// 返回 1 表示 warmed；0 表示已存在跳过。
const luaWarmIfMissing = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
redis.call('HSET', KEYS[1],
  'balance', ARGV[1],
  'available', ARGV[2],
  'frozen', ARGV[3],
  'version', ARGV[4],
  'category', ARGV[5],
  'status', ARGV[6])
redis.call('EXPIRE', KEYS[1], ARGV[7])
return 1
`

// WarmAccount 将账户余额写入 Redis（由 MySQL 数据初始化）。
//
// 仅在 Redis 中无该账户 key 时写入；若已存在则跳过（防止覆盖正在被 Transfer
// 应用的 delta 导致丢钱）。CreateAccount 后或缓存 miss 时调用都安全。
//
// 如果业务确实需要"强制刷新 Redis 用 MySQL 最新值覆盖"（极少见，仅在 ops
// 介入修复时），用 RefreshAccount。
func (c *BalanceCache) WarmAccount(ctx context.Context, info BalanceInfo) error {
	key := balanceKey(ctx, info.AccountNo)
	res, err := c.rdb.Eval(ctx, luaWarmIfMissing, []string{key},
		info.Balance, info.Available, info.Frozen, info.Version,
		info.Category, info.Status, int(balanceTTL.Seconds()),
	).Result()
	if err != nil {
		return fmt.Errorf("warm account %s: %w", info.AccountNo, err)
	}
	// res == 0 表示 key 已存在跳过；不视为错误。
	_ = res
	return nil
}

// RefreshAccount 强制覆盖 Redis 中的账户余额为给定值。
//
// 危险：调用前必须确认没有 in-flight Transfer 在用同一账户（例如 ops 介入
// 修复时先停服或 freeze 账户）。日常运行不应使用。
func (c *BalanceCache) RefreshAccount(ctx context.Context, info BalanceInfo) error {
	key := balanceKey(ctx, info.AccountNo)
	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key,
		"balance", info.Balance,
		"available", info.Available,
		"frozen", info.Frozen,
		"version", info.Version,
		"category", info.Category,
		"status", info.Status,
	)
	pipe.Expire(ctx, key, balanceTTL)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("refresh account %s: %w", info.AccountNo, err)
	}
	return nil
}

// GetBalance 读取账户余额（缓存 miss 返回 ErrAccountNotInCache）
func (c *BalanceCache) GetBalance(ctx context.Context, accountNo string) (*BalanceInfo, error) {
	key := balanceKey(ctx, accountNo)
	vals, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("hgetall %s: %w", key, err)
	}
	if len(vals) == 0 {
		return nil, ErrAccountNotInCache
	}
	ver, err := strconv.ParseInt(vals["version"], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("balance cache corrupt: invalid version for %s: %w", accountNo, err)
	}
	cat, err := strconv.Atoi(vals["category"])
	if err != nil {
		return nil, fmt.Errorf("balance cache corrupt: invalid category for %s: %w", accountNo, err)
	}
	status, err := strconv.Atoi(vals["status"])
	if err != nil {
		return nil, fmt.Errorf("balance cache corrupt: invalid status for %s: %w", accountNo, err)
	}
	return &BalanceInfo{
		AccountNo: accountNo,
		Balance:   vals["balance"],
		Available: vals["available"],
		Frozen:    vals["frozen"],
		Version:   ver,
		Category:  cat,
		Status:    status,
	}, nil
}

// Exists 判断账户是否在缓存中
func (c *BalanceCache) Exists(ctx context.Context, accountNo string) (bool, error) {
	n, err := c.rdb.Exists(ctx, balanceKey(ctx, accountNo)).Result()
	return n > 0, err
}

// Transfer 原子多账户余额变更，带 voucher 级幂等保护。
//
// 参数:
//
//	voucherNo 幂等键（通常 = 凭证号）。同一 voucherNo 多次调用只会应用一次 delta，
//	          崩溃重试 / OutboxWorker 恢复场景下避免 double-count。传空串将以
//	          随机一次性 key 兜底（退化为无幂等，仅用于非热路径调用）。
//	entries   accountNo → delta（正=加 负=减）
//
// 底层调用 Lua 脚本：先 SET NX 幂等哨兵、再全量校验、再全量变更，原子执行。
// 返回 nil 同时表示两种结果：首次应用成功 / 幂等命中（msg = "already_applied"）。
func (c *BalanceCache) Transfer(ctx context.Context, voucherNo string, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}

	// 幂等 key 也按 ctx 隔离主 / 影：相同 voucherNo 在主和影流量下生成不同
	// idemKey，避免压测被误判成主流量重试 / 反之亦然。
	idemKey := shadow.RedisKey(ctx, transferIdempotencyKeyPrefix+voucherNo)
	if voucherNo == "" {
		// 兜底：无幂等键时生成随机一次性 key，不触发 already_applied 分支。
		// 不应在热路径出现（所有 Outbox-backed 调用都应传 voucherNo）。
		idemKey = shadow.RedisKey(ctx, fmt.Sprintf("%snovoucher:%d", transferIdempotencyKeyPrefix, time.Now().UnixNano()))
	}

	// KEYS = [idemKey, balance-key-1, balance-key-2, ...]
	// ARGV = [ttl,     delta-1,       delta-2,      ...]
	keys := make([]string, 0, len(entries)+1)
	args := make([]interface{}, 0, len(entries)+1)
	keys = append(keys, idemKey)
	args = append(args, transferIdempotencyTTL)
	for accNo, delta := range entries {
		// 影子流量进 _shadow 副本 key 池；Lua 脚本一次只跑一个 ctx 视角，
		// 主 / 影 entries 不能混在同一次 Transfer（业务侧由 ctx.IsShadow 保证）。
		keys = append(keys, balanceKey(ctx, accNo))
		args = append(args, delta)
	}

	result, err := c.scriptTransfer.Run(ctx, c.rdb, keys, args...).Slice()
	if err != nil {
		return fmt.Errorf("lua transfer: %w", err)
	}
	log.Printf("transfer result (%v): (%v)", result, err)
	code, _ := result[0].(int64)
	msg, _ := result[1].(string)

	switch code {
	case 1:
		// 'ok' 和 'already_applied' 都是成功；msg 只作调试。
		// already_applied 命中时打 metric，方便观测幂等保护是否真的拦下了重试。
		if msg == "already_applied" {
			metrics.TransferIdempotencyHitsTotal.Inc()
		}
		return nil
	case -1:
		return fmt.Errorf("%w: %s", ErrAccountNotInCache, msg)
	case -2:
		return fmt.Errorf("%w: %s", ErrAccountFrozen, msg)
	case -3:
		return fmt.Errorf("%w: %s", ErrInsufficientBalance, msg)
	default:
		return fmt.Errorf("lua transfer unknown result code=%d msg=%s", code, msg)
	}
}

// Invalidate 删除缓存（账户变更后强制刷新）
func (c *BalanceCache) Invalidate(ctx context.Context, accountNos ...string) error {
	keys := make([]string, len(accountNos))
	for i, no := range accountNos {
		keys[i] = balanceKey(ctx, no)
	}
	return c.rdb.Del(ctx, keys...).Err()
}
