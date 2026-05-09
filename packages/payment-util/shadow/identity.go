// identity.go: shadow 流量与主流量的身份段（user_id / 各种字符串 ID）隔离。
//
// 全平台统一约束（与 order-core / accounting-system / user-merchant-core 的
// CLAUDE.md 段约定对齐）：
//
//	┌──────────────────────────────┬────────────────────────────────┐
//	│ 段                          │ 用途                           │
//	├──────────────────────────────┼────────────────────────────────┤
//	│ user_id  [0, 99]            │ accounting fleet 平台账户保留  │
//	│ user_id  [100, 99_999_999]  │ 保留未用                       │
//	│ user_id  [1e8, 9e8)         │ 主流量真实用户                 │
//	│ user_id  [9e8, 9e9)         │ 保留未用（隔离区）             │
//	│ user_id  [9e9, 9.9e9]       │ ★ shadow 影子用户              │
//	└──────────────────────────────┴────────────────────────────────┘
//
//	所有字符串业务 ID（merchant_id / account_no / pi_id / ch_id / re_id /
//	pa_id / dp_id / voucher_no / tcc_id / customer_id / ...）：
//	  主流量：原格式（不允许以 "shadow_" 开头）
//	  shadow：必须以 "shadow_" 前缀，例如：
//	    主    pi_115xxxxx   → shadow  shadow_pi_115xxxxx
//	    主    mch_xxx       → shadow  shadow_mch_xxx
//	    主    115xxxxx-1    → shadow  shadow_115xxxxx-1
//
// 这套段约束**双向强制隔离**主 / 影子身份空间：
//
//   - shadow ctx 携带主段 ID（< 9e9 / 不带前缀）→ Validate 拒
//   - 主 ctx 携带 shadow 段 ID（>= 9e9 / 带前缀）→ Validate 拒
//
// 推荐在 service / RPC handler 入口做一次 ValidateUserID +
// ValidateID(ctx, mid, "merchant_id") 等校验。

package shadow

import (
	"context"
	"fmt"
	"strconv"
)

// ─── user_id（数字段隔离）─────────────────────────────────────────────────

// user_id 段常量。改这些值需要全平台同步评估（accounting fleet 占用、
// order-core PI 路由、user-merchant-core 号段分配 init seed 等都依赖具体边界）。
//
// 段细分（保留 fleet 平台账户独立子段，预留充足容量给未来扩展）：
//
//	┌────────────────────────────────────┬──────────────────────────────┐
//	│ 段                                │ 用途                         │
//	├────────────────────────────────────┼──────────────────────────────┤
//	│ [0, 1e6)                          │ 老 fleet 兼容段（已弃用）    │
//	│ [1e6, 1e7)              900 万容量 │ ★ 主流量平台 fleet 账户       │
//	│ [1e7, 1e8)                        │ 保留                         │
//	│ [1e8, 9e8)              8 亿容量   │ ★ 主流量真实用户              │
//	│ [9e8, 9e9)                        │ 保留（隔离区）                │
//	│ [9e9, 9.01e9)           1000 万容量 │ ★ shadow 平台 fleet 账户     │
//	│ [9.01e9, 9.9e9]         8.9 亿容量 │ ★ shadow 真实用户            │
//	└────────────────────────────────────┴──────────────────────────────┘
//
// fleet 段使用规则：accounting-system 每个 globalTableIdx (0-99) 分配一个 fleet
// user_id，实际占用 100 × N (N 种 platform 账户类型，~6 种)。900 万容量给未来
// 加新 platform 账户类型 / 扩 shard 数量等留充足空间。
const (
	// ─── 主流量段 ────────────────────────────────────────────────────
	// MainFleetUserIDMin 主流量平台 fleet 账户段下界（含）。1e6。
	MainFleetUserIDMin int64 = 1_000_000
	// MainFleetUserIDMax 主流量平台 fleet 账户段上界（含）。1e7 - 1。
	MainFleetUserIDMax int64 = 9_999_999

	// MainUserIDMin 主流量真实用户段下界（含）。1e8。
	MainUserIDMin int64 = 100_000_000
	// MainUserIDMax 主流量真实用户段上界（含）。
	MainUserIDMax int64 = 899_999_999 // 8.99e8

	// ─── shadow 段 ───────────────────────────────────────────────────
	// ShadowFleetUserIDMin 影子平台 fleet 账户段下界（含）。9e9。
	ShadowFleetUserIDMin int64 = 9_000_000_000
	// ShadowFleetUserIDMax 影子平台 fleet 账户段上界（含）。9.01e9 - 1。
	ShadowFleetUserIDMax int64 = 9_009_999_999

	// ShadowUserIDMin 影子真实用户段下界（含）。让出前 1000 万给 shadow fleet。
	ShadowUserIDMin int64 = 9_010_000_000
	// ShadowUserIDMax 影子真实用户段上界（含）。9.899e9。
	ShadowUserIDMax int64 = 9_899_999_999
)

// FleetIDLayout fleet 账户的 user_id 分配两维 layout：
//
//	user_id - MainFleetUserIDMin = channelCode * FleetChannelStride + globalTableIdx
//
// 这样 fleet 账户支持**两种独立扩展**：
//   - 横向：globalTableIdx ∈ [0, FleetChannelStride-1]（默认 1000；一个 channel
//     可以分配最多 1000 个 shard，远超当前 100 上限，给未来 4× 扩容预留充足空间）
//   - 纵向：channelCode ∈ [0, MaxFleetChannels-1]（默认 9000；可以给每个支付渠道 /
//     业务线 / 国家分配独立 fleet 账户段）
//
// 之前的 layout `MainFleetUserID(globalTbl) = 1_000_000 + globalTbl` 把 channel
// 和 shard 拍扁到同一维，导致：
//   1. 扩 shard 时新 shard 没有对应 fleet user_id（旧 fleet 锁死在 user_id 1_000_000-99）
//   2. 多 channel 没法独立分配 fleet（不同渠道 transit account 全挤一起）
//
// **总容量 = 9000 × 1000 = 9_000_000**，落在 fleet 段 [1_000_000, 9_999_999] 内。
// 即便每个 channel 用满 1000 shard，也只占 channelCode×stride 一行号段，不溢出。
const (
	// FleetChannelStride 单个 channel 内可分配的 shard 数量上限。1000 足够给当前
	// 100 shard × 10× 头部空间。改这个值会让所有 fleet user_id 整体平移——只能在
	// 全新部署时改。
	FleetChannelStride int = 1000
	// MaxFleetChannels 全局允许的 channel 段位数量。9000 等于把 fleet 段 [1e6, 1e7)
	// 的 9_000_000 容量除以 1000 stride。
	MaxFleetChannels int = 9000
)

// FleetChannelDefault 默认 channel 段位（兼容旧调用：MainFleetUserID(globalTbl)
// 等价于 FleetUserID(0, globalTbl)）。新建系统强烈建议显式指定 channel。
const FleetChannelDefault int = 0

// FleetUserID 给定 (channelCode, globalTableIdx) 计算 fleet user_id（按 ctx 自动
// 区分主流量 / shadow 段）。
//
// 用法：
//
//	// 默认 channel + 当前分片
//	uid := FleetUserID(ctx, FleetChannelDefault, globalTableIdx)
//
//	// 渠道独立 fleet（gcash channel 占 channelCode=10）
//	uid := FleetUserID(ctx, 10, globalTableIdx)
func FleetUserID(ctx context.Context, channelCode, globalTableIdx int) int64 {
	if channelCode < 0 || channelCode >= MaxFleetChannels {
		// 越界给 0，调用方应该自己检查；不 panic 是因为 ctx 路径不该 panic
		channelCode = 0
	}
	if globalTableIdx < 0 || globalTableIdx >= FleetChannelStride {
		globalTableIdx = 0
	}
	offset := int64(channelCode*FleetChannelStride + globalTableIdx)
	if IsShadow(ctx) {
		return ShadowFleetUserIDMin + offset
	}
	return MainFleetUserIDMin + offset
}

// MainFleetUserID 给定 globalTableIdx (0-99) 返回对应的主流量 fleet user_id。
//
// **Deprecated**: 用 FleetUserID(ctx, FleetChannelDefault, globalTableIdx) 替代，
// 支持 channel 独立分配。本函数保留兼容 init seed 脚本 + 旧 batchtask。
//
//	globalTableIdx=0  → 1_000_000
//	globalTableIdx=99 → 1_000_099
func MainFleetUserID(globalTableIdx int) int64 {
	return MainFleetUserIDMin + int64(globalTableIdx)
}

// ShadowFleetUserID 给定 globalTableIdx (0-99) 返回对应的 shadow fleet user_id。
//
// **Deprecated**: 用 FleetUserID(shadow_ctx, FleetChannelDefault, globalTableIdx) 替代。
//
//	globalTableIdx=0  → 9_000_000_000
//	globalTableIdx=99 → 9_000_000_099
func ShadowFleetUserID(globalTableIdx int) int64 {
	return ShadowFleetUserIDMin + int64(globalTableIdx)
}

// DecodeFleetUserID 反解 FleetUserID 编码出的 user_id。返回 (channelCode, globalTableIdx)。
// 输入不在 fleet 段时返回 (-1, -1)。
func DecodeFleetUserID(userID int64) (channelCode, globalTableIdx int) {
	var offset int64
	switch {
	case userID >= MainFleetUserIDMin && userID <= MainFleetUserIDMax:
		offset = userID - MainFleetUserIDMin
	case userID >= ShadowFleetUserIDMin && userID <= ShadowFleetUserIDMax:
		offset = userID - ShadowFleetUserIDMin
	default:
		return -1, -1
	}
	channelCode = int(offset / int64(FleetChannelStride))
	globalTableIdx = int(offset % int64(FleetChannelStride))
	return
}

// IsFleetUserID 判断 user_id 是否在主或影子的 fleet 段（不依赖 ctx）。
func IsFleetUserID(userID int64) bool {
	if userID >= MainFleetUserIDMin && userID <= MainFleetUserIDMax {
		return true
	}
	if userID >= ShadowFleetUserIDMin && userID <= ShadowFleetUserIDMax {
		return true
	}
	return false
}

// UserIDRange 按 ctx 返回合法 user_id 段 [min, max]（含）。
func UserIDRange(ctx context.Context) (min, max int64) {
	if IsShadow(ctx) {
		return ShadowUserIDMin, ShadowUserIDMax
	}
	return MainUserIDMin, MainUserIDMax
}

// ValidateUserID 双向校验 user_id 是否落在 ctx 对应段内。
//
//   - shadow ctx + user_id ∈ [9e9, 9.9e9]   → nil
//   - 主流量 ctx + user_id ∈ [1e8, 9e8)     → nil
//   - 其他组合（含跨段、保留段、平台 fleet）→ error
func ValidateUserID(ctx context.Context, userID int64) error {
	lo, hi := UserIDRange(ctx)
	if userID < lo || userID > hi {
		return fmt.Errorf("user_id %d out of range [%d, %d] for shadow=%v",
			userID, lo, hi, IsShadow(ctx))
	}
	return nil
}

// IsShadowUserID 仅按数值判断是否在 shadow 段（不依赖 ctx）。
// 跨服务回包按 ID 反推流量类型用。
func IsShadowUserID(userID int64) bool {
	return userID >= ShadowUserIDMin && userID <= ShadowUserIDMax
}

// IsMainUserID 仅按数值判断是否在主流量真实用户段。
// 平台 fleet 段 [0, 99] 和保留段都返回 false。
func IsMainUserID(userID int64) bool {
	return userID >= MainUserIDMin && userID <= MainUserIDMax
}

// ShadowUserID 把主段 user_id 映射到 shadow 段（保持 offset）。
//
//	main 1e8       → shadow 9e9
//	main 1e8 + 1   → shadow 9e9 + 1
//	main 9e8 - 1   → shadow 9.9e9 - 1
//
// 输入已经在 shadow 段 → 幂等返回原值。
// 输入在 fleet / 保留段 → 返回 0 让调用方处理。
func ShadowUserID(mainUserID int64) int64 {
	if IsShadowUserID(mainUserID) {
		return mainUserID
	}
	if !IsMainUserID(mainUserID) {
		return 0
	}
	return ShadowUserIDMin + (mainUserID - MainUserIDMin)
}

// ─── merchant_id（数字位段编码）─────────────────────────────────────────
//
// merchant_id 用 19 位十进制 layout，shadow 通过最高位区分（与 account_id
// 同模式）：
//
//	位号 (右起 1 始):
//	   19    18 17 16 15 14 13 12 11 10 9 8 7 6 5 4 3 2 1
//	    │    └────────────── 18 位 seq ──────────────────┘
//	    └─ shadow flag (0 = 主流量 / 1 = shadow)
//
// 容量：18 位 seq = 1e18 个商户，永不会用完。
//
// 示例：
//
//	主流量第 1 个商户  → 1
//	主流量第 100001 个 → 100001
//	shadow  第 1 个    → 1_000000000000000001 = 1000000000000000001
//	shadow  第 100001  → 1_000000000000100001 = 1000000000000100001
//
// 数量级与 user_id（最大 9.9e9）/account_id（最大 9.99e18 顶部位）天然区隔，
// 运维一眼可辨：
//
//	1-9.9e9             → user_id / customer_id
//	1-1e18 / 1e18-2e18  → merchant_id（其中 ≥1e18 = shadow）
//	1e17 数量级 + 高位 1→ account_id (shadow)
const (
	// MerchantIDShadowMul shadow flag 在最高位的偏移量。bit 19 = 1e18。
	MerchantIDShadowMul int64 = 1_000_000_000_000_000_000 // 1e18
	// MerchantIDSeqMax 单流量空间里最大商户序号（18 位）。
	MerchantIDSeqMax int64 = 999_999_999_999_999_999 // 1e18 - 1
)

// EncodeMerchantID 用 ctx + seq 拼一个 merchant_id。
// shadow ctx 自动加最高位偏移；seq 必须在 [1, 1e18-1]（0 保留作"无效"）。
func EncodeMerchantID(ctx context.Context, seq int64) (int64, error) {
	if seq <= 0 || seq > MerchantIDSeqMax {
		return 0, fmt.Errorf("merchant_id seq %d out of range [1, %d]", seq, MerchantIDSeqMax)
	}
	id := seq
	if IsShadow(ctx) {
		id += MerchantIDShadowMul
	}
	return id, nil
}

// DecodeMerchantID 从 merchant_id 解出 (isShadow, seq)。不依赖 ctx。
func DecodeMerchantID(merchantID int64) (isShadow bool, seq int64) {
	if merchantID >= MerchantIDShadowMul {
		isShadow = true
		seq = merchantID - MerchantIDShadowMul
	} else {
		seq = merchantID
	}
	return
}

// IsShadowMerchantID 用 layout 高位判断 shadow（不依赖 ctx）。
func IsShadowMerchantID(merchantID int64) bool {
	return merchantID >= MerchantIDShadowMul
}

// ValidateMerchantID 双向校验 merchant_id 与 ctx 段一致：
//   - shadow ctx + merchant_id ≥ 1e18 → ok
//   - 主流量 ctx + merchant_id ∈ [1, 1e18) → ok
//   - 错配 / 越界 / ≤ 0 → error
func ValidateMerchantID(ctx context.Context, merchantID int64) error {
	if merchantID <= 0 {
		return fmt.Errorf("merchant_id %d must be positive", merchantID)
	}
	isShadow, seq := DecodeMerchantID(merchantID)
	if seq <= 0 || seq > MerchantIDSeqMax {
		return fmt.Errorf("merchant_id %d: seq=%d out of range", merchantID, seq)
	}
	if isShadow != IsShadow(ctx) {
		return fmt.Errorf("merchant_id %d shadow=%v mismatches ctx shadow=%v",
			merchantID, isShadow, IsShadow(ctx))
	}
	return nil
}

// ShadowMerchantID 把主流量 merchant_id 映射到 shadow 段（保持 seq）。
// 已经是 shadow → 幂等返回；非法 / ≤ 0 → 返回 0。
func ShadowMerchantID(mainMerchantID int64) int64 {
	if mainMerchantID <= 0 {
		return 0
	}
	if IsShadowMerchantID(mainMerchantID) {
		return mainMerchantID
	}
	return mainMerchantID + MerchantIDShadowMul
}

// ─── customer_id（与 user_id 同段，不单独编码）─────────────────────────
//
// customer_id 是 user-merchant-core / order-core 里和 user_id 等价的概念
// （同一段 [1e8, 9e8) 主 / [9e9, 9.9e9] shadow）；不需要单独的 layout。
// ValidateCustomerID 直接复用 ValidateUserID 的逻辑。

// ValidateCustomerID 等价于 ValidateUserID（同段约束）。
func ValidateCustomerID(ctx context.Context, customerID int64) error {
	if err := ValidateUserID(ctx, customerID); err != nil {
		return fmt.Errorf("customer_id %d (treated as user_id): %v", customerID, err)
	}
	return nil
}

// ─── 通用 EntityID（按分片表分布的业务实体 ID）─────────────────────────
//
// 用于 order-core 的 payment_intent / charge / refund / pay_action / dispute，
// accounting-system 的 voucher / tcc，clearing-settlement 的 settlement_record，
// payment-channel 的 acquirer_tx 等所有"按分片表分布"的实体 ID。
//
// 全部用统一 19 位十进制 layout（int64）：
//
//	位号 (右起 1 始):
//	   19    18 17    16 15 14 13 12 11 10 9 8 7 6 5 4 3 2 1
//	    │    │  │     └──────────── 16 位 seq ──────────────┘
//	    │    │  │
//	    │    └──┘
//	    │  globalTableIdx (00-99，业务的全局分片表序号)
//	    └─ shadow flag (0=主 / 1=shadow)
//
// 路由规则（与 order-core / accounting-system 现有 10×10 分片对齐）：
//
//	globalTableIdx ∈ [00, 99]
//	dbIdx          = globalTableIdx / 10
//	tableSuffix    = globalTableIdx          // 表名 = base_NN
//
// 容量：单 (shadow, globalTableIdx) 组合下 1e16 个实体（10 万亿），
// 永不会用完；总分片 100 张 → 1e18 容量。
//
// 示例：
//
//	主流量 PI 在 shard 42, seq 1 → 0_42_0000000000000001 = 420000000000000001
//	shadow PI 在 shard 42, seq 1 → 1_42_0000000000000001 = 1420000000000000001
//
// 不同实体类型（PI / Charge / Refund / ...）的 ID 共享同一 layout，但通过**所在
// 表**区分（payment_intent_42 / charge_42 / refund_42）。同一 globalTableIdx
// 下不同表的 seq 是独立的（idgen 按 biz_tag 分配）→ pi_id 与 ch_id 数值上可能
// 撞，但属于不同表，业务侧用上下文（哪张表）区分。
const (
	// EntityIDSeqMul globalTableIdx 的进位（seq 16 位）
	EntityIDSeqMul int64 = 10_000_000_000_000_000 // 1e16
	// EntityIDShadowMul shadow flag 的进位（1 + globalTableIdx 2 + seq 16 = 19 位）
	EntityIDShadowMul int64 = 100 * EntityIDSeqMul // 1e18 = 100e16

	// EntityIDSeqMax 单 (shadow, globalTableIdx) 内最大 seq（16 位 - 1）
	EntityIDSeqMax int64 = EntityIDSeqMul - 1 // 9999999999999999
	// EntityIDTableMax 全局表序号上界（10 库 × 10 表 = 100 张）
	EntityIDTableMax int = 99
)

// EncodeEntityID 拼一个统一的实体 ID。globalTableIdx 0-99，seq 1+。
// shadow ctx → 高位 1。
func EncodeEntityID(ctx context.Context, globalTableIdx int, seq int64) (int64, error) {
	if globalTableIdx < 0 || globalTableIdx > EntityIDTableMax {
		return 0, fmt.Errorf("globalTableIdx %d out of range [0, %d]", globalTableIdx, EntityIDTableMax)
	}
	if seq <= 0 || seq > EntityIDSeqMax {
		return 0, fmt.Errorf("seq %d out of range [1, %d]", seq, EntityIDSeqMax)
	}
	id := int64(globalTableIdx)*EntityIDSeqMul + seq
	if IsShadow(ctx) {
		id += EntityIDShadowMul
	}
	return id, nil
}

// DecodeEntityID 从实体 ID 解出 (isShadow, globalTableIdx, seq)。不依赖 ctx。
func DecodeEntityID(entityID int64) (isShadow bool, globalTableIdx int, seq int64) {
	if entityID >= EntityIDShadowMul {
		isShadow = true
		entityID -= EntityIDShadowMul
	}
	globalTableIdx = int(entityID / EntityIDSeqMul)
	seq = entityID % EntityIDSeqMul
	return
}

// EntityIDDBIndex 从实体 ID 解出 dbIdx（10 库分片，全局表 0-99 → db 0-9）。
func EntityIDDBIndex(entityID int64) int {
	_, gti, _ := DecodeEntityID(entityID)
	return gti / 10
}

// EntityIDTableIndex 从实体 ID 解出 globalTableIdx（00-99）。
func EntityIDTableIndex(entityID int64) int {
	_, gti, _ := DecodeEntityID(entityID)
	return gti
}

// IsShadowEntityID 用 layout 高位（bit 19）判断 shadow（不依赖 ctx）。
func IsShadowEntityID(entityID int64) bool {
	return entityID >= EntityIDShadowMul
}

// ValidateEntityID 双向校验：
//   - shadow ctx + 高位 1 + globalTableIdx ∈ [0, 99] + seq > 0 → ok
//   - 主 ctx + 高位 0 + 同上 → ok
//   - 错配 / 越界 / ≤ 0 → error
//
// 业务侧实体（pi / ch / re / pa / dp / voucher / tcc / settlement / acquirer_tx
// 等）都用本函数校验；路由侧用 EntityIDDBIndex / EntityIDTableIndex 解析分片。
func ValidateEntityID(ctx context.Context, entityID int64) error {
	if entityID <= 0 {
		return fmt.Errorf("entity_id %d must be positive", entityID)
	}
	isShadow, gti, seq := DecodeEntityID(entityID)
	if gti < 0 || gti > EntityIDTableMax {
		return fmt.Errorf("entity_id %d: globalTableIdx=%d out of range", entityID, gti)
	}
	if seq <= 0 || seq > EntityIDSeqMax {
		return fmt.Errorf("entity_id %d: seq=%d out of range", entityID, seq)
	}
	if isShadow != IsShadow(ctx) {
		return fmt.Errorf("entity_id %d shadow=%v mismatches ctx shadow=%v",
			entityID, isShadow, IsShadow(ctx))
	}
	return nil
}

// 业务实体便利包装（错误信息一致，且让 IDE 跳转到具体 ID 类型）—— 全部委托给
// ValidateEntityID。新加实体类型时直接抄一份。

func ValidatePaymentIntentID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("payment_intent_id: %v", err)
	}
	return nil
}

func ValidateChargeID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("charge_id: %v", err)
	}
	return nil
}

func ValidateRefundID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("refund_id: %v", err)
	}
	return nil
}

func ValidatePayActionID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("pay_action_id: %v", err)
	}
	return nil
}

func ValidateDisputeID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("dispute_id: %v", err)
	}
	return nil
}

func ValidateVoucherNo(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("voucher_no: %v", err)
	}
	return nil
}

func ValidateTCCID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("tcc_id: %v", err)
	}
	return nil
}

func ValidateSettlementID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("settlement_id: %v", err)
	}
	return nil
}

func ValidateAcquirerTxID(ctx context.Context, id int64) error {
	if err := ValidateEntityID(ctx, id); err != nil {
		return fmt.Errorf("acquirer_tx_id: %v", err)
	}
	return nil
}

// ─── account_id（数字段隔离，给 accounting-system 迁移到 int64 用）──────
//
// 当前 accounting-system 的 account_no 是字符串格式
// `{dbIdx}{tblIdx:02d}{user_id_last8:0>8}-{business_type}`（如 "115xxxxx-1"），
// 既影响可读性也使 ID 比较 / 索引偏向字符串路径。规划迁移到 int64 后，按下面段
// 隔离主 / 影子，与 user_id 段同套思路：
//
//	┌──────────────────────────────┬───────────────────────────────┐
//	│ 段                          │ 用途                          │
//	├──────────────────────────────┼───────────────────────────────┤
//	│ account_id  [0, 1e10 )      │ 保留（fleet 平台账户 / 字典）│
//	│ account_id  [1e10, 9e10)    │ 主流量真实账户                │
//	│ account_id  [9e10, 9.9e10]  │ ★ shadow 影子账户             │
//	└──────────────────────────────┴───────────────────────────────┘
//
// 容量足够：100 亿主账户 ≫ 9 亿用户 × 任意币种 × 任意业务类型组合，
// 永远不会和 user_id 段重叠（数量级差 100 倍）。迁移路径：
//
//   1. 给 account 表加 account_id BIGINT 列（与现 account_no 字符串并存）
//   2. 新建账户时同时写 account_id（按段分配）+ account_no（保留兼容）
//   3. 各服务读写改用 account_id；监控双向一致
//   4. 半年后 account_no 列可下线（保留快照表的 account_no 用于审计追溯）
//
// 在那之前，accounting-system 的 ValidateAccountNo（字符串前缀）继续用。
const (
	// MainAccountIDMin 主流量真实账户段下界（含）。1e10。
	MainAccountIDMin int64 = 10_000_000_000
	// MainAccountIDMax 主流量真实账户段上界（含）。9e10 - 1。
	MainAccountIDMax int64 = 89_999_999_999
	// ShadowAccountIDMin 影子账户段下界（含）。9e10。
	ShadowAccountIDMin int64 = 90_000_000_000
	// ShadowAccountIDMax 影子账户段上界（含）。9.9e10 - 1。
	ShadowAccountIDMax int64 = 98_999_999_999
)

// AccountIDRange 按 ctx 返回合法 account_id 段 [min, max]（含）。
func AccountIDRange(ctx context.Context) (min, max int64) {
	if IsShadow(ctx) {
		return ShadowAccountIDMin, ShadowAccountIDMax
	}
	return MainAccountIDMin, MainAccountIDMax
}

// ValidateAccountID 双向校验数字 account_id 是否落在 ctx 对应段内。
// accounting-system 迁移到 int64 之前，调用方可继续用 ValidateAccountNo 校验
// 字符串格式；迁移后切到本函数。
func ValidateAccountID(ctx context.Context, accountID int64) error {
	lo, hi := AccountIDRange(ctx)
	if accountID < lo || accountID > hi {
		return fmt.Errorf("account_id %d out of range [%d, %d] for shadow=%v",
			accountID, lo, hi, IsShadow(ctx))
	}
	return nil
}

// IsShadowAccountID 仅按数值判断属不属于 shadow 段（不依赖 ctx）。
func IsShadowAccountID(accountID int64) bool {
	return accountID >= ShadowAccountIDMin && accountID <= ShadowAccountIDMax
}

// IsMainAccountID 仅按数值判断属不属于主流量真实账户段。
func IsMainAccountID(accountID int64) bool {
	return accountID >= MainAccountIDMin && accountID <= MainAccountIDMax
}

// ShadowAccountID 把主段 account_id 映射到 shadow 段（保持 offset）。
// 输入已经是 shadow 段 → 幂等。输入在保留段 → 返回 0。
//
// Deprecated: 简单平移段，不带语义。新代码推荐用 EncodeAccountID 编码完整
// (shadow / currency / business_type / seq) 维度，shadow 通过 layout 高位
// 标识；这里保留供过渡期使用。
func ShadowAccountID(mainAccountID int64) int64 {
	if IsShadowAccountID(mainAccountID) {
		return mainAccountID
	}
	if !IsMainAccountID(mainAccountID) {
		return 0
	}
	return ShadowAccountIDMin + (mainAccountID - MainAccountIDMin)
}

// ─── account_id 位段编码（推荐 layout）─────────────────────────────────
//
// 十进制位段编码 — 让 account_id 一眼看出归属，运维 / 客服查单友好。
// 总长 19 位十进制（int64 上界 9.22e18，留出 sign bit）。
//
//	位号 (右起 1 始):
//	   19    18 17 16    15    14 13 12    11 10 9 8 7 6 5 4 3 2 1
//	    │    │  │  │     │     │  │  │     └────────── 11 位 seq ──────────┘
//	    │    └──┴──┘     │     └──┴──┘
//	    │   currency     │     business_type
//	    │                └─ account_type (owner: USER=1 / MERCHANT=2 / ...)
//	    └─ shadow flag (0=主流量 / 1=shadow)
//
// 字段含义：
//
//	bit 19      shadow flag       0 主流量 / 1 shadow（与 ctx.IsShadow 一致）
//	bit 18-16   currency          ISO 4217 数字代码（PHP=608 / USD=840 / CNY=156 / IDR=360 ...）
//	bit 15      account_type      AccountType 字典（USER=1 MERCHANT=2 PENDING=3 PLATFORM=4 ...）
//	bit 14-12   business_type     1-999（与 accounting-system AccountBusinessType 对齐）
//	bit 11-1    seq               每 (shadow, currency, account_type, business_type) 独立递增；1000 亿容量
//
// 示例：
//
//	主流量 user@globalTbl=01 PHP USER(01) business=0001 seq=1
//	   shadow=0  currency=608  type=01  globalTbl=01  business=0001  seq=0000001
//	   →   0_608_01_01_0001_0000001  =  608010100010000001
//
//	shadow user@globalTbl=37 PHP MERCHANT(02) business=0002 seq=2
//	   shadow=1  currency=608  type=02  globalTbl=37  business=0002  seq=0000002
//	   →   1_608_02_37_0002_0000002  =  1608023700020000002
//
// **关键设计点**：globalTblIdx 嵌入 account_id，让 RouteByAccountNo 可以直接
// 从 ID 数值上拿到分片，不需要外部映射 → 保证"用户的所有账户都在用户那一片"
// 的强约束（TCC try/confirm/cancel 单分片事务前提）。
//
// 容量评估：单 (shadow, currency, accountType, globalTblIdx, businessType) 组合下
// 10M 个账户（seq 7 位）。100 shard × 10M = 1B per (currency, accountType, businessType)
// 全局，足以支撑主流量 8 亿用户上限。总 max = 1.999e18 不溢 int64。
//
// **CreateAccount 时业务侧必须校验**：accountType ≤ 99 且 businessType ≤ 9999；
// EncodeAccountID 在底层兜底校验，业务层提前 reject 给更友好错误。
//
// 调用模式：accounting-system 拿到 user 路由后的 globalTblIdx + leaf-segment idgen 的 seq，
// 用 EncodeAccountID(ctx, currency, accountType, globalTblIdx, businessType, seq) 拼最终 ID 落表。
// 读取侧 DecodeAccountID 解析 5 个字段；AccountIDDBIndex / AccountIDTableIndex 直接给路由。
// 位段乘数（10^"低于本字段的总位宽"），用于值 → 位段拼接 / 解析。
//
// layout 从低到高（bit 1 → bit 19）：
//
//	seq           bit 1-7   (7 位) → 乘数 1
//	business      bit 8-11  (4 位) → 乘数 1e7
//	globalTblIdx  bit 12-13 (2 位) → 乘数 1e11
//	accountType   bit 14-15 (2 位) → 乘数 1e13
//	currency      bit 16-18 (3 位) → 乘数 1e15
//	shadow flag   bit 19    (1 位) → 偏移 1e18
const (
	accountIDBusinessMul    int64 = 10_000_000                // 1e7
	accountIDGlobalTblMul   int64 = 100_000_000_000           // 1e11
	accountIDAccountTypeMul int64 = 10_000_000_000_000        // 1e13
	accountIDCurrencyMul    int64 = 1_000_000_000_000_000     // 1e15
	accountIDShadowMul      int64 = 1_000_000_000_000_000_000 // 1e18

	// 各字段值上界
	accountIDSeqMax         int64 = 9_999_999 // 7 位最大（10M per (currency,type,gtbl,biz) 组合）
	accountIDBusinessMax    int64 = 9_999     // 4 位
	accountIDGlobalTblMax   int64 = 99        // 2 位（10×10=100 shard 上限）
	accountIDAccountTypeMax int64 = 99        // 2 位
	accountIDCurrencyMax    int64 = 999       // 3 位（ISO 4217 上限）
)

// V2 layout —— 1000 shard 升级（reshard SOP Phase 1）。
//
// 设计目标：与 V1 完全前向兼容（V1 ID 继续走 V1 解码），并在 int64 内为
// V2 留出独立编码空间。
//
// V2 layout（payload 在 [0, 1e18) 之内，与 V1 不重叠）：
//
//	seq           bit 1-7   (7 位)  → 乘数 1
//	business      bit 8-11  (4 位)  → 乘数 1e7
//	globalTblIdx  bit 12-14 (3 位)  → 乘数 1e11   ← UPGRADED 2→3 位 (max 999)
//	accountType   bit 15    (1 位)  → 乘数 1e14   ← compressed 2→1 位 (max 9 已足)
//	currency      bit 16-18 (3 位)  → 乘数 1e15
//
// 高位 flag（与 V1 共存）：
//
//	bit 19  → 偏移 1e18  shadow flag        (V1 / V2 共用)
//	bit 20  → 偏移 2e18  layout version=2  (NEW: V1 = 0, V2 = 1)
//
// V2 编码值范围:
//
//	V2 main:   accountID ∈ [2e18, 3e18)  // version=1, shadow=0
//	V2 shadow: accountID ∈ [3e18, 4e18)  // version=1, shadow=1
//
// V1 同样占 [0, 2e18)，所以 4e18 是 V2 上限，距 int64 max (9.22e18) 充足余量。
//
// **accountType 压缩** 1→1 位：当前所有 AccountType 常量值都在 [1, 9]，1 位足够。
// 任何新加 AccountType 必须 ≤ 9 才能用 V2 编码（V1 仍支持 0-99）。
const (
	accountIDV2BusinessMul    int64 = 10_000_000                // 1e7
	accountIDV2GlobalTblMul   int64 = 100_000_000_000           // 1e11
	accountIDV2AccountTypeMul int64 = 100_000_000_000_000       // 1e14
	accountIDV2CurrencyMul    int64 = 1_000_000_000_000_000     // 1e15
	accountIDV2VersionMul     int64 = 2_000_000_000_000_000_000 // 2e18 (V2 marker)

	accountIDV2SeqMax         int64 = 9_999_999 // 7 位 (10M per combo)
	accountIDV2BusinessMax    int64 = 9_999     // 4 位
	accountIDV2GlobalTblMax   int64 = 999       // 3 位 (1000 shard 上限)
	accountIDV2AccountTypeMax int64 = 9         // 1 位
	accountIDV2CurrencyMax    int64 = 999       // 3 位
)

// AccountID layout 字段类型常量（与 accounting-system AccountType 字典对齐）。
const (
	AccountTypeUSER             = 1 // 用户主账户
	AccountTypeMERCHANT         = 2 // 商户账户
	AccountTypeMERCHANTPENDING  = 3 // 商户待结算
	AccountTypePLATFORM         = 4 // 平台损益
	AccountTypeTRANSITRECEIVE   = 5 // 中间渠道应收 (fleet)
	AccountTypeTRANSITPAYABLE   = 6 // 中间渠道应付
	AccountTypeTRANSACTIONFEE   = 7 // 手续费
	AccountTypeCHARGEFEE        = 8 // 服务费
	AccountTypeTRANSIT          = 9 // 平台中间账户
)

// EncodeAccountID 把 (currency, accountType, globalTableIdx, businessType, seq) + ctx.IsShadow
// 编码成 19 位十进制 int64 account_id。
//
// 出错条件：任何字段超界。
//
// **globalTableIdx 必须等于 user_id 路由后的 globalTableIdx**（router.RouteByUserID 返回值），
// 这样 RouteByAccountNo 直接从 account_id 数值上拿到分片，不需要外部映射 →
// 保证"用户的所有账户在用户那一片"的强约束（TCC try/confirm/cancel 单分片事务前提）。
//
// currency 是 ISO 4217 数字代码（不是字符串 "PHP"）；调用方用
// payment-util/money.NumericCode("PHP") 之类先转换。
func EncodeAccountID(ctx context.Context, currency, accountType, globalTableIdx, businessType int, seq int64) (int64, error) {
	if currency < 0 || int64(currency) > accountIDCurrencyMax {
		return 0, fmt.Errorf("currency %d out of range [0, %d]", currency, accountIDCurrencyMax)
	}
	if accountType < 0 || int64(accountType) > accountIDAccountTypeMax {
		return 0, fmt.Errorf("account_type %d out of range [0, %d]", accountType, accountIDAccountTypeMax)
	}
	if globalTableIdx < 0 || int64(globalTableIdx) > accountIDGlobalTblMax {
		return 0, fmt.Errorf("global_table_idx %d out of range [0, %d]", globalTableIdx, accountIDGlobalTblMax)
	}
	if businessType < 0 || int64(businessType) > accountIDBusinessMax {
		return 0, fmt.Errorf("business_type %d out of range [0, %d]", businessType, accountIDBusinessMax)
	}
	if seq <= 0 || seq > accountIDSeqMax {
		return 0, fmt.Errorf("seq %d out of range [1, %d]", seq, accountIDSeqMax)
	}
	id := seq +
		int64(businessType)*accountIDBusinessMul +
		int64(globalTableIdx)*accountIDGlobalTblMul +
		int64(accountType)*accountIDAccountTypeMul +
		int64(currency)*accountIDCurrencyMul
	if IsShadow(ctx) {
		id += accountIDShadowMul
	}
	return id, nil
}

// AccountIDLayoutVersion 根据 account_id 数值识别 layout 版本。
//
//	返回 1 → V1 (现役)
//	返回 2 → V2 (1000-shard 升级，bit 20 = 1)
//
// 探测规则：accountID >= 2e18 即 V2，否则 V1。读取性能 O(1)。
func AccountIDLayoutVersion(accountID int64) int {
	if accountID >= accountIDV2VersionMul {
		return 2
	}
	return 1
}

// EncodeAccountIDV2 V2 编码：与 EncodeAccountID 同语义，但 globalTblIdx 支持
// 0-999（V1 是 0-99），accountType 限制 0-9（V1 是 0-99，V2 压缩 1 位让出 globalTblIdx）。
//
// **使用前置条件**：
//   1. 调用方已经把 globalTblIdx 路由到 V2 路由器（10×100 layout，0-999）
//   2. accountType ≤ 9（当前所有 AccountType 常量都符合）
//   3. CurrentLayoutVersion = LayoutV2（写入端协调好才能产生 V2 ID）
//
// **不做的事**：
//   - 不自动转换 V1 → V2 (历史 ID 数据迁移走单独的 reshard worker)
//   - 不校验 router 是否真的是 V2 (caller 责任)
func EncodeAccountIDV2(ctx context.Context, currency, accountType, globalTableIdx, businessType int, seq int64) (int64, error) {
	if currency < 0 || int64(currency) > accountIDV2CurrencyMax {
		return 0, fmt.Errorf("V2 currency %d out of range [0, %d]", currency, accountIDV2CurrencyMax)
	}
	if accountType < 0 || int64(accountType) > accountIDV2AccountTypeMax {
		return 0, fmt.Errorf("V2 account_type %d out of range [0, %d] (V2 layout 1-digit limit; V1 supports 0-99)",
			accountType, accountIDV2AccountTypeMax)
	}
	if globalTableIdx < 0 || int64(globalTableIdx) > accountIDV2GlobalTblMax {
		return 0, fmt.Errorf("V2 global_table_idx %d out of range [0, %d]", globalTableIdx, accountIDV2GlobalTblMax)
	}
	if businessType < 0 || int64(businessType) > accountIDV2BusinessMax {
		return 0, fmt.Errorf("V2 business_type %d out of range [0, %d]", businessType, accountIDV2BusinessMax)
	}
	if seq <= 0 || seq > accountIDV2SeqMax {
		return 0, fmt.Errorf("V2 seq %d out of range [1, %d]", seq, accountIDV2SeqMax)
	}
	id := seq +
		int64(businessType)*accountIDV2BusinessMul +
		int64(globalTableIdx)*accountIDV2GlobalTblMul +
		int64(accountType)*accountIDV2AccountTypeMul +
		int64(currency)*accountIDV2CurrencyMul
	if IsShadow(ctx) {
		id += accountIDShadowMul
	}
	id += accountIDV2VersionMul // V2 marker
	return id, nil
}

// DecodeAccountID 从 layout-编码的 account_id 解出 5 个字段。
// 自动按 V1/V2 layout 版本分发。
//
// 不需要 ctx；纯数值解析（也可独立用于跨服务回包）。
func DecodeAccountID(accountID int64) (isShadow bool, currency, accountType, globalTableIdx, businessType int, seq int64) {
	version := AccountIDLayoutVersion(accountID)
	// 剥离 version 标志位
	if version == 2 {
		accountID -= accountIDV2VersionMul
	}
	// 剥离 shadow 标志位
	if accountID >= accountIDShadowMul {
		isShadow = true
		accountID -= accountIDShadowMul
	}
	if version == 2 {
		// V2 layout
		currency = int(accountID / accountIDV2CurrencyMul)
		accountID %= accountIDV2CurrencyMul
		accountType = int(accountID / accountIDV2AccountTypeMul)
		accountID %= accountIDV2AccountTypeMul
		globalTableIdx = int(accountID / accountIDV2GlobalTblMul)
		accountID %= accountIDV2GlobalTblMul
		businessType = int(accountID / accountIDV2BusinessMul)
		accountID %= accountIDV2BusinessMul
		seq = accountID
		return
	}
	// V1 layout (legacy)
	currency = int(accountID / accountIDCurrencyMul)
	accountID %= accountIDCurrencyMul
	accountType = int(accountID / accountIDAccountTypeMul)
	accountID %= accountIDAccountTypeMul
	globalTableIdx = int(accountID / accountIDGlobalTblMul)
	accountID %= accountIDGlobalTblMul
	businessType = int(accountID / accountIDBusinessMul)
	accountID %= accountIDBusinessMul
	seq = accountID
	return
}

// AccountIDGlobalTableIdx 直接从 account_id 拿 globalTblIdx，自动按 layout 版本分发。
//
//	V1 → 0-99   (2 位)
//	V2 → 0-999  (3 位)
//
// 比 DecodeAccountID 便宜：纯位段除模，不解其他字段。
func AccountIDGlobalTableIdx(accountID int64) int {
	version := AccountIDLayoutVersion(accountID)
	if version == 2 {
		accountID -= accountIDV2VersionMul
		if accountID >= accountIDShadowMul {
			accountID -= accountIDShadowMul
		}
		accountID %= accountIDV2AccountTypeMul
		return int(accountID / accountIDV2GlobalTblMul)
	}
	if accountID >= accountIDShadowMul {
		accountID -= accountIDShadowMul
	}
	accountID %= accountIDAccountTypeMul
	return int(accountID / accountIDGlobalTblMul)
}

// AccountIDDBIndex 给 sharding router 用：从 account_id 算 dbIdx。
//
//	V1: dbCount=10, tablePerDB=10, globalTblIdx ∈ [0, 99]   → dbIdx = globalTbl / 10
//	V2: dbCount=10, tablePerDB=100, globalTblIdx ∈ [0, 999] → dbIdx = globalTbl / 100
//
// **注意**：V2 router 的 (dbCount, tablePerDB) 必须严格匹配 (10, 100) — 见
// accounting-system/internal/infrastructure/sharding/router_v2.go ShardDBCountV2 / ShardTablePerDBV2。
func AccountIDDBIndex(accountID int64) int {
	gtbl := AccountIDGlobalTableIdx(accountID)
	if AccountIDLayoutVersion(accountID) == 2 {
		return gtbl / 100 // V2 tablePerDB = 100
	}
	return gtbl / 10 // V1 tablePerDB = 10
}

// AccountIDTableIndex 给 sharding router 用：返回 globalTblIdx，与表名后缀对应。
//
//	V1 → 0-99   (表名后缀 "_NN")
//	V2 → 0-999  (表名后缀 "_NN_NNN" by RouterV2.TableName)
func AccountIDTableIndex(accountID int64) int {
	return AccountIDGlobalTableIdx(accountID)
}

// IsShadowAccountIDLayout 自动按 V1/V2 layout 取 shadow flag (bit 19)。
//
// 实现：(accountID / 1e18) % 2 — 在 V1/V2 上均成立：
//
//	V1 main:   accountID < 1e18              → 0 % 2 = 0 → false ✓
//	V1 shadow: accountID ∈ [1e18, 2e18)      → 1 % 2 = 1 → true  ✓
//	V2 main:   accountID ∈ [2e18, 3e18)      → 2 % 2 = 0 → false ✓
//	V2 shadow: accountID ∈ [3e18, 4e18)      → 3 % 2 = 1 → true  ✓
func IsShadowAccountIDLayout(accountID int64) bool {
	return (accountID/accountIDShadowMul)%2 == 1
}

// ValidateAccountIDLayout 双向校验 layout-编码的 account_id 与 ctx 段一致：
//   - shadow ctx + 高位 1 → ok
//   - 主 ctx + 高位 0 → ok
//   - 错配 → error
//
// 同时校验 5 个字段都在合法范围内（防止外部传入垃圾值绕过 Encode 校验）。
func ValidateAccountIDLayout(ctx context.Context, accountID int64) error {
	if accountID < 0 {
		return fmt.Errorf("account_id %d is negative", accountID)
	}
	isShadow, currency, accountType, globalTableIdx, businessType, seq := DecodeAccountID(accountID)
	if isShadow != IsShadow(ctx) {
		return fmt.Errorf("account_id %d shadow=%v mismatches ctx shadow=%v",
			accountID, isShadow, IsShadow(ctx))
	}
	if int64(currency) > accountIDCurrencyMax {
		return fmt.Errorf("account_id %d: currency=%d out of range", accountID, currency)
	}
	if int64(accountType) > accountIDAccountTypeMax {
		return fmt.Errorf("account_id %d: account_type=%d out of range [0, %d]", accountID, accountType, accountIDAccountTypeMax)
	}
	if int64(globalTableIdx) > accountIDGlobalTblMax {
		return fmt.Errorf("account_id %d: global_table_idx=%d out of range [0, %d]", accountID, globalTableIdx, accountIDGlobalTblMax)
	}
	if int64(businessType) > accountIDBusinessMax {
		return fmt.Errorf("account_id %d: business_type=%d out of range [0, %d]", accountID, businessType, accountIDBusinessMax)
	}
	if seq <= 0 || seq > accountIDSeqMax {
		return fmt.Errorf("account_id %d: seq=%d out of range [1, %d]", accountID, seq, accountIDSeqMax)
	}
	return nil
}

// ─── 通用业务 ID 编码（非账户型） ─────────────────────────────────────────────
//
// 全 monorepo 所有非账户型 ID（voucher、transaction、freeze、gltxn、dispute、
// inbound webhook、outbox、merchant_id、risk decision 等）一律走本 layout。
//
// 19 位 int64 layout（从低到高）：
//
//	seq          bit 1-13 (13 位) → 乘数 1
//	globalTbl    bit 14-15 (2 位) → 乘数 1e13   (无 shard 关系填 00)
//	idType       bit 16-18 (3 位) → 乘数 1e15   (000-999 全局枚举，按系统分段)
//	shadow flag  bit 19   (1 位)  → 偏移 1e18
//
// **idType 段位规划**（每系统 100 个号，留扩展空间）：
//
//	000-099 accounting-system    (001=voucher, 002=transaction, 003=freeze_tx, 004=request_id)
//	100-199 order-core           (110=gltxn, 111=dispute, 112=inbound_webhook,
//	                              113=accounting_outbox, 114=mkd_doc, 199=evt_test)
//	200-299 payment-channel      (200=acquirer_tx)
//	300-399 user-merchant-core   (300=merchant_id, 301=mkd_doc, 302=user_account_name)
//	400-499 risk-manage          (400=decision_id)
//	500-599 payment-core         (TBD)
//	600-699 reconplatform        (TBD)
//	700-799 api-gateway          (TBD - 预留)
//	800-899 kms-manage           (TBD - 预留)
//	900-999 id-generator + 通用  (TBD)

const (
	idTypeMul        int64 = 1_000_000_000_000_000     // 1e15
	idTypeGlobalMul  int64 = 10_000_000_000_000        // 1e13
	idTypeShadowMul  int64 = 1_000_000_000_000_000_000 // 1e18

	idTypeMax        int64 = 999             // 3 位
	idTypeGlobalMax  int64 = 99              // 2 位
	idTypeSeqMax     int64 = 9_999_999_999_999 // 13 位 (10T per (idType, globalTbl) 组合)
)

// 全局 idType 枚举（按系统分段 000-999）。新加 ID 类型时往下追加，不要复用已分配号。
const (
	// accounting-system 段 000-099
	IDTypeVoucher        = 1 // accounting voucher_no
	IDTypeTransaction    = 2 // accounting account_transaction
	IDTypeFreezeTx       = 3 // accounting freeze transaction
	IDTypeRequestID      = 4 // accounting facade request_id
	IDTypeBatchOrder     = 5 // accounting batch order

	// order-core 段 100-199
	IDTypeOrderPI            = 100 // payment_intent
	IDTypeOrderCharge        = 101 // charge
	IDTypeOrderRefund        = 102 // refund
	IDTypeOrderGLTxn         = 110 // general ledger transaction
	IDTypeOrderDispute       = 111 // dispute
	IDTypeOrderInboundWebhook = 112 // inbound webhook event
	IDTypeOrderAccountingOutbox = 113 // accounting outbox row
	IDTypeOrderKYCDoc        = 114 // merchant KYC doc (order-core 端)
	IDTypeOrderEvtTest       = 199 // 测试用 webhook event ID

	// payment-channel 段 200-299
	IDTypeChannelAcquirerTx  = 200 // 渠道 acquirer 流水

	// user-merchant-core 段 300-399
	IDTypeMerchantID         = 300 // merchant ID（公开暴露的"mch_<seq>"）
	IDTypeMerchantKYCDoc     = 301 // merchant KYC doc (user-merchant-core 端)
	IDTypeUserAccountName    = 302 // user account display name

	// risk-manage 段 400-499
	IDTypeRiskDecision       = 400 // risk decision_id

	// 后续保留：500-999 给 payment-core / reconplatform / api-gateway / kms / id-generator
)

// EncodeID 通用 ID 编码（非账户型）。
//
//   - idType: 全局枚举（IDTypeXxx），3 位（000-999）
//   - globalTableIdx: 0-99；该 ID 不需要分片路由时填 0
//   - seq: leaf-segment idgen 出的 seq，必须 ≥ 1
//   - shadow: 来自 ctx (IsShadow)，自动加 1e18 高位
//
// 出错条件：任何字段超界。
func EncodeID(ctx context.Context, idType, globalTableIdx int, seq int64) (int64, error) {
	if idType < 0 || int64(idType) > idTypeMax {
		return 0, fmt.Errorf("idType %d out of range [0, %d]", idType, idTypeMax)
	}
	if globalTableIdx < 0 || int64(globalTableIdx) > idTypeGlobalMax {
		return 0, fmt.Errorf("global_table_idx %d out of range [0, %d]", globalTableIdx, idTypeGlobalMax)
	}
	if seq <= 0 || seq > idTypeSeqMax {
		return 0, fmt.Errorf("seq %d out of range [1, %d]", seq, idTypeSeqMax)
	}
	id := seq +
		int64(globalTableIdx)*idTypeGlobalMul +
		int64(idType)*idTypeMul
	if IsShadow(ctx) {
		id += idTypeShadowMul
	}
	return id, nil
}

// EncodeIDStr 同 EncodeID 但直接返回十进制字符串，方便存到 VARCHAR 列。
func EncodeIDStr(ctx context.Context, idType, globalTableIdx int, seq int64) (string, error) {
	id, err := EncodeID(ctx, idType, globalTableIdx, seq)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

// DecodeID 从通用 ID 解出字段。
func DecodeID(id int64) (isShadow bool, idType, globalTableIdx int, seq int64) {
	if id >= idTypeShadowMul {
		isShadow = true
		id -= idTypeShadowMul
	}
	idType = int(id / idTypeMul)
	id %= idTypeMul
	globalTableIdx = int(id / idTypeGlobalMul)
	id %= idTypeGlobalMul
	seq = id
	return
}

// IDDBIndex 给 sharding router 用（仅当 idType 的存储是 shard 化的）。
// 与 AccountIDDBIndex 区分；layout 不同（idType 占用了 currency/accountType 的位段）。
func IDDBIndex(id int64) int {
	if id >= idTypeShadowMul {
		id -= idTypeShadowMul
	}
	id %= idTypeMul
	return int(id/idTypeGlobalMul) / 10
}

// IDTableIndex 同 IDDBIndex，返回 globalTblIdx (0-99)。
func IDTableIndex(id int64) int {
	if id >= idTypeShadowMul {
		id -= idTypeShadowMul
	}
	id %= idTypeMul
	return int(id / idTypeGlobalMul)
}
