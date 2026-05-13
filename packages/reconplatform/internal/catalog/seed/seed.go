// Package seed — 启动时把内建 catalog 规则 (.star) auto-register 到 script.Store.
//
// 工作流:
//
//  1. //go:embed *.star 把规则源码打进二进制
//  2. 服务启动后调 SeedBuiltins(ctx, store, logger, "system:seeder"):
//     - 对每条规则,检查 store 里是否已存在 (按 id)
//     - 不存在 → SaveDef 创建
//     - 存在且未被用户改过 → 比较 hash,版本不一致自动升级
//     - 存在且用户改过 → 跳过(保留个性化)
//
// 升级路径:
//
//   每条规则的 .star 文件第一行带:
//      # meta-version: N
//   N 增加 → seeder 会比对当前 store 里的 code hash,不一致就升级.
//   用户在 admin UI 改过规则 → store 里的 UpdatedBy != "system:..." → 跳过升级,
//   尊重用户定制。要强制刷回内建版,在 admin UI 删了重新种入即可。
package seed

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"reconcile-system/internal/script"
)

//go:embed *.star
var builtinFS embed.FS

// Rule 一条内建规则的元数据 (.star 文件外的属性).
type Rule struct {
	ID          string   // 与 .star 文件名 (去 .star) 一致
	Name        string   // 人类可读名 (UI 显示)
	Description string   // 一句话说明
	Severity    string   // critical / warning / info
	Schedule    string   // cron 表达式,空 = 仅手动触发
	Triggers    []string // 事件驱动触发: ["payment-channel:acquirer_tx", ...]
}

// BuiltinRules 三方对账之 + 经典 catalog 规则元数据.
//
// 顺序决定 UI catalog 页的展示顺序;靠前 = 越关键。
//
// 升级新规则:
//   1. 在 internal/catalog/scripts/<id>.star 写代码
//   2. 在本列表加一条 Rule{...}
//   3. 重启 admin 即自动 seed
var BuiltinRules = []Rule{
	{
		ID:          "order_in_channel",
		Name:        "order → channel 存在性",
		Description: "order PI succeeded 但 payment-channel 无 acquirer_tx — 渠道路由失败/webhook 丢",
		Severity:    "critical",
		Schedule:    "*/5 * * * *",
		Triggers:    []string{"order-core:payment_intent"},
	},
	{
		ID:          "channel_in_accounting",
		Name:        "channel → accounting 存在性",
		Description: "渠道已扣款但 accounting 未记账 — outbox 未投递或 consumer 卡住,资损前兆",
		Severity:    "critical",
		Schedule:    "*/5 * * * *",
		Triggers:    []string{"payment-channel:acquirer_tx"},
	},
	{
		ID:          "three_way_amount",
		Name:        "三方金额相等",
		Description: "order.pi.amount == channel.tx.amount == accounting.ledger.amount, 任一不等 = 资损",
		Severity:    "critical",
		Schedule:    "*/10 * * * *",
		Triggers:    []string{"order-core:payment_intent"},
	},
	{
		ID:          "three_way_status",
		Name:        "三方状态一致",
		Description: "PI 终态与 channel/accounting 一致,失配 = 用户被错扣或显示假成功",
		Severity:    "critical",
		Schedule:    "*/5 * * * *",
		Triggers:    []string{"order-core:payment_intent"},
	},
	{
		ID:          "orphan_channel_tx",
		Name:        "孤儿 channel tx",
		Description: "渠道有 tx 但 order-core 无 PI — 业务侧需认领",
		Severity:    "warning",
		Schedule:    "*/15 * * * *",
		Triggers:    []string{"payment-channel:acquirer_tx"},
	},
	{
		ID:          "orphan_accounting_entry",
		Name:        "孤儿 accounting 账目",
		Description: "账目记了但渠道无对应 tx — 手工调账或重复 outbox 投递",
		Severity:    "warning",
		Schedule:    "*/15 * * * *",
		Triggers:    []string{"accounting-system:ledger_entry"},
	},
	{
		ID:          "refund_three_way",
		Name:        "退款三方对账",
		Description: "order.refund → channel.refund → accounting.reverse 三方齐 + 金额相等",
		Severity:    "critical",
		Schedule:    "*/5 * * * *",
		Triggers:    []string{"order-core:refund_request"},
	},
	{
		ID:          "three_way_sync_lag",
		Name:        "三方同步滞后",
		Description: "channel succeeded 后 N 秒内 order/accounting 未到位 (Kafka MM 抖动 / consumer 卡)",
		Severity:    "warning",
		Schedule:    "*/5 * * * *",
		Triggers:    []string{"payment-channel:acquirer_tx"},
	},
}

// SeedBuiltins 把内建规则灌进 store. 重复调安全 (idempotent).
//
// actor 在 audit-log 里记 "由谁种入",通常传 "system:seeder".
// 返已 seed 的规则数 (新建 + 升级合计).
func SeedBuiltins(ctx context.Context, store *script.Store, logger *zap.Logger, actor string) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("nil store")
	}
	if actor == "" {
		actor = "system:seeder"
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	files, err := loadAllStarFiles()
	if err != nil {
		return 0, fmt.Errorf("load embedded .star: %w", err)
	}

	seeded := 0
	for _, r := range BuiltinRules {
		code, ok := files[r.ID+".star"]
		if !ok {
			logger.Warn("seed: source missing", zap.String("rule", r.ID))
			continue
		}

		existing, _ := store.LoadDef(ctx, r.ID)
		if existing != nil {
			if sha256hash(existing.Code) == sha256hash(code) {
				continue // 完全一致,无需升级
			}
			if isUserModified(existing) {
				logger.Info("seed: rule modified by user, skip upgrade",
					zap.String("rule", r.ID),
					zap.String("last_updated_by", existing.UpdatedBy))
				continue
			}
			if _, err := store.SaveDef(ctx, r.ID, r.Name, code, r.Schedule, r.Triggers, actor); err != nil {
				logger.Warn("seed: upgrade failed", zap.String("rule", r.ID), zap.Error(err))
				continue
			}
			seeded++
			logger.Info("seed: upgraded builtin rule",
				zap.String("rule", r.ID),
				zap.String("severity", r.Severity))
			continue
		}
		if _, err := store.SaveDef(ctx, r.ID, r.Name, code, r.Schedule, r.Triggers, actor); err != nil {
			logger.Warn("seed: create failed", zap.String("rule", r.ID), zap.Error(err))
			continue
		}
		seeded++
		logger.Info("seed: created builtin rule",
			zap.String("rule", r.ID),
			zap.String("severity", r.Severity))
	}
	logger.Info("seed: complete", zap.Int("seeded", seeded), zap.Int("total", len(BuiltinRules)))
	return seeded, nil
}

// loadAllStarFiles 把 embed FS 里的 *.star 全部读进 map.
func loadAllStarFiles() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(builtinFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".star") {
			return nil
		}
		body, err := builtinFS.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.Base(path)] = string(body)
		return nil
	})
	return out, err
}

// sha256hash 算 hash 用于版本比对.
func sha256hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// isUserModified 检查 script.Script.UpdatedBy 是不是 system: 开头.
//
// 若 last update 是 system:*,自动升级安全;否则用户改过,跳过.
func isUserModified(s *script.Script) bool {
	if s == nil {
		return false
	}
	return s.UpdatedBy != "" && !strings.HasPrefix(s.UpdatedBy, "system:")
}

// MustValidate 自检 (启动期):BuiltinRules 列表里每条规则都有对应 .star 文件.
// 没对应文件 → panic, 编译期就该发现, 防 release 时 catalog 缺规则。
func MustValidate() {
	files, err := loadAllStarFiles()
	if err != nil {
		panic(fmt.Sprintf("seed: load embed FS: %v", err))
	}
	missing := []string{}
	for _, r := range BuiltinRules {
		if _, ok := files[r.ID+".star"]; !ok {
			missing = append(missing, r.ID)
		}
	}
	if len(missing) > 0 {
		panic(fmt.Sprintf("seed: missing .star files for: %v", missing))
	}
}
