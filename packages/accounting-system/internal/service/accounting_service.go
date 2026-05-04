package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	commonutil "github.com/accounting-system/internal/common"
	"github.com/accounting-system/internal/currency"
	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/idgen"
	"github.com/accounting-system/internal/infrastructure/cache"
	"github.com/accounting-system/internal/infrastructure/database"
	kafkamq "github.com/accounting-system/internal/infrastructure/kafka"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/metrics"
	"github.com/accounting-system/internal/repository"
	"github.com/xiongwp/payment-util/money"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ─── 常量 ─────────────────────────────────────────────────────────────────────

const (
	// maxMarkRedisDoneConcurrency MarkRedisDone 异步 goroutine 最大并发数
	maxMarkRedisDoneConcurrency = 32
	// markRedisDoneTimeout MarkRedisDone 单次调用的超时时间
	markRedisDoneTimeout = 3 * time.Second
	// warmAccountsTimeout 账户预热操作的总超时时间
	warmAccountsTimeout = 10 * time.Second
	// idempotentMaxAttempts 幂等锁重试最大次数
	idempotentMaxAttempts = 3
)

// AccountingEntry 记账条目
// DebitAmount/CreditAmount 单位：ISO 最小货币单位 × 100（参见 currency 包）。
// 例如 USD $3.42 → 34200；JPY ¥342 → 34200。
type AccountingEntry struct {
	AccountNo    string
	DebitAmount  int64
	CreditAmount int64
	Description  string
}

// DoubleEntryBookingRequest 复式记账请求
type DoubleEntryBookingRequest struct {
	RequestID    string // 幂等键（调用方通过 gRPC metadata x-request-id 提供，必填）
	BusinessNo   string
	BusinessType model.BusinessType
	Entries      []AccountingEntry
	Currency     string
	Description  string
}

// AtomicBatchBookingRequest 原子批量记账请求
// 批次内所有单笔记账通过 TCC 保证整体原子性：全部成功 or 全部回滚。
// 请求参数在执行前全量持久化到 batch_order 表，保证崩溃后可审计。
type AtomicBatchBookingRequest struct {
	BatchRequestID  string                      // 批次幂等键（必填）
	BatchBusinessNo string                      // 批次业务号（用于 batch_order 路由/查询）
	Requests        []DoubleEntryBookingRequest // 各单笔记账（每笔有独立 RequestID）
	Description     string
}

// AtomicBatchBookingResult 原子批量记账结果
type AtomicBatchBookingResult struct {
	BatchID    string                   // 批次ID（= BatchRequestID）
	AllSuccess bool                     // 全部成功时为 true
	Items      []BatchBookingItemResult // 各单笔结果
}

// BatchBookingItemResult 单笔记账结果
type BatchBookingItemResult struct {
	RequestID string
	VoucherNo string
	TxIDs     []string
	Err       error
}

// AccountingService 记账服务接口
type AccountingService interface {
	// DoubleEntryBooking 复式记账（预生成 ID + 参数持久化 + 热路径/TCC 自动路由）
	DoubleEntryBooking(ctx context.Context, req *DoubleEntryBookingRequest) (voucherNo string, transactionIDs []string, err error)

	// HybridDoubleEntryBooking 等同于 DoubleEntryBooking，显式标注混合路由语义
	// 热路径（Redis）优先，miss 时自动切换 TCC 冷路径
	// 请求参数 + 预生成 ID 在订单创建时持久化，重试时复用，保证完整幂等
	HybridDoubleEntryBooking(ctx context.Context, req *DoubleEntryBookingRequest) (voucherNo string, transactionIDs []string, idempotentHit bool, err error)

	// AtomicBatchBooking 原子批量记账：全部成功或全部回滚
	// 执行前先保存批次请求参数到 batch_order 表，再执行 TCC 多分支事务
	AtomicBatchBooking(ctx context.Context, req *AtomicBatchBookingRequest) (*AtomicBatchBookingResult, error)

	// CreateAccount 创建账户
	// userId + accountBusinessType 唯一键，重复时幂等返回已有账户
	// CreateAccount 创建用户 / 商户 / 商户待结算账户（业务账户）。
	// owner_id 必须落在对应业务区间，否则报错。平台/中间/手续费类型必须用
	// CreatePlatformAccount，不能通过本方法创建。
	CreateAccount(ctx context.Context, userID int64, accountBusinessType model.AccountBusinessType, accountType model.AccountType, category model.AccountCategory, currency string) (*model.Account, error)

	// CreatePlatformAccount 创建单个系统内部账户（平台 / 中间 / 手续费 / 权益 / 中转）。
	// reservedID ∈ [1, ReservedOwnerIDMax]；business_type / category 由 accountType 1:1 推导。
	// 适合单点创建；渠道级批量注册请用 CreatePlatformAccountFleet。
	CreatePlatformAccount(ctx context.Context, reservedID int64, accountType model.AccountType, currency string) (*model.Account, error)

	// CreatePlatformAccountFleet 为一个渠道批量创建 100 个系统内部账户（每分片表一个）+
	// 在 account_business_type_info 登记一条渠道注册。
	//
	// 调用方提供一个 CreatePlatformChannelRequest，见该类型的字段注释。
	// 返回 CreatePlatformChannelResult 包含：分配到的 business_type 数字码、100 个分片账户。
	//
	// 业务类型数字码（ChannelBusinessType）刻意不用 Go enum，避免每新增一个渠道就要改
	// 代码 + 发版。接口传 int，范围 1-999 的任意值合法（0 = 让服务端自动分配）。
	CreatePlatformAccountFleet(ctx context.Context, req *CreatePlatformChannelRequest) (*CreatePlatformChannelResult, error)

	// ListBusinessTypes 返回 account_business_type_info 的所有注册记录（channel registry）。
	// 供 admin-web 渲染"业务类型列表"页面。
	ListBusinessTypes(ctx context.Context) ([]*model.AccountBusinessTypeInfo, error)

	// ReloadRegistry 从 meta DB 全量加载 account_business_type_info + account_type_info
	// 到本地进程缓存。启动时调一次，之后 admin 新增 business_type 时通过扇出
	// POST /admin/reload/business-types 触发所有实例重新 load。
	ReloadRegistry(ctx context.Context) error

	// RetryStuckConfirmingTcc 修复 TCC CONFIRMING 半挂起状态。
	//
	// 触发场景：进程在 phase=CONFIRMING 阶段崩溃 / DB 中断，部分 branch 已 CONFIRMED
	// （原 tx 已 commit），其他 branch 仍 TRYING。已 commit 的不能 cancel（金额已变），
	// TccRecoveryWorker 看到 CONFIRMING 也不会 cancel（注释里说"需 RetryConfirmTcc"）。
	// 这里就是那个 RetryConfirmTcc。
	//
	// 行为：扫描 phase=CONFIRMING 且 updated_at < before 的 coordinator → 列出所有
	// branch → 对仍 TRYING 的 branch 重新 tccConfirm（用 deltaKnown=false 路径，从
	// branch.balance_delta 恢复）→ 全部 CONFIRMED 后协调者 transition → CONFIRMED。
	//
	// 返回：成功修复的 coordinator 数（不计跳过 / 错误）。
	RetryStuckConfirmingTcc(ctx context.Context, before time.Time) (int, error)


	// RegisterBusinessType 仅向 account_business_type_info 写入一条业务类型登记，
	// **不创建任何账户**（账户创建走 CreatePlatformAccountFleet）。
	//
	// 参数:
	//   accountType      绑定的 AccountType；category 由此派生（不存表）
	//   businessTypeCode 业务码名（unique key），如 ALIPAY_RECEIVABLE
	//   description      可选描述
	//   businessType     业务类型数字码；传 0 让服务端自动从 [101, 999] 分配
	RegisterBusinessType(ctx context.Context, accountType model.AccountType, businessTypeCode, description string, businessType int) (*model.AccountBusinessTypeInfo, error)

	// ListPlatformAccountsByBusinessType 返回指定 (business_type, currency) 对应的 fleet 账户（含当前余额）。
	// 用 user_id 0..99 并发查询对应分片；找不到的静默跳过（可能尚未 fleet-create 完成）。
	// currency 必填——同 business_type 不同币种各 100 账户，汇总没意义。
	ListPlatformAccountsByBusinessType(ctx context.Context, businessType model.AccountBusinessType, currency string) ([]*model.Account, error)

	// ListPlatformSnapshotsByBusinessType 返回 (business_type, currency) 在 cutDate 的快照。
	// 每条结果包含账户信息 + 快照（若该账户当日无快照，snapshot 为 nil）。currency 必填。
	ListPlatformSnapshotsByBusinessType(ctx context.Context, businessType model.AccountBusinessType, cutDate, currency string) ([]*PlatformAccountSnapshot, error)

	// GetAccount 根据账户号查询账户
	GetAccount(ctx context.Context, accountNo string) (*model.Account, error)

	// GetAccountByUserAndBusinessType 根据 userId + businessType 查询账户
	// 注意：多币种用户会有多条记录；本方法仅返回首条。要拿全部币种用
	// ListAccountsByUserAndBusinessType。
	GetAccountByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType) (*model.Account, error)

	// ListAccountsByUserAndBusinessType 返回该 (userID, businessType) 下全部币种账户。
	// currency 非空时按币种精确过滤。
	ListAccountsByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType, currency string) ([]*model.Account, error)

	// GetBalanceSnapshot 查询余额快照
	GetBalanceSnapshot(ctx context.Context, accountNo string, date string) (*model.AccountBalanceSnapshot, error)

	// ReloadHotAllowlist 从 account_meta 重新拉取热点账户白名单并更新本地缓存。
	// 返回重新加载后的账户数量。admin web 更新完热点账户表后调用此接口。
	ReloadHotAllowlist(ctx context.Context) (int, error)

	// ReloadBufferAccountConfig 从 account_meta 重新拉取缓冲记账账户配置并重建刷新调度。
	// 返回重新加载后的账户数量。admin web 更新后调用此接口。
	ReloadBufferAccountConfig(ctx context.Context) (int, error)

	// RebuildHotAccounts 从 MySQL 重建 Redis 上的热点账户余额（disaster recovery）。
	// 详见 redis_rebuild.go。
	RebuildHotAccounts(ctx context.Context, opts RebuildOptions) (*RebuildReport, error)

	// FlushIntervalForAccount 返回账户的缓冲刷新间隔。
	// 如果账户在 buffer_account_config 中，返回对应的 flush_interval_level（分钟）；
	// 否则返回默认值（BufferFlushInterval 秒），适用于平台/中间账户。
	FlushIntervalForAccount(accountNo string) time.Duration
}

// ErrRequestInProgress 相同 request_id 的请求正在处理中，调用方应稍后重试
var ErrRequestInProgress = errors.New("a request with the same x-request-id is already being processed, please retry later")

type accountingService struct {
	accountRepo     repository.AccountRepository
	transactionRepo repository.TransactionRepository
	tccRepo         repository.TccRepository
	tccCoordRepo    repository.TccCoordinatorRepository   // TCC 全局协调者（Risk A 修复）
	orderRepo       repository.TransactionOrderRepository
	outboxRepo      repository.SettlementOutboxRepository // 热路径 WAL，非 nil 时热路径才安全
	batchOrderRepo  repository.BatchOrderRepository
	bufferRepo      repository.BalanceBufferRepository
	hotAccountRepo  repository.HotAccountRepository       // 热点账户配置（meta DB）
	bufferAcctRepo  repository.BufferAccountRepository    // 缓冲记账账户配置（meta DB）
	businessTypeRepo repository.AccountBusinessTypeRepository // channel / business_type 注册表（meta DB）
	ruleRepo         repository.TransactionRuleRepository    // account_type_info / transaction_rule（meta DB）
	dbManager       *database.Manager
	router          *sharding.Router
	idGen           idgen.IDGenerator // 号段模式 ID 生成器：voucher_no / transaction_id
	cutDate         CutDateProvider   // 日切归属日；nil 时回退到 Local 午夜（开发兜底）
	logger          *zap.Logger

	// 热路径组件（可选）：Redis 余额缓存 + Kafka 结算投递
	balanceCache     *cache.BalanceCache // nil = 降级到 TCC 模式
	settlementWriter *kafkamq.Producer   // 可选通知用途（非资金安全关键路径）
	outboxNotifier   *kafkamq.Producer   // 可选：outbox REDIS_DONE 后向 OutboxWorker 推送即时通知（PERF-5）

	// 热路径账户白名单：只有在此 map 中的账户才走 Redis 路径
	// 空 map = 热路径对所有账户关闭（即使 balanceCache != nil）
	// allowlistMu 保护并发 ReloadHotAllowlist 与 allInAllowlist 的读写安全。
	allowlistMu      sync.RWMutex
	accountAllowlist map[string]struct{}

	// 缓冲记账账户集合：按刷新间隔等级（分钟数）分组存储账户号。
	// bufferAcctMu 保护并发重载与查询。
	bufferAcctMu      sync.RWMutex
	bufferAcctByLevel map[model.BufferFlushLevel]map[string]struct{} // level → set of accountNo

	// platformAccountsCache 短 TTL 缓存：business_type(int) → *platformAccountsCacheEntry。
	// 见 ListPlatformAccountsByBusinessType 注释；TTL=2s，admin-web 多 tab 刷新友好。
	platformAccountsCache sync.Map

	// ─── 账户类型 / 业务类型本地 registry ─────────────────────────────────────
	// 两张 meta 表都是配置，极少写、海量读（CreateAccount 每笔都查 business_type）。
	// 启动时一次性 load 进内存；CreateAccount / CreatePlatformAccountFleet / Fleet 校验
	// 全部走这个 cache，0 次 DB 往返。新增 business_type 时 RegisterBusinessType
	// 更新本地 cache，再由 admin-web backend 向所有存活实例广播 /admin/reload/business-types
	// 触发 reload（约定 200ms 内所有实例完成同步）。
	registryMu          sync.RWMutex
	cachedBusinessTypes map[model.AccountBusinessType]*model.AccountBusinessTypeInfo // business_type → info
	cachedAccountTypes  map[model.AccountType]*model.AccountTypeInfo                 // owner_type (数字) → info

	// shutdownCtx 是 app 生命周期 context，通过 EnableHotPath 注入。
	// 热路径后台 goroutine（如 MarkRedisDone）使用此 ctx，确保进程关闭时不泄露。
	shutdownCtx context.Context

	// markRedisDoneSem 限制并发 MarkRedisDone goroutine 数量，防止高并发时 goroutine 爆炸。
	// 容量 = maxMarkRedisDoneConcurrency，满时新请求阻塞等待（不丢弃）。
	markRedisDoneSem chan struct{}

	// warmGroup 利用 singleflight 合并同一账户的并发预热请求，防止 Redis 重启后的雷鸣效应。
	warmGroup singleflight.Group
}

// HotPathEnabler 热路径组件注入接口
// main.go 通过 fx.Invoke 获取此接口，根据 hot_path.enabled 配置决定是否激活热路径。
// 关闭时系统退回并行 TCC 模式（~3万 TPS），无需 Redis / Kafka 依赖。
type HotPathEnabler interface {
	// ctx 为 app 生命周期 context（由 fx.Lifecycle OnStop 取消），
	// 注入后用于热路径内部的后台 goroutine，确保进程关闭时不泄露。
	EnableHotPath(ctx context.Context, bc *cache.BalanceCache, producer *kafkamq.Producer, allowlist []string)
}

// NewAccountingService 创建记账服务
func NewAccountingService(
	accountRepo repository.AccountRepository,
	transactionRepo repository.TransactionRepository,
	tccRepo repository.TccRepository,
	tccCoordRepo repository.TccCoordinatorRepository,
	orderRepo repository.TransactionOrderRepository,
	outboxRepo repository.SettlementOutboxRepository,
	batchOrderRepo repository.BatchOrderRepository,
	bufferRepo repository.BalanceBufferRepository,
	hotAccountRepo repository.HotAccountRepository,
	bufferAcctRepo repository.BufferAccountRepository,
	businessTypeRepo repository.AccountBusinessTypeRepository,
	ruleRepo repository.TransactionRuleRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	idGen idgen.IDGenerator,
	cutDate CutDateProvider,
	logger *zap.Logger,
) AccountingService {
	return &accountingService{
		accountRepo:         accountRepo,
		transactionRepo:     transactionRepo,
		tccRepo:             tccRepo,
		tccCoordRepo:        tccCoordRepo,
		orderRepo:           orderRepo,
		outboxRepo:          outboxRepo,
		batchOrderRepo:      batchOrderRepo,
		bufferRepo:          bufferRepo,
		hotAccountRepo:      hotAccountRepo,
		bufferAcctRepo:      bufferAcctRepo,
		businessTypeRepo:    businessTypeRepo,
		ruleRepo:            ruleRepo,
		dbManager:           dbManager,
		router:              router,
		idGen:               idGen,
		cutDate:             cutDate,
		logger:              logger,
		markRedisDoneSem:    make(chan struct{}, maxMarkRedisDoneConcurrency),
		bufferAcctByLevel:   make(map[model.BufferFlushLevel]map[string]struct{}),
		cachedBusinessTypes: map[model.AccountBusinessType]*model.AccountBusinessTypeInfo{},
		cachedAccountTypes:  map[model.AccountType]*model.AccountTypeInfo{},
	}
}

// resolveCutDate 给 booking 路径用的 helper：CutDateProvider 注入时调用之；
// 否则回退到 transaction_date（YYYY-MM-DD Local），保证 NOT NULL 列必填。
func (s *accountingService) resolveCutDate(now time.Time) string {
	if s.cutDate != nil {
		return s.cutDate.CutDateForTime(now)
	}
	return now.Format("2006-01-02")
}

// NewHotPathEnabler 将 AccountingService 暴露为 HotPathEnabler 供 main.go 的 fx.Invoke 使用。
// 在同一包内做类型断言，无需导出具体类型。
func NewHotPathEnabler(svc AccountingService) HotPathEnabler {
	return svc.(*accountingService)
}

// EnableHotPath 注入 Redis + Kafka 组件，启用热路径（15-20万 TPS，资金 100% 安全）。
// ctx 为 app 生命周期 context，注入后用于热路径内部后台 goroutine。
// allowlist 为账户号白名单；一笔记账的所有分录账户都在白名单中才走热路径，
// 否则（包括白名单为空）整笔回落 TCC 冷路径。
// SetOutboxNotifier 注入可选 Kafka producer，用于 hot path outbox REDIS_DONE 后推送通知。
// 不注入则保持纯轮询模式。OutboxWorker 侧需要对应调 StartKafkaConsumer 订阅同一 topic。
func (s *accountingService) SetOutboxNotifier(p *kafkamq.Producer) { s.outboxNotifier = p }

func (s *accountingService) EnableHotPath(ctx context.Context, bc *cache.BalanceCache, producer *kafkamq.Producer, allowlist []string) {
	s.shutdownCtx = ctx
	s.balanceCache = bc
	s.settlementWriter = producer
	newMap := make(map[string]struct{}, len(allowlist))
	for _, acc := range allowlist {
		newMap[acc] = struct{}{}
	}
	s.allowlistMu.Lock()
	s.accountAllowlist = newMap
	s.allowlistMu.Unlock()
	s.logger.Info("hot path enabled: Redis + MySQL Outbox",
		zap.Int("allowlist_size", len(allowlist)))
}

// ReloadHotAllowlist 从 account_meta.hot_account_config 重新拉取启用账户并替换本地缓存。
func (s *accountingService) ReloadHotAllowlist(ctx context.Context) (int, error) {
	if s.hotAccountRepo == nil {
		return 0, fmt.Errorf("hot account repo not configured")
	}
	accounts, err := s.hotAccountRepo.LoadEnabledAccounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("load hot accounts from DB: %w", err)
	}
	newMap := make(map[string]struct{}, len(accounts))
	for _, acc := range accounts {
		newMap[acc] = struct{}{}
	}
	s.allowlistMu.Lock()
	s.accountAllowlist = newMap
	s.allowlistMu.Unlock()
	s.logger.Info("hot allowlist reloaded from DB", zap.Int("count", len(accounts)))
	return len(accounts), nil
}

// ReloadBufferAccountConfig 从 account_meta.buffer_account_config 重新拉取配置并重建本地缓存。
// 返回重新加载后启用的账户总数。
func (s *accountingService) ReloadBufferAccountConfig(ctx context.Context) (int, error) {
	if s.bufferAcctRepo == nil {
		return 0, fmt.Errorf("buffer account repo not configured")
	}
	configs, err := s.bufferAcctRepo.LoadAllEnabled(ctx)
	if err != nil {
		return 0, fmt.Errorf("load buffer account configs from DB: %w", err)
	}
	newByLevel := make(map[model.BufferFlushLevel]map[string]struct{})
	for _, cfg := range configs {
		if _, ok := newByLevel[cfg.FlushIntervalLevel]; !ok {
			newByLevel[cfg.FlushIntervalLevel] = make(map[string]struct{})
		}
		newByLevel[cfg.FlushIntervalLevel][cfg.AccountNo] = struct{}{}
	}
	s.bufferAcctMu.Lock()
	s.bufferAcctByLevel = newByLevel
	s.bufferAcctMu.Unlock()
	s.logger.Info("buffer account config reloaded from DB", zap.Int("count", len(configs)))
	return len(configs), nil
}

// isBufferedAccount 检查账户是否在缓冲记账配置列表中（任意等级）。
func (s *accountingService) isBufferedAccount(accountNo string) bool {
	s.bufferAcctMu.RLock()
	defer s.bufferAcctMu.RUnlock()
	for _, set := range s.bufferAcctByLevel {
		if _, ok := set[accountNo]; ok {
			return true
		}
	}
	return false
}

// FlushIntervalForAccount 返回账户的缓冲刷新间隔。
// buffer_account_config 中配置的账户返回对应的 flush_interval_level（分钟）；
// 否则返回默认值（BufferFlushInterval 秒），适用于平台/中间账户。
func (s *accountingService) FlushIntervalForAccount(accountNo string) time.Duration {
	s.bufferAcctMu.RLock()
	defer s.bufferAcctMu.RUnlock()
	for level, set := range s.bufferAcctByLevel {
		if _, ok := set[accountNo]; ok {
			return time.Duration(level) * time.Minute
		}
	}
	return time.Duration(model.BufferFlushInterval) * time.Second
}

// bufferFlushJitter 根据账户号计算确定性抖动，将不同账户的初始 flush_scheduled_at 错开，
// 防止大量账户在同一时刻同时触发刷新（雷鸣效应）。
// 抖动范围：[0, interval/4]，基于账户号 FNV-32 哈希，同一账户每次抖动固定，不同账户均匀分布。
func bufferFlushJitter(accountNo string, interval time.Duration) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(accountNo))
	maxJitter := interval / 4
	if maxJitter < time.Second {
		maxJitter = time.Second
	}
	return time.Duration(h.Sum32()%uint32(maxJitter.Seconds())) * time.Second
}

// allInAllowlist 检查记账请求的所有账户是否都在热路径白名单中。
// 白名单为空或任一账户不在其中，返回 false → 整笔走 TCC。
func (s *accountingService) allInAllowlist(entries []AccountingEntry) bool {
	s.allowlistMu.RLock()
	al := s.accountAllowlist
	s.allowlistMu.RUnlock()
	if len(al) == 0 {
		return false
	}
	for _, e := range entries {
		if _, ok := al[e.AccountNo]; !ok {
			return false
		}
	}
	return true
}

// ─── TCC 分布式复式记账 ────────────────────────────────────────────────────────
//
// TCC（Try-Confirm-Cancel）保证跨库记账的原子性：
//
//   Try     阶段：并行对每个分录加锁账户、检查余额、冻结资金，写 TCC 分支记录（TRYING）。
//             各分录打到不同分片时完全并行，打到同一分片时由 MySQL 行锁自动串行。
//             任意分支 Try 失败 → 并行 Cancel 所有已成功的 Try 分支，整体返回错误。
//
//   Confirm 阶段：所有 Try 成功后，并行对每个分录真正修改 balance、写流水、置 CONFIRMED。
//             Confirm 具备幂等性：若分支已是 CONFIRMED，直接跳过。
//             Confirm 失败（极少，网络抖动/节点宕机）：保留 TRYING 状态供后台恢复任务重试。
//
//   Cancel  阶段：仅在 Try 阶段失败时触发，并行释放冻结资金，置 CANCELLED。
//             空回滚保护：分支记录不存在时直接视为成功。
//
// 性能对比（4分录支付，不同分片）：
//   串行 TCC：Try1→Try2→Try3→Try4→Confirm1→Confirm2→Confirm3→Confirm4  ≈ 8×RTT
//   并行 TCC：[Try1‖Try2‖Try3‖Try4] → [Confirm1‖Confirm2‖Confirm3‖Confirm4]  ≈ 2×RTT

// DoubleEntryBooking 复式记账入口（幂等）
//
// 幂等性：
//
//	通过 req.RequestID（来自调用方 gRPC metadata x-request-id）查询 TransactionOrder 表：
//	- 已成功：直接返回历史结果，不重复执行
//	- 处理中：返回 ErrRequestInProgress，调用方稍后重试
//	- 失败/待处理：重新执行（最多重试 3 次抢锁）
//
// 执行路由：
//
//	热路径（Redis + Kafka）: 延迟 < 2ms，支持 30万+ TPS
//	冷路径（TCC + MySQL） : 延迟 ~8ms，支持 ~3万 TPS（热路径缓存 miss 时自动降级）
func (s *accountingService) DoubleEntryBooking(ctx context.Context, req *DoubleEntryBookingRequest) (string, []string, error) {
	if err := validateCurrency(req.Currency); err != nil {
		return "", nil, err
	}
	if err := s.validateEntries(req.Entries); err != nil {
		return "", nil, fmt.Errorf("validate entries: %w", err)
	}
	metrics.InflightBookingsGauge.Inc()
	defer metrics.InflightBookingsGauge.Dec()
	return s.idempotentBooking(ctx, req)
}

// HybridDoubleEntryBooking 混合路由记账（热路径优先，TCC 兜底），语义同 DoubleEntryBooking。
// 额外返回 idempotentHit 标志，供调用方区分"本次执行成功"与"幂等命中历史结果"。
func (s *accountingService) HybridDoubleEntryBooking(ctx context.Context, req *DoubleEntryBookingRequest) (string, []string, bool, error) {
	if err := validateCurrency(req.Currency); err != nil {
		return "", nil, false, err
	}
	if err := s.validateEntries(req.Entries); err != nil {
		return "", nil, false, fmt.Errorf("validate entries: %w", err)
	}
	// 先检查是否已成功，标记幂等命中
	bno, btype := req.BusinessNo, string(req.BusinessType)
	existing, err := s.orderRepo.GetByOrderKey(ctx, req.RequestID, btype, bno)
	if err != nil {
		return "", nil, false, fmt.Errorf("hybrid: check idempotency: %w", err)
	}
	if existing != nil && existing.Status == model.TransactionOrderStatusSuccess {
		extra := s.parseOrderExtra(existing.Extra)
		return existing.VoucherNo, extra.TxIDs, true, nil
	}

	voucherNo, txIDs, err := s.idempotentBooking(ctx, req)
	return voucherNo, txIDs, false, err
}

// AtomicBatchBooking 原子批量记账（全部成功 or 全部回滚）
//
// 执行流程：
//  1. 幂等检查（batch_order 表）：已成功 → 直接返回历史结果
//  2. 持久化批次请求参数到 batch_order（status=PENDING）
//  3. 预生成所有单笔 voucherNo + txIDs（确保幂等）
//  4. 并行执行所有单笔 TCC Try 阶段
//  5. 全部 Try 成功 → 并行 Confirm；任意 Try 失败 → 并行 Cancel 所有已 Try 分支
//  6. 更新 batch_order 状态
func (s *accountingService) AtomicBatchBooking(ctx context.Context, req *AtomicBatchBookingRequest) (*AtomicBatchBookingResult, error) {
	if len(req.Requests) == 0 {
		return nil, fmt.Errorf("atomic batch: no requests provided")
	}
	for i := range req.Requests {
		if err := validateCurrency(req.Requests[i].Currency); err != nil {
			return nil, fmt.Errorf("atomic batch: request[%d] invalid: %w", i, err)
		}
		if err := s.validateEntries(req.Requests[i].Entries); err != nil {
			return nil, fmt.Errorf("atomic batch: request[%d] invalid: %w", i, err)
		}
	}

	// ── Step 1: 预生成所有单笔 IDs（在持久化之前生成，保证重试幂等）────────────
	storedPregens := make([]batchStoredPregen, len(req.Requests))
	for i, r := range req.Requests {
		vno, err := s.generateVoucherNo(ctx, r.BusinessNo)
		if err != nil {
			return nil, fmt.Errorf("atomic batch: generate voucher no for request[%d]: %w", i, err)
		}
		storedPregens[i].VoucherNo = vno
		storedPregens[i].TxIDs = make([]string, len(r.Entries))
		for j, e := range r.Entries {
			txID, err := s.generateTransactionID(ctx, e.AccountNo)
			if err != nil {
				return nil, fmt.Errorf("atomic batch: generate transaction id for request[%d] entry[%d]: %w", i, j, err)
			}
			storedPregens[i].TxIDs[j] = txID
		}
	}

	// ── Step 2: 持久化批次请求到 batch_order（幂等：重复时读回已有记录）────────
	// **关键**：cut_date 在 batch_order 第一次创建时定死，写入 Extra。后续重试
	// 进入此函数时，CreateIfNotExists 读回已存在的行，cut_date 从 Extra 读，
	// 永不重算 → 抗 SystemConfig.scheduled_time 变更、CutDateProvider 热重载、
	// 跨 cut_hour 边界、DB 重启 + 多 pod 时钟漂移。
	now := time.Now()
	transactionDate := now.Format("2006-01-02")
	cutDate := s.resolveCutDate(now)
	batchExtra := s.buildBatchExtra(req, storedPregens, cutDate)
	if err := checkBatchExtraSize(batchExtra); err != nil {
		return nil, fmt.Errorf("atomic batch: %w", err)
	}
	batchOrder := &model.BatchOrder{
		BatchID:     req.BatchRequestID,
		BusinessNo:  req.BatchBusinessNo,
		ItemCount:   len(req.Requests),
		Status:      model.TransactionOrderStatusPending,
		Description: req.Description,
		Extra:       batchExtra,
	}
	// CreateIfNotExists：若 batch_id 已存在则读回已有记录（含已存储的 storedPregens + cut_date）
	if err := s.batchOrderRepo.CreateIfNotExists(ctx, batchOrder); err != nil {
		return nil, fmt.Errorf("atomic batch: save batch order: %w", err)
	}

	// 检查批次是否已执行成功（幂等返回，携带原始 VoucherNo / TxIDs）
	if batchOrder.Status == model.TransactionOrderStatusSuccess {
		return s.parseBatchResult(batchOrder), nil
	}

	// 若读回的是已有记录，使用其中存储的预生成 IDs + cut_date（而非本次新算的）
	if batchOrder.Extra != batchExtra {
		var existingExtra batchOrderExtra
		if err := json.Unmarshal([]byte(batchOrder.Extra), &existingExtra); err == nil {
			if len(existingExtra.Pregens) == len(req.Requests) {
				storedPregens = existingExtra.Pregens
			}
			// **资金安全核心**：retry 时一定要用持久化的 cut_date，不能用本地新算的值
			if existingExtra.CutDate != "" {
				if existingExtra.CutDate != cutDate {
					s.logger.Warn("atomic batch: existing batch has different cut_date than local (retry/cfg reload), using persisted",
						zap.String("batchID", req.BatchRequestID),
						zap.String("local", cutDate),
						zap.String("persisted", existingExtra.CutDate))
				}
				cutDate = existingExtra.CutDate
			}
		}
	}

	// ── 协调者：为每个子请求创建 TRYING 阶段标记（Risk A 修复）──────────────────
	// 写入失败为非致命错误：协调者不存在时 RecoveryWorker 退化为扫描分支记录。
	//
	// 此时 cutDate 已是权威值（首次=本地算的，重试=batch_order.Extra 持久化的），
	// 直接 propagate 到 coord 和所有子请求 Phase 2，永不再变。
	batchCutDates := make([]string, len(storedPregens))
	for i := range batchCutDates {
		batchCutDates[i] = cutDate
	}
	if s.tccCoordRepo != nil {
		for i, pregen := range storedPregens {
			if coordErr := s.tccCoordRepo.Create(ctx, pregen.VoucherNo, req.Requests[i].BusinessNo, cutDate, req.Requests[i].Currency, len(req.Requests[i].Entries)); coordErr != nil {
				s.logger.Warn("atomic batch: tcc coordinator create failed (non-fatal)",
					zap.String("batchID", req.BatchRequestID),
					zap.String("voucherNo", pregen.VoucherNo),
					zap.Error(coordErr))
			}
			// 双保险：从 coord 回读 cut_date —— 如果 batch 之间分次推进，coord
			// 可能已带有上次的 cut_date，以 coord 持久化值优先。
			if existing, gErr := s.tccCoordRepo.GetByTccID(ctx, pregen.VoucherNo); gErr == nil && existing != nil && existing.CutDate != "" {
				if existing.CutDate != cutDate {
					s.logger.Warn("atomic batch: coord has different cut_date than batch, using coord value",
						zap.String("voucherNo", pregen.VoucherNo),
						zap.String("batch", cutDate),
						zap.String("coord", existing.CutDate))
				}
				batchCutDates[i] = existing.CutDate
			}
		}
	}

	// ── Step 3: 全库并行 Try（每 DB 一个事务，DB-level 事务合并）───────────────
	//
	// tried[i][j]      = request[i].entry[j] 的 tccBranchMeta（Try 成功后填充）
	// triedDelta[i][j] = request[i].entry[j] 的 balanceDelta（供 Confirm 跳过 GetBranchForUpdate）
	// tryErrs[i][j]    = request[i].entry[j] 的 Try 错误（nil = 成功）
	//
	// 每个 goroutine 独占一组 {reqIdx, entIdx} 对，无竞争，故可无锁直接写三个切片。

	tried := make([][]tccBranchMeta, len(req.Requests))
	triedDelta := make([][]int64, len(req.Requests))
	tryErrs := make([][]error, len(req.Requests))
	for i, r := range req.Requests {
		tried[i] = make([]tccBranchMeta, len(r.Entries))
		triedDelta[i] = make([]int64, len(r.Entries))
		tryErrs[i] = make([]error, len(r.Entries))
	}

	// batchWork 携带路由信息（分组时计算一次，goroutine 内直接复用）。
	type batchWork struct {
		reqIdx   int
		entIdx   int
		entry    AccountingEntry
		dbIdx    int
		tableIdx int
		vno      string
		branchID string
	}

	// 按物理 DB 分组（key = dbIndex，同一 DB 不同 table 合并为一个事务）
	batchDBGroupMap := make(map[int][]batchWork)
	batchDBGroupKeys := make([]int, 0)
	for i, r := range req.Requests {
		for j, entry := range r.Entries {
			dbIdx, tableIdx := s.router.RouteByAccountNo(entry.AccountNo)
			if _, ok := batchDBGroupMap[dbIdx]; !ok {
				batchDBGroupKeys = append(batchDBGroupKeys, dbIdx)
			}
			batchDBGroupMap[dbIdx] = append(batchDBGroupMap[dbIdx], batchWork{i, j, entry, dbIdx, tableIdx, storedPregens[i].VoucherNo, storedPregens[i].TxIDs[j]})
		}
	}
	// 每个 DB 组内按 account_no 升序排列（全局一致的行锁顺序，防死锁）
	for dbIdx := range batchDBGroupMap {
		sort.Slice(batchDBGroupMap[dbIdx], func(a, b int) bool {
			return batchDBGroupMap[dbIdx][a].entry.AccountNo < batchDBGroupMap[dbIdx][b].entry.AccountNo
		})
	}

	var tryWg sync.WaitGroup
	for _, dbIdx := range batchDBGroupKeys {
		dbIdx, group := dbIdx, batchDBGroupMap[dbIdx]
		tryWg.Add(1)
		go func() {
			defer tryWg.Done()
			db, err := s.dbManager.GetDB(dbIdx)
			if err != nil {
				for _, w := range group {
					tryErrs[w.reqIdx][w.entIdx] = fmt.Errorf("get db[%d]: %w", dbIdx, err)
				}
				return
			}
			tx := db.WithContext(ctx).Begin()
			if tx.Error != nil {
				for _, w := range group {
					tryErrs[w.reqIdx][w.entIdx] = fmt.Errorf("begin tx: %w", tx.Error)
				}
				return
			}

			type groupDelta struct{ delta int64 }
			deltas := make([]groupDelta, len(group))
			var groupErr error
			for gi, w := range group {
				account, accErr := s.accountRepo.GetAccountForUpdate(ctx, tx, w.entry.AccountNo, w.dbIdx, w.tableIdx)
				if accErr != nil || account == nil {
					msg := "account not found"
					if accErr != nil {
						msg = accErr.Error()
					}
					groupErr = fmt.Errorf("get account %s: %s", w.entry.AccountNo, msg)
					break
				}
				delta := computeBalanceDelta(w.entry, isAssetOrExpense(account))
				if tryErr := s.tccTry(ctx, tx, w.vno, w.branchID, w.entry, account, w.dbIdx, w.tableIdx); tryErr != nil {
					groupErr = fmt.Errorf("tcc try account=%s: %w", w.entry.AccountNo, tryErr)
					break
				}
				deltas[gi].delta = delta
			}

			if groupErr != nil {
				tx.Rollback()
				for _, w := range group {
					tryErrs[w.reqIdx][w.entIdx] = groupErr
				}
				return
			}
			if commitErr := tx.Commit().Error; commitErr != nil {
				for _, w := range group {
					tryErrs[w.reqIdx][w.entIdx] = fmt.Errorf("commit try db[%d]: %w", dbIdx, commitErr)
				}
				return
			}
			// Commit 成功：记录 meta + balanceDelta（Confirm 阶段复用，跳过 GetBranchForUpdate）
			for gi, w := range group {
				tried[w.reqIdx][w.entIdx] = tccBranchMeta{w.dbIdx, w.tableIdx, w.branchID, w.entry}
				triedDelta[w.reqIdx][w.entIdx] = deltas[gi].delta
			}
		}()
	}
	tryWg.Wait()

	// 汇总 Try 结果
	successMetas := make([]tccBranchMeta, 0)
	var allTryErr error
	for i := range tryErrs {
		for j := range tryErrs[i] {
			if tryErrs[i][j] != nil {
				if allTryErr == nil {
					allTryErr = tryErrs[i][j]
				}
			} else {
				successMetas = append(successMetas, tried[i][j])
			}
		}
	}

	if allTryErr != nil {
		// Try 失败 → 使用脱离请求 ctx 的独立上下文执行清理（防止调用方超时中断取消操作）
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cleanupCancel()
		// 协调者：Try 失败 → 标记 CANCELLED（非致命）
		if s.tccCoordRepo != nil {
			for _, pregen := range storedPregens {
				if cErr := s.tccCoordRepo.TransitionToCancelled(cleanupCtx, pregen.VoucherNo); cErr != nil {
					s.logger.Warn("atomic batch: coordinator cancel transition failed",
						zap.String("batchID", req.BatchRequestID),
						zap.String("voucherNo", pregen.VoucherNo),
						zap.Error(cErr))
				}
			}
		}
		s.cancelTried(cleanupCtx, successMetas)
		s.updateBatchOrderStatus(ctx, req.BatchRequestID, req.BatchBusinessNo, model.TransactionOrderStatusFailed, allTryErr.Error())
		return &AtomicBatchBookingResult{
			BatchID:    req.BatchRequestID,
			AllSuccess: false,
			Items:      s.buildFailedItems(req, allTryErr),
		}, fmt.Errorf("atomic batch try failed (all cancelled): %w", allTryErr)
	}

	// ── Step 4: 所有 Try 成功 → 并行 Confirm（每 DB 一个事务，与 Try 相同锁顺序）──
	// Confirm 失败极罕见（网络抖动/节点宕机）；失败时将分支 TRYING 状态保留以供后台恢复。
	// 即使有分支 Confirm 失败，也必须收集错误并将批次标记为 FAILED，不能误报 SUCCESS。
	items := make([]BatchBookingItemResult, len(req.Requests))
	for i, r := range req.Requests {
		items[i] = BatchBookingItemResult{
			RequestID: r.RequestID,
			VoucherNo: storedPregens[i].VoucherNo,
			TxIDs:     storedPregens[i].TxIDs,
		}
	}

	// 按物理 DB 重新分组（使用 tried 中的路由元数据，与 Try 阶段分组一致）
	type batchCfmWork struct {
		reqIdx       int
		entIdx       int
		entry        AccountingEntry
		meta         tccBranchMeta
		vno          string
		balanceDelta int64
	}
	cfmDBGroupMap := make(map[int][]batchCfmWork)
	cfmDBGroupKeys := make([]int, 0)
	for i, r := range req.Requests {
		for j := range r.Entries {
			meta := tried[i][j]
			if _, ok := cfmDBGroupMap[meta.dbIndex]; !ok {
				cfmDBGroupKeys = append(cfmDBGroupKeys, meta.dbIndex)
			}
			cfmDBGroupMap[meta.dbIndex] = append(cfmDBGroupMap[meta.dbIndex], batchCfmWork{
				i, j, r.Entries[j], meta, storedPregens[i].VoucherNo, triedDelta[i][j],
			})
		}
	}
	// 与 Try 阶段保持相同的行锁顺序（按 account_no 升序），防止跨 goroutine 死锁
	for dbIdx := range cfmDBGroupMap {
		sort.Slice(cfmDBGroupMap[dbIdx], func(a, b int) bool {
			return cfmDBGroupMap[dbIdx][a].entry.AccountNo < cfmDBGroupMap[dbIdx][b].entry.AccountNo
		})
	}

	// ── 独立 Confirm 上下文（脱离请求 ctx，防止调用方超时中断 Confirm 导致 CONFIRMING 悬挂）──
	// 若 gRPC 请求在 Try 成功后、Confirm 完成前超时，使用请求 ctx 会导致 Confirm goroutine
	// 全部失败，coordinator 永久停留 CONFIRMING，须人工介入。
	// 使用 30s 独立上下文足以完成正常 Confirm（单 DB 事务 < 100ms）。
	confirmCtx, confirmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer confirmCancel()

	// ── 协调者：Try 全部成功 → 原子 CAS 进入 CONFIRMING 阶段 ────────────────────
	// CAS = UPDATE … WHERE phase = TRYING → CONFIRMING；rows_affected=0 说明
	// recovery worker 已经把 coordinator 改成 CANCELLED 并 cancel 掉了 branch
	// (释放冻结)。此时再继续 Confirm 等于"释放冻结后又应用余额 = 凭空多钱"，
	// 资金会不平。**必须 fail-fast 不再继续 Confirm，让调用方拿到 error 重试。**
	//
	// 注：以前这里写了"非致命继续"，是 trial-balance 不平 bug 的根因之一；
	// 已统一改成 fail-fast，详见 tcc_service.CancelTcc 的对称 CAS。
	if s.tccCoordRepo != nil {
		for _, pregen := range storedPregens {
			if cErr := s.tccCoordRepo.TransitionToConfirming(confirmCtx, pregen.VoucherNo); cErr != nil {
				s.logger.Error("atomic batch: coordinator confirming CAS failed — aborting confirm to protect fund integrity",
					zap.String("batchID", req.BatchRequestID),
					zap.String("voucherNo", pregen.VoucherNo),
					zap.Error(cErr))
				// 致命：返回 error 让上层 caller 知道整笔 batch 被 recovery 抢先取消，
				// 不要继续 Confirm（避免在已 cancel 的分支上叠加余额变更）。
				return nil, fmt.Errorf("tcc confirm aborted: coordinator %s no longer in TRYING (recovered/cancelled): %w", pregen.VoucherNo, cErr)
			}
		}
	}

	var confirmWg sync.WaitGroup
	var confirmErrMu sync.Mutex
	var firstConfirmErr error
	recordConfirmErr := func(err error) {
		confirmErrMu.Lock()
		if firstConfirmErr == nil {
			firstConfirmErr = err
		}
		confirmErrMu.Unlock()
	}
	for _, dbIdx := range cfmDBGroupKeys {
		dbIdx, group := dbIdx, cfmDBGroupMap[dbIdx]
		confirmWg.Add(1)
		go func() {
			defer confirmWg.Done()
			db, err := s.dbManager.GetDB(dbIdx)
			if err != nil {
				recordConfirmErr(fmt.Errorf("confirm get db[%d]: %w", dbIdx, err))
				s.logger.Error("atomic batch: confirm get db failed, needs recovery",
					zap.Error(err), zap.String("batchID", req.BatchRequestID))
				return
			}
			tx := db.WithContext(confirmCtx).Begin()
			if tx.Error != nil {
				recordConfirmErr(fmt.Errorf("confirm begin tx: %w", tx.Error))
				s.logger.Error("atomic batch: confirm begin tx failed, needs recovery",
					zap.Error(tx.Error), zap.String("batchID", req.BatchRequestID))
				return
			}

			var groupErr error
			for _, w := range group {
				account, accErr := s.accountRepo.GetAccountForUpdate(confirmCtx, tx, w.entry.AccountNo, w.meta.dbIndex, w.meta.tableIndex)
				if accErr != nil || account == nil {
					msg := "account not found"
					if accErr != nil {
						msg = accErr.Error()
					}
					groupErr = fmt.Errorf("get account %s: %s", w.entry.AccountNo, msg)
					s.logger.Error("atomic batch: confirm get account failed, needs recovery",
						zap.String("batchID", req.BatchRequestID), zap.String("accountNo", w.entry.AccountNo))
					break
				}
				cfmErr := s.tccConfirm(confirmCtx, tx, w.meta.branchID, w.entry, account, &bookingParams{
					transactionID:   w.meta.branchID,
					voucherNo:       w.vno,
					businessNo:      req.Requests[w.reqIdx].BusinessNo,
					businessType:    req.Requests[w.reqIdx].BusinessType,
					entry:           w.entry,
					currency:        req.Requests[w.reqIdx].Currency,
					transactionDate: transactionDate,
					transactionTime: now,
					description:     req.Requests[w.reqIdx].Description,
					// 用回读 coord 的 cut_date（batchCutDates[reqIdx]），不用刚算的 cutDate
					// 局部变量。重试场景下 coord 持久化值才是 ground truth，避免 cut_date
					// 飘到次日。
					cutDate:         batchCutDates[w.reqIdx],
				}, w.meta.dbIndex, w.meta.tableIndex, w.balanceDelta, true)
				if cfmErr != nil {
					groupErr = cfmErr
					s.logger.Error("atomic batch: confirm failed, needs recovery",
						zap.Error(cfmErr),
						zap.String("batchID", req.BatchRequestID), zap.String("branchID", w.meta.branchID))
					break
				}
			}

			if groupErr != nil {
				tx.Rollback()
				recordConfirmErr(groupErr)
				return
			}
			if commitErr := tx.Commit().Error; commitErr != nil {
				s.logger.Error("atomic batch: confirm commit failed, needs recovery",
					zap.Error(commitErr), zap.String("batchID", req.BatchRequestID))
				recordConfirmErr(commitErr)
			}
		}()
	}
	confirmWg.Wait()

	if firstConfirmErr != nil {
		// 部分 Confirm 失败：分支处于 TRYING 状态，后台恢复任务会重试；
		// 批次标记 FAILED，避免误报 SUCCESS 导致重复记账。
		s.updateBatchOrderStatus(ctx, req.BatchRequestID, req.BatchBusinessNo, model.TransactionOrderStatusFailed, firstConfirmErr.Error())
		return &AtomicBatchBookingResult{
			BatchID:    req.BatchRequestID,
			AllSuccess: false,
			Items:      items,
		}, fmt.Errorf("atomic batch confirm partial failure (needs recovery): %w", firstConfirmErr)
	}

	// 协调者：所有子请求 Confirm 成功 → 标记 CONFIRMED。
	// cut_date 已在 Create 时写入 + propagate 到所有 entry，无需 fan-out。
	if s.tccCoordRepo != nil {
		for _, pregen := range storedPregens {
			if cErr := s.tccCoordRepo.TransitionToConfirmed(confirmCtx, pregen.VoucherNo); cErr != nil {
				s.logger.Warn("atomic batch: coordinator confirmed transition failed (non-fatal)",
					zap.String("batchID", req.BatchRequestID),
					zap.String("voucherNo", pregen.VoucherNo),
					zap.Error(cErr))
			}
		}
	}

	s.updateBatchOrderStatus(ctx, req.BatchRequestID, req.BatchBusinessNo, model.TransactionOrderStatusSuccess, "")
	return &AtomicBatchBookingResult{
		BatchID:    req.BatchRequestID,
		AllSuccess: true,
		Items:      items,
	}, nil
}

// updateBatchOrderStatus 更新 batch_order 状态（按 businessNo 路由）
func (s *accountingService) updateBatchOrderStatus(ctx context.Context, batchID, businessNo string, status int8, errMsg string) {
	if err := s.batchOrderRepo.UpdateStatus(ctx, batchID, businessNo, status, truncateErrMsg(errMsg)); err != nil {
		s.logger.Error("updateBatchOrderStatus failed", zap.Error(err), zap.String("batchID", batchID))
	}
}

// batchStoredPregen 单笔记账的预生成 ID（存入 batch_order.Extra 以确保重试幂等）
type batchStoredPregen struct {
	VoucherNo string   `json:"voucher_no"`
	TxIDs     []string `json:"tx_ids"`
}

// batchOrderExtra batch_order.Extra 的完整 JSON 结构
type batchOrderExtra struct {
	Items   []batchExtraItem    `json:"items"`   // 请求参数快照
	Pregens []batchStoredPregen `json:"pregens"` // 预生成 ID（首次创建时写入，重试时复用）
	// CutDate 日切归属日（YYYY-MM-DD）。**第一次创建批次订单那一刻就定死，
	// 后续所有重试 / Phase 2 Confirm / Recovery 一律读本字段，永不重算**。
	// 抗 SystemConfig.day_cut.scheduled_time 变更、CutDateProvider 热重载、
	// 跨 cut_hour 边界、DB 重启 + 多 pod 时钟漂移 —— 同 batch 全部子请求 +
	// 全部 entry 必然共享同一 cut_date。
	CutDate string `json:"cut_date,omitempty"`
}

type batchExtraItem struct {
	RequestID    string       `json:"request_id"`
	BusinessNo   string       `json:"business_no"`
	BusinessType string       `json:"business_type"`
	Currency     string       `json:"currency"`
	Description  string       `json:"description"`
	Entries      []entryParam `json:"entries"`
}

// buildBatchExtra 将批次请求参数和预生成 ID 序列化为 JSON（用于持久化）
func (s *accountingService) buildBatchExtra(req *AtomicBatchBookingRequest, pregens []batchStoredPregen, cutDate string) string {
	items := make([]batchExtraItem, len(req.Requests))
	for i, r := range req.Requests {
		eps := make([]entryParam, len(r.Entries))
		for j, e := range r.Entries {
			eps[j] = entryParam{AccountNo: e.AccountNo, DebitAmount: strconv.FormatInt(e.DebitAmount, 10), CreditAmount: strconv.FormatInt(e.CreditAmount, 10), Description: e.Description}
		}
		items[i] = batchExtraItem{RequestID: r.RequestID, BusinessNo: r.BusinessNo, BusinessType: string(r.BusinessType), Currency: r.Currency, Description: r.Description, Entries: eps}
	}
	b, _ := json.Marshal(batchOrderExtra{Items: items, Pregens: pregens, CutDate: cutDate})
	return string(b)
}

// checkBatchExtraSize 校验批次 extra 是否超出 DB 列限制。
// 超出时返回错误（fail-fast），禁止截断：截断后 JSON 损坏会导致重试时解析失败、
// 生成不同的预生成 ID，破坏幂等性，造成重复记账。
// 调用方应拒绝本次请求并提示客户端缩小批次或缩短描述字段。
func checkBatchExtraSize(s string) error {
	if len(s) <= model.TransactionOrderExtraMaxLen {
		return nil
	}
	return fmt.Errorf("batch extra JSON is too large (%d bytes, max %d): reduce batch size or shorten description/business_no fields",
		len(s), model.TransactionOrderExtraMaxLen)
}

// parseBatchResult 从已完成的 batch_order 中构造结果（幂等返回）
// 解码 Extra 字段中存储的预生成 ID，以返回与首次请求相同的 VoucherNo 和 TxIDs。
func (s *accountingService) parseBatchResult(order *model.BatchOrder) *AtomicBatchBookingResult {
	var extra batchOrderExtra
	if err := json.Unmarshal([]byte(order.Extra), &extra); err != nil {
		s.logger.Warn("parseBatchResult: failed to decode batch extra, IDs unavailable",
			zap.Error(err), zap.String("batchID", order.BatchID))
	}

	items := make([]BatchBookingItemResult, len(extra.Pregens))
	for i, p := range extra.Pregens {
		requestID := ""
		if i < len(extra.Items) {
			requestID = extra.Items[i].RequestID
		}
		items[i] = BatchBookingItemResult{
			RequestID: requestID,
			VoucherNo: p.VoucherNo,
			TxIDs:     p.TxIDs,
		}
	}
	return &AtomicBatchBookingResult{
		BatchID:    order.BatchID,
		AllSuccess: order.Status == model.TransactionOrderStatusSuccess,
		Items:      items,
	}
}

// buildFailedItems 构造全部失败的批次结果
func (s *accountingService) buildFailedItems(req *AtomicBatchBookingRequest, err error) []BatchBookingItemResult {
	items := make([]BatchBookingItemResult, len(req.Requests))
	for i, r := range req.Requests {
		items[i] = BatchBookingItemResult{RequestID: r.RequestID, Err: err}
	}
	return items
}

// idempotentBooking TransactionOrder 幂等包装层
//
// 状态机：PENDING / PROCESSING → SUCCESS / FAILED
// 用 CAS (UpdateToProcessing) 保证并发时只有一个请求真正执行。
//
// 幂等 ID 预生成策略：
//
//	订单首次创建时，一次性生成 voucherNo 和所有 txIDs，存入 Extra 字段。
//	后续重试从 Extra 读取，确保同一 requestId 始终使用相同的凭证号和流水号，
//	避免因重试产生孤儿流水或凭证号不一致。
//
// 请求参数持久化：
//
//	完整请求参数快照（reqParams）随订单一同写入 Extra，供审计和幂等对账使用。
//
// 参数一致性检查：
//
//	同一 request_id 的请求，所有业务参数必须完全相同（通过 SHA-256 指纹校验）。
//	参数不一致时写 Fatal 日志并终止进程——这是调用方的严重编程错误。
func (s *accountingService) idempotentBooking(ctx context.Context, req *DoubleEntryBookingRequest) (string, []string, error) {

	reqHash := computeRequestHash(req)
	bno := req.BusinessNo
	btype := string(req.BusinessType)
	orderExtra := orderExtra{} // 用于存储订单额外信息（如请求指纹、transaction_ids），避免多次解析 Extra 字段

	for attempt := 0; attempt < idempotentMaxAttempts; attempt++ {
		order, err := s.orderRepo.GetByOrderKey(ctx, req.RequestID, btype, bno)
		if err != nil {
			return "", nil, fmt.Errorf("idempotency: get order: %w", err)
		}
		if order != nil {
			orderExtra := s.parseOrderExtra(order.Extra)
			// ── 参数一致性校验（任何状态下均检查）─────────────────────────────
			s.checkRequestConsistency(req, reqHash, order)
			switch order.Status {
			case model.TransactionOrderStatusSuccess:
				// 已成功：返回存储的预生成 ID（幂等响应）
				s.logger.Info("idempotent: returning cached result",
					zap.String("requestID", req.RequestID),
					zap.String("voucherNo", order.VoucherNo))
				return order.VoucherNo, orderExtra.TxIDs, nil

			case model.TransactionOrderStatusProcessing:
				// 另一个并发请求正在执行，告知调用方重试
				return order.VoucherNo, orderExtra.TxIDs, ErrRequestInProgress

			case model.TransactionOrderStatusPending, model.TransactionOrderStatusFailed:
				// 读取订单中预存的 ID，保证重试使用相同凭证号和流水号（orderExtra 已在上方解析）
				voucherNo, txIDs, err := s.ensureIDs(ctx, req, orderExtra)
				if err != nil {
					return order.VoucherNo, orderExtra.TxIDs, fmt.Errorf("idempotency: ensure IDs: %w", err)
				}

				// **关键**：cut_date 从 orderExtra 持久化值读，永不重算。
				// 升级前的老订单 Extra 中可能没有 cut_date —— 用当时的本地值兜底
				// （没有更好的来源；新订单一律有 cut_date）。
				cutDate := orderExtra.CutDate
				if cutDate == "" {
					cutDate = s.resolveCutDate(time.Now())
					s.logger.Warn("idempotency: legacy order has no persisted cut_date, using local fallback (only happens for pre-upgrade data)",
						zap.String("requestID", req.RequestID),
						zap.String("voucherNo", voucherNo),
						zap.String("cutDate", cutDate))
				}

				// CAS 抢锁：只有 pending/failed → processing 成功者才执行
				affected, casErr := s.orderRepo.UpdateToProcessing(ctx, req.RequestID, btype, bno)
				if casErr != nil {
					return order.VoucherNo, orderExtra.TxIDs, fmt.Errorf("idempotency: claim order: %w", casErr)
				}
				if affected == 0 {
					continue // 被其他请求抢先，重新循环检查状态
				}
				return s.executeAndRecord(ctx, req, reqHash, voucherNo, cutDate, txIDs)
			}
		}

		// 订单不存在：预生成所有 ID + **此处一次性算定 cut_date**，写入 Extra。
		// 这是 cut_date 的唯一权威源；后续所有路径（hotPath/tcc/Recovery）一律
		// 从此值传递，永不重算。即便服务重启 / config 修改 / DB 抖动，本订单的
		// cut_date 永不改变 = 同 voucher 全部 entry 必然共享同一 cut_date。
		voucherNo, err := s.generateVoucherNo(ctx, req.BusinessNo)
		if err != nil {
			return "", nil, fmt.Errorf("idempotency: generate voucher no: %w", err)
		}
		txIDs := make([]string, len(req.Entries))
		for i, e := range req.Entries {
			txIDs[i], err = s.generateTransactionID(ctx, e.AccountNo)
			if err != nil {
				return "", nil, fmt.Errorf("idempotency: generate transaction id: %w", err)
			}
		}
		cutDate := s.resolveCutDate(time.Now())
		newOrder := &model.TransactionOrder{
			VoucherNo:    voucherNo,
			OrderNo:      req.RequestID,
			BusinessNo:   bno,
			BusinessType: btype,
			Description:  req.Description,
			Currency:     req.Currency,
			Extra:        buildOrderExtra(reqHash, voucherNo, cutDate, txIDs, buildReqParams(req)),
			Status:       model.TransactionOrderStatusPending,
		}

		if createErr := s.orderRepo.Create(ctx, newOrder); createErr != nil {
			if isDuplicateKeyError(createErr) {
				continue // 并发请求已创建，重新循环读取其写入的 ID
			}
			return voucherNo, orderExtra.TxIDs, fmt.Errorf("idempotency: create order: %w", createErr)
		}

		// 创建成功，立即抢锁执行（使用本次预生成的 ID + cut_date）
		affected, casErr := s.orderRepo.UpdateToProcessing(ctx, req.RequestID, btype, bno)
		if casErr != nil {
			return "", nil, fmt.Errorf("idempotency: claim new order: %w", casErr)
		}
		if affected == 0 {
			continue
		}
		return s.executeAndRecord(ctx, req, reqHash, voucherNo, cutDate, txIDs)
	}
	return "", nil, fmt.Errorf("idempotency: failed to acquire lock for request_id=%s after retries", req.RequestID)
}

// ensureIDs 从已存储的 Extra 中读取预生成 ID；若因升级等原因 Extra 中无 ID，则重新生成。
// 重新生成不影响幂等性：此时说明旧订单从未执行过（PENDING/FAILED 且无 ID 记录）。
func (s *accountingService) ensureIDs(ctx context.Context, req *DoubleEntryBookingRequest, extra orderExtra) (voucherNo string, txIDs []string, err error) {
	voucherNo = extra.VoucherNo
	if voucherNo == "" {
		voucherNo, err = s.generateVoucherNo(ctx, req.BusinessNo)
		if err != nil {
			return "", nil, err
		}
	}
	txIDs = extra.TxIDs
	if len(txIDs) != len(req.Entries) {
		txIDs = make([]string, len(req.Entries))
		for i, e := range req.Entries {
			txIDs[i], err = s.generateTransactionID(ctx, e.AccountNo)
			if err != nil {
				return "", nil, err
			}
		}
	}
	return voucherNo, txIDs, nil
}

// checkRequestConsistency 校验请求参数与已存储指纹是否一致。
// 不一致说明调用方将同一 request_id 用于不同请求，属于严重编程错误，写 Fatal 日志终止进程。
func (s *accountingService) checkRequestConsistency(req *DoubleEntryBookingRequest, reqHash string, order *model.TransactionOrder) {
	stored := s.parseOrderExtra(order.Extra)
	if stored.ReqHash == "" {
		// 旧数据（升级前写入）无指纹，跳过校验
		return
	}
	if stored.ReqHash == reqHash {
		return
	}
	s.logger.Fatal("IDEMPOTENCY VIOLATION: request_id reused with different parameters",
		zap.String("requestID", req.RequestID),
		zap.String("storedHash", stored.ReqHash),
		zap.String("incomingHash", reqHash),
		zap.String("businessNo", req.BusinessNo),
		zap.String("businessType", string(req.BusinessType)),
		zap.String("currency", req.Currency),
		zap.Int("entryCount", len(req.Entries)),
	)
}

// executeAndRecord 执行记账（使用预生成 ID）并将结果写回 TransactionOrder
//
// voucherNo 和 txIDs 均在 idempotentBooking 创建订单时已预生成，
// 此处直接使用，不重新生成，确保重试幂等性。
func (s *accountingService) executeAndRecord(ctx context.Context, req *DoubleEntryBookingRequest, reqHash, voucherNo, cutDate string, txIDs []string) (string, []string, error) {
	bno := req.BusinessNo
	btype := string(req.BusinessType)
	vno, txs, err := s.doBooking(ctx, req, voucherNo, cutDate, txIDs)
	if err != nil {
		_ = s.orderRepo.UpdateFailed(ctx, req.RequestID, btype, bno, err.Error())
		return "", nil, err
	}
	// UpdateSuccess 只更新 status、voucher_no；reqParams + cut_date 已在订单创建时写入，无需重复写
	extra := buildOrderExtra(reqHash, vno, cutDate, txs, nil)
	if updateErr := s.orderRepo.UpdateSuccess(ctx, req.RequestID, btype, bno, vno, extra); updateErr != nil {
		// 结果已写成功，记录日志但不覆盖成功响应
		s.logger.Error("idempotency: UpdateSuccess failed", zap.Error(updateErr),
			zap.String("requestID", req.RequestID))
	}
	return vno, txs, nil
}

// doBooking 实际执行记账（热路径 or TCC），不含幂等逻辑。
//
// voucherNo 和 txIDs 由 idempotentBooking 层预生成后传入，不在此处重新生成。
//
// 路由规则：
//
//	热路径（Redis + Kafka）：hot_path.enabled=true
//	                         且 该笔记账的所有账户均在 hot_path.account_allowlist 中
//	                         且 账户已预热到 Redis
//	TCC 冷路径            ：其余所有情况
//
// 重要：白名单内的账户 cache miss 时不降级 TCC。
//
//	原因：热路径账户的权威余额在 Redis，MySQL 可能滞后（Kafka 未消费完）；
//	      若降级 TCC 读了偏高的 MySQL 余额会导致双花。
//	处理：触发后台预热，向调用方返回 ErrCacheWarming，让其稍后重试。
func (s *accountingService) doBooking(ctx context.Context, req *DoubleEntryBookingRequest, voucherNo, cutDate string, txIDs []string) (string, []string, error) {
	// ── 热路径：Redis 原子操作 + MySQL Outbox 持久化 ──────────────────────────
	if s.balanceCache != nil && s.allInAllowlist(req.Entries) {
		vno, txs, err := s.hotPathBooking(ctx, req, voucherNo, cutDate, txIDs)
		if err == nil {
			metrics.BookingTotal.WithLabelValues("hot").Inc()
			return vno, txs, nil
		}
		if errors.Is(err, cache.ErrAccountNotInCache) {
			// 白名单账户未在缓存：触发预热，拒绝降级 TCC（避免读脏 MySQL 余额双花）
			s.logger.Warn("hot path account cache miss, warming in background, caller should retry",
				zap.String("businessNo", req.BusinessNo))
			metrics.WarmAccountsTotal.Inc()
			metrics.HotPathFallbackTotal.WithLabelValues("cache_miss").Inc()
			go s.warmAccounts(req.Entries)
			return "", nil, fmt.Errorf("account cache warming in progress, please retry: %w", err)
		}
		// Redis 故障或余额不足，直接向上透传
		metrics.HotPathFallbackTotal.WithLabelValues("redis_error").Inc()
		return "", nil, err
	}

	// ── 冷路径：并行 TCC ──────────────────────────────────────────────────────
	// 账户不在白名单（或热路径未启用）：MySQL 是权威余额来源，TCC 保证跨库原子性
	vno, txs, err := s.tccBooking(ctx, req, voucherNo, cutDate, txIDs)
	if err == nil {
		metrics.BookingTotal.WithLabelValues("tcc").Inc()
	}
	return vno, txs, err
}

// orderExtra TransactionOrder.Extra 字段的统一结构
//
// 设计原则：所有需要保证幂等的 ID（voucherNo、txIDs）在订单创建时一次性生成并写入，
// 后续重试直接读取，确保同一 requestId 始终使用相同的凭证号和流水号。
type orderExtra struct {
	ReqHash   string     `json:"req_hash"`             // 请求参数 SHA-256 指纹（一致性校验）
	VoucherNo string     `json:"voucher_no,omitempty"` // 预生成凭证号（创建时写入，重试时复用）
	TxIDs     []string   `json:"tx_ids,omitempty"`     // 预生成流水号（每条分录一个，创建时写入）
	// CutDate 日切归属日（YYYY-MM-DD）。**第一次创建订单那一刻就定死，后续所有
	// 重试 / 恢复 / Phase 2 Confirm 一律读本字段，永不重算**。即便 SystemConfig
	// 的 day_cut.scheduled_time 在重试间被修改、CutDateProvider 热重载、跨日切
	// 边界、DB 重启 + 多 pod 时钟漂移，同一 voucher 全部 entry 的 cut_date 必然
	// = 本字段。这是试算平衡按 cut_date 过滤天然成立的最终保证。
	CutDate   string     `json:"cut_date,omitempty"`
	ReqParams *reqParams `json:"req_params,omitempty"` // 完整请求参数快照（创建时写入，供审计/重试对账）
}

// reqParams 请求参数快照，存入 Extra 用于审计和幂等对账
type reqParams struct {
	BusinessNo   string       `json:"business_no"`
	BusinessType string       `json:"business_type"`
	Currency     string       `json:"currency"`
	Description  string       `json:"description"`
	Entries      []entryParam `json:"entries"`
}

// entryParam 单条分录参数快照
type entryParam struct {
	AccountNo    string `json:"account_no"`
	DebitAmount  string `json:"debit_amount"`
	CreditAmount string `json:"credit_amount"`
	Description  string `json:"description,omitempty"`
}

// parseOrderExtra 反序列化 Extra 字段
func (s *accountingService) parseOrderExtra(extra string) orderExtra {
	if extra == "" {
		return orderExtra{}
	}
	var v orderExtra
	if err := json.Unmarshal([]byte(extra), &v); err != nil {
		// Extra 字段损坏：幂等信息丢失，将当前请求视为全新请求
		s.logger.Warn("parseOrderExtra: corrupt extra field, treating as new request",
			zap.String("extra_prefix", extra[:min(len(extra), 80)]), zap.Error(err))
		return orderExtra{}
	}
	return v
}

// buildOrderExtra 序列化 Extra 字段
func buildOrderExtra(reqHash, voucherNo, cutDate string, txIDs []string, params *reqParams) string {
	b, err := json.Marshal(orderExtra{
		ReqHash:   reqHash,
		VoucherNo: voucherNo,
		TxIDs:     txIDs,
		CutDate:   cutDate,
		ReqParams: params,
	})
	if err != nil {
		// 实际上不可能失败（所有字段均为 string/slice）
		return "{}"
	}
	return string(b)
}

// buildReqParams 将请求参数转换为可持久化快照
func buildReqParams(req *DoubleEntryBookingRequest) *reqParams {
	entries := make([]entryParam, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = entryParam{
			AccountNo:    e.AccountNo,
			DebitAmount:  strconv.FormatInt(e.DebitAmount, 10),
			CreditAmount: strconv.FormatInt(e.CreditAmount, 10),
			Description:  e.Description,
		}
	}
	return &reqParams{
		BusinessNo:   req.BusinessNo,
		BusinessType: string(req.BusinessType),
		Currency:     req.Currency,
		Description:  req.Description,
		Entries:      entries,
	}
}

// computeRequestHash 计算请求参数的 SHA-256 指纹
//
// 覆盖字段：business_no, business_type, currency, description, entries（按 account_no 排序）
// request_id 本身不参与计算（它是 key，不是 value）。
func computeRequestHash(req *DoubleEntryBookingRequest) string {
	type entryFingerprint struct {
		AccountNo    string `json:"a"`
		DebitAmount  string `json:"d"`
		CreditAmount string `json:"c"`
	}
	entries := make([]entryFingerprint, len(req.Entries))
	for i, e := range req.Entries {
		entries[i] = entryFingerprint{
			AccountNo:    e.AccountNo,
			DebitAmount:  strconv.FormatInt(e.DebitAmount, 10),
			CreditAmount: strconv.FormatInt(e.CreditAmount, 10),
		}
	}
	// 规范排序：防止调用方条目顺序不同时产生不同 hash
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].AccountNo < entries[j].AccountNo
	})

	payload := struct {
		BusinessNo   string             `json:"business_no"`
		BusinessType model.BusinessType `json:"business_type"`
		Currency     string             `json:"currency"`
		Description  string             `json:"description"`
		Entries      []entryFingerprint `json:"entries"`
	}{
		BusinessNo:   req.BusinessNo,
		BusinessType: req.BusinessType,
		Currency:     req.Currency,
		Description:  req.Description,
		Entries:      entries,
	}
	b, _ := json.Marshal(payload)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hotPathBooking Redis Lua 原子转账 + MySQL Outbox 持久化
//
// 安全保证（Transactional Outbox Pattern）：
//
//  1. 从 Redis 读取变更前余额，构造 SettlementEvent
//  2. 将 SettlementEvent 写入 settlement_outbox（status=PENDING）—— 持久化 WAL
//     若此步失败：Redis 未动，整笔请求报错，无任何副作用
//  3. Redis Lua 原子转账（全量校验 + 全量变更，原子执行）
//     若此步失败：将 outbox 标记 FAILED，返回错误（outbox 有记录可审计，Redis 未变更）
//  4. 异步将 outbox 状态置为 REDIS_DONE（非阻塞）
//     即使此步丢失（进程崩溃），RecoveryWorker 会在 30s 后检测 PENDING 并重试
//  5. 返回成功
//
// OutboxWorker 后台消费 REDIS_DONE → 持久化 account_transaction + 更新 account balance
// → 置 MYSQL_DONE。MySQL 最终必然与 Redis 对齐，资金 100% 不丢失。
func (s *accountingService) hotPathBooking(ctx context.Context, req *DoubleEntryBookingRequest, voucherNo, cutDate string, txIDs []string) (string, []string, error) {
	now := time.Now()
	// cutDate 由调用方（idempotentBooking 创建订单那一刻）决定并持久化在
	// TransactionOrder.Extra，从这里通过参数传入。本函数永不调 resolveCutDate
	// 重算 —— 抗时钟漂移、抗 SystemConfig reload、抗多 pod / DB 重启 / 重试。

	// ── Step 1: 读取变更前余额，构造 delta map 和流水快照 ────────────────────────
	type entrySnapshot struct {
		txID   string
		before string
		delta  string
	}
	snapshots := make([]entrySnapshot, len(req.Entries))
	deltaMap := make(map[string]string, len(req.Entries))

	for i, e := range req.Entries {
		snapshots[i].txID = txIDs[i] // 使用预生成的流水号，保证幂等

		info, err := s.balanceCache.GetBalance(ctx, e.AccountNo)
		if err != nil {
			return "", nil, err // ErrAccountNotInCache → 调用方触发预热重试
		}
		snapshots[i].before = info.Available

		isAsset := info.Category == 1
		delta := computeBalanceDeltaStr(e, isAsset)
		snapshots[i].delta = delta
		deltaMap[e.AccountNo] = delta
	}

	// ── Step 2: 构造 SettlementEvent（BalanceAfter = before + delta，确定性计算）────
	entries := make([]model.SettlementEntry, len(req.Entries))
	for i, e := range req.Entries {
		before, err := strconv.ParseInt(snapshots[i].before, 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("hot path: parse balance_before for %s: %w", e.AccountNo, err)
		}
		delta, err := strconv.ParseInt(snapshots[i].delta, 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("hot path: parse balance_delta for %s: %w", e.AccountNo, err)
		}
		after := before + delta
		entries[i] = model.SettlementEntry{
			TransactionID: snapshots[i].txID,
			AccountNo:     e.AccountNo,
			DebitAmount:   strconv.FormatInt(e.DebitAmount, 10),
			CreditAmount:  strconv.FormatInt(e.CreditAmount, 10),
			BalanceDelta:  snapshots[i].delta,
			BalanceBefore: snapshots[i].before,
			BalanceAfter:  strconv.FormatInt(after, 10),
		}
	}
	event := model.SettlementEvent{
		VoucherNo:    voucherNo,
		BusinessNo:   req.BusinessNo,
		BusinessType: req.BusinessType,
		Currency:     req.Currency,
		Description:  req.Description,
		Entries:      entries,
		TransactAt:   now,
	}

	// ── Step 3: 写入 MySQL Outbox（WAL）—— 必须在 Redis 更新之前 ─────────────────
	eventBytes, err := json.Marshal(event)
	if err != nil {
		return "", nil, fmt.Errorf("hot path: marshal settlement event: %w", err)
	}
	// **关键**：cut_date 在本入口处一次性算定（cutDate := s.resolveCutDate(now)），
	// 直接持久化到 outbox 行；OutboxWorker / 重投 / 多 pod 一律读本字段，
	// 永远不再 time.Now() 或 CutDateProvider 重算。这保证了即便 SystemConfig
	// 变更、时钟漂移、DB 重启、跨 pod replay，同一 voucher 的所有 entry
	// 拿到的 cut_date 都是入口当时锁定的同一个值 → 试算平衡天然成立。
	outbox := &model.SettlementOutbox{
		VoucherNo:       voucherNo,
		EventData:       string(eventBytes),
		TransactionDate: now.Format("2006-01-02"),
		CutDate:         cutDate,
		Status:          model.OutboxStatusPending,
	}
	if createErr := s.outboxRepo.Create(ctx, outbox); createErr != nil {
		if !isDuplicateKeyError(createErr) {
			// Outbox 写入失败：Redis 完全未动，无副作用，安全返回错误
			return "", nil, fmt.Errorf("hot path: outbox write failed: %w", createErr)
		}
		// DUPLICATE KEY：上次执行在 Redis 失败后将 outbox 置 FAILED，本次重试需重置为 PENDING。
		// 若非 FAILED 状态（PENDING/REDIS_DONE/MYSQL_DONE），说明存在并发冲突，返回错误。
		if resetErr := s.outboxRepo.ResetFailedToPending(ctx, outbox); resetErr != nil {
			return "", nil, fmt.Errorf("hot path: outbox reset for retry failed: %w", resetErr)
		}
	}

	// ── Step 4: Redis Lua 原子转账（voucherNo 级幂等保护）──────────────────────
	// Transfer 内部 SET NX 幂等哨兵；若本 voucher 的 delta 已被 Redis 应用过（上次崩溃
	// 在 Transfer 成功与 MarkRedisDone 之间），Recovery Worker 重跑时会走 already_applied
	// 分支，不会 double-count。
	if err := s.balanceCache.Transfer(ctx, voucherNo, deltaMap); err != nil {
		// Redis 失败：outbox 有 PENDING 记录（可审计），余额未变更
		_ = s.outboxRepo.MarkFailed(ctx, voucherNo, err.Error())
		return "", nil, err
	}

	// ── Step 5: 异步置 REDIS_DONE（非阻塞，不影响响应延迟）─────────────────────
	// 使用 shutdownCtx（app 生命周期 context）而非 context.Background()，
	// 确保进程关闭时此 goroutine 可感知退出信号，不泄露。
	// markRedisDoneSem 限制并发 goroutine 数量，防止高并发时内存爆炸。
	markCtx := s.shutdownCtx
	if markCtx == nil {
		markCtx = context.Background()
	}
	// 非阻塞地获取信号量：若已满则跳过（RecoveryWorker 兜底），避免阻塞热路径
	select {
	case s.markRedisDoneSem <- struct{}{}:
		go func() {
			defer func() { <-s.markRedisDoneSem }()
			bCtx, cancel := context.WithTimeout(markCtx, markRedisDoneTimeout)
			defer cancel()
			if err := s.outboxRepo.MarkRedisDone(bCtx, voucherNo); err != nil {
				// 非致命：RecoveryWorker 会在 outboxRecoveryInterval 后检测 PENDING 并重试 Redis
				s.logger.Warn("hot path: mark outbox redis_done failed (recovery will handle)",
					zap.Error(err), zap.String("voucherNo", voucherNo))
				return
			}
			// MarkRedisDone 成功 → 若启用了 Kafka push，发通知让 OutboxWorker 立即处理，
			// 不用等下一轮 100ms 轮询。消息丢失也无害（polling 兜底）。
			if s.outboxNotifier != nil {
				if err := s.outboxNotifier.SendMessage(bCtx, voucherNo, "outbox_ready", voucherNo); err != nil {
					s.logger.Debug("hot path: outbox notify kafka send failed (polling will handle)",
						zap.String("voucherNo", voucherNo), zap.Error(err))
				}
			}
		}()
	default:
		// 并发 goroutine 已达上限，跳过异步标记，由 RecoveryWorker 补偿
		s.logger.Debug("hot path: mark redis_done skipped (semaphore full, recovery will handle)",
			zap.String("voucherNo", voucherNo))
	}

	// ── Step 6: 可选 Kafka 通知（下游实时消费，非资金安全关键路径）──────────────
	if s.settlementWriter != nil {
		if err := s.settlementWriter.SendMessage(ctx, voucherNo, "settlement", event); err != nil {
			// Kafka 失败可以安全忽略：outbox 已保证 MySQL 最终落库
			s.logger.Warn("hot path: kafka notify failed (non-critical, outbox ensures durability)",
				zap.Error(err), zap.String("voucherNo", voucherNo))
		}
	}

	s.logger.Debug("hot path booking success",
		zap.String("voucherNo", voucherNo), zap.String("businessNo", req.BusinessNo))
	return voucherNo, txIDs, nil // txIDs 是传入的预生成流水号
}

// warmAccounts 后台异步预热账户到 Redis（缓存 miss 后触发）
//
// 雷鸣效应防护（singleflight）：
//
//	Redis 重启后大量账户同时 cache miss，会并发调用 warmAccounts。
//	- outbox 数据获取：用 singleflight key "__fetch_outbox__" 去重，
//	  多个并发调用只做一次跨分片扫描，其余等待并共享结果。
//	- 单账户预热：用 singleflight key = accountNo 去重，
//	  同一账户并发预热只执行一次 MySQL 查询 + Redis 写入。
//
// 读取顺序（消除竞态，保证安全）：
//
//	先读 outbox 快照，再读 MySQL 余额。
//	若 OutboxWorker 在两步之间完成某条 outbox：double-count → Redis 余额偏低（安全）。
//	偏低只会短暂多拒绝几笔，下次预热后自动恢复；偏高会多放行，有双花风险。
func (s *accountingService) warmAccounts(entries []AccountingEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), warmAccountsTimeout)
	defer cancel()

	// ── Step 1: 用 singleflight 去重 outbox 跨分片扫描 ──────────────────────
	var pendingDeltas map[string]int64
	if s.outboxRepo != nil {
		result, err, _ := s.warmGroup.Do("__fetch_outbox__", func() (interface{}, error) {
			unprocessed, err := s.outboxRepo.FindUnprocessed(ctx, "") // 缓存预热不限日期，补偿所有待处理增量
			if err != nil {
				return nil, err
			}
			return computePendingOutboxDeltas(unprocessed), nil
		})
		if err != nil {
			s.logger.Error("warm: failed to load pending outbox, skipping warm to avoid stale balance",
				zap.Error(err))
			return
		}
		if result != nil {
			if m, ok := result.(map[string]int64); ok {
				pendingDeltas = m
			}
		}
	}

	// ── Step 2: 按分片分组，每个分片一次 IN 查询，各分片并行 ────────────────
	// 将 N 个独立 GetAccountByNo 点查合并为 M 个 IN 查询（M = 不同分片数 ≤ 100），
	// 并行执行将预热延迟从 O(N) 降至 O(max_shard_latency)。
	// singleflight 仍保留在单账户维度，防止并发预热同一账户重复写 Redis。
	type warmShardKey struct{ db, tbl int }
	shardAccNos := make(map[warmShardKey][]string)
	shardOrder := make([]warmShardKey, 0)
	seen := make(map[string]bool)
	for _, e := range entries {
		if seen[e.AccountNo] {
			continue
		}
		seen[e.AccountNo] = true
		dbIdx, tableIdx := s.router.RouteByAccountNo(e.AccountNo)
		k := warmShardKey{dbIdx, tableIdx}
		if _, ok := shardAccNos[k]; !ok {
			shardOrder = append(shardOrder, k)
		}
		shardAccNos[k] = append(shardAccNos[k], e.AccountNo)
	}

	var fetchWg sync.WaitGroup
	for _, k := range shardOrder {
		dbIdx, tableIdx := k.db, k.tbl
		accountNos := shardAccNos[k]
		fetchWg.Add(1)
		go func() {
			defer fetchWg.Done()
			accs, err := s.accountRepo.GetAccountsByNos(ctx, accountNos, dbIdx, tableIdx)
			if err != nil {
				s.logger.Warn("warm: batch fetch accounts failed",
					zap.Int("db", dbIdx), zap.Int("table", tableIdx), zap.Error(err))
				return
			}
			for _, acc := range accs {
				acc := acc
				var delta int64
				if pendingDeltas != nil {
					delta = pendingDeltas[acc.AccountNo]
				}
				// singleflight 去重：同一账户的并发预热只执行一次 Redis 写
				s.warmGroup.Do(acc.AccountNo, func() (interface{}, error) { //nolint:errcheck
					s.doWarmFromAcc(ctx, acc, delta)
					return nil, nil
				})
			}
		}()
	}
	fetchWg.Wait()
}

// doWarmFromAcc 直接用已查到的 account 对象预热 Redis，无需再次查 MySQL。
func (s *accountingService) doWarmFromAcc(ctx context.Context, acc *model.Account, pendingDelta int64) {
	balance := acc.Balance + pendingDelta
	available := acc.AvailableBalance + pendingDelta
	cat := 1
	if !isAssetOrExpense(acc) {
		cat = 2
	}
	status := 1
	if acc.Status != model.AccountStatusActive {
		status = int(acc.Status)
	}
	if err := s.balanceCache.WarmAccount(ctx, cache.BalanceInfo{
		AccountNo: acc.AccountNo,
		Balance:   strconv.FormatInt(balance, 10),
		Available: strconv.FormatInt(available, 10),
		Frozen:    strconv.FormatInt(acc.FrozenBalance, 10),
		Version:   acc.Version,
		Category:  cat,
		Status:    status,
	}); err != nil {
		s.logger.Warn("warm account failed", zap.String("accountNo", acc.AccountNo), zap.Error(err))
	}
}

// doWarmAccount 读 MySQL account 余额并叠加 outbox delta 后预热 Redis（供 singleflight 调用）
func (s *accountingService) doWarmAccount(ctx context.Context, accountNo string, pendingDelta int64) {
	acc, err := s.accountRepo.GetAccountByNo(ctx, accountNo)
	if err != nil || acc == nil {
		return
	}
	balance := acc.Balance + pendingDelta
	available := acc.AvailableBalance + pendingDelta

	cat := 1
	if !isAssetOrExpense(acc) {
		cat = 2
	}
	status := 1
	if acc.Status != model.AccountStatusActive {
		status = int(acc.Status)
	}
	if err := s.balanceCache.WarmAccount(ctx, cache.BalanceInfo{
		AccountNo: acc.AccountNo,
		Balance:   strconv.FormatInt(balance, 10),
		Available: strconv.FormatInt(available, 10),
		Frozen:    strconv.FormatInt(acc.FrozenBalance, 10),
		Version:   acc.Version,
		Category:  cat,
		Status:    status,
	}); err != nil {
		s.logger.Warn("warm account failed", zap.String("accountNo", accountNo), zap.Error(err))
	}
}

// computeBalanceDeltaStr 计算余额净增量字符串（用于 Redis Lua 脚本，整数格式）
func computeBalanceDeltaStr(entry AccountingEntry, isAsset bool) string {
	return strconv.FormatInt(computeBalanceDelta(entry, isAsset), 10)
}

// tccBooking 并行 TCC 实现
//
// 性能关键优化（相较于朴素逐账户开事务方案）：
//
//  1. DB 级事务合并：同一物理 DB 上的所有分录共享一个 MySQL 事务。
//     例如 4 分录打到 2 个 DB：Try 阶段 2 个事务（非 4 个），Confirm 阶段同理。
//     对于 innodb_flush_log_at_trx_commit=1（全持久化），每笔 COMMIT 触发 fsync；
//     减少 COMMIT 次数 = 减少 fsync = 直接降低 99th 延迟。
//
//  2. DB 间完全并行：不同 DB 的事务通过 goroutine 并发执行，无相互等待。
//
//  3. DB 内按 account_no 排序加锁：全局一致的行锁顺序，彻底消除同 DB 内的死锁风险。
//
//  4. 省略冗余校验：validateEntries 已由外层 DoubleEntryBooking/idempotentBooking 调用。
func (s *accountingService) tccBooking(ctx context.Context, req *DoubleEntryBookingRequest, voucherNo, cutDate string, transactionIDs []string) (string, []string, error) {
	now := time.Now()
	transactionDate := now.Format("2006-01-02")
	// cutDate 由调用方（idempotentBooking 创建订单那一刻）决定并持久化在
	// TransactionOrder.Extra，从这里通过参数传入。本函数永不调 resolveCutDate
	// 重算 —— 抗时钟漂移、抗 SystemConfig reload、抗多 pod / DB 重启 / 重试。

	// ── TCC 协调者：写入 TRYING 阶段标记（Risk A 修复）────────────────────────────
	// RecoveryWorker 依据此记录判断超时事务是 Cancel（TRYING）还是需要告警不取消（CONFIRMING）。
	// 写入失败为非致命错误：协调者不存在时 RecoveryWorker 退化为原有行为（扫描分支记录）。
	if s.tccCoordRepo != nil {
		if coordErr := s.tccCoordRepo.Create(ctx, voucherNo, req.BusinessNo, cutDate, req.Currency, len(req.Entries)); coordErr != nil {
			s.logger.Warn("tcc coordinator create failed (non-fatal, recovery may fall back to branch scan)",
				zap.String("voucherNo", voucherNo), zap.Error(coordErr))
		}
	}

	// shardedEntry 在分组时一并计算路由，消除 goroutine 内重复调用 RouteByAccountNo。
	type shardedEntry struct {
		idx      int
		entry    AccountingEntry
		dbIdx    int
		tableIdx int
	}

	// 按物理 DB 分组（key = dbIndex，同一 DB 不同 table 合并为一个事务）
	dbGroupMap := make(map[int][]shardedEntry)
	dbGroupKeys := make([]int, 0, len(req.Entries))
	for i, e := range req.Entries {
		dbIdx, tableIdx := s.router.RouteByAccountNo(e.AccountNo)
		if _, ok := dbGroupMap[dbIdx]; !ok {
			dbGroupKeys = append(dbGroupKeys, dbIdx)
		}
		dbGroupMap[dbIdx] = append(dbGroupMap[dbIdx], shardedEntry{i, e, dbIdx, tableIdx})
	}
	// 每个 DB 组内按 account_no 升序排列，保证全局一致的行锁顺序（防死锁）
	for dbIdx := range dbGroupMap {
		sort.Slice(dbGroupMap[dbIdx], func(a, b int) bool {
			return dbGroupMap[dbIdx][a].entry.AccountNo < dbGroupMap[dbIdx][b].entry.AccountNo
		})
	}

	// ── Phase 1: 并行 Try（每 DB 一个事务）──────────────────────────────────
	//
	// outcomes[i].err == nil 表示第 i 条分录 Try 成功。
	// outcomes[i].balanceDelta 由 Try goroutine 填充，供 Confirm 阶段直接使用，
	// 避免 Confirm 重复执行 GetBranchForUpdate（正常路径节省 1 次 SELECT FOR UPDATE/entry）。
	// DB 组内任意分录失败 → 整组事务回滚 → 组内所有分录标记失败（保证原子性）。
	type tryOutcome struct {
		meta         tccBranchMeta
		balanceDelta int64 // 非零：Try 阶段已计算，Confirm 可跳过 GetBranchForUpdate
		err          error
	}
	outcomes := make([]tryOutcome, len(req.Entries))

	// entryDelta 关联 tableIdx + balanceDelta（供 Confirm 阶段跳过 GetBranchForUpdate）
	type entryDelta struct {
		tableIdx     int
		balanceDelta int64
	}

	var wg sync.WaitGroup
	for _, dbIdx := range dbGroupKeys {
		dbIdx, group := dbIdx, dbGroupMap[dbIdx]
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := s.dbManager.GetDB(dbIdx)
			if err != nil {
				for _, se := range group {
					outcomes[se.idx].err = fmt.Errorf("get db[%d]: %w", dbIdx, err)
				}
				return
			}

			// 包一层 retryOnDeadlock：InnoDB gap-lock 死锁（热点账户 fee / platform 上常见）
			// 由本函数内部 rollback 释放所有锁 + jitter 退避后重试，整组 Try 再做一遍。
			// branchID 全局唯一、幂等检查已由 tccTry 内 GetBranch 处理，重试安全。
			var pendingDeltas []entryDelta
			tryErr := s.retryOnDeadlock(func() error {
				tx := db.WithContext(ctx).Begin()
				if tx.Error != nil {
					return fmt.Errorf("begin tx: %w", tx.Error)
				}

				localDeltas := make([]entryDelta, len(group))
				for gi, se := range group {
					branchID := transactionIDs[se.idx]

					account, accErr := s.accountRepo.GetAccountForUpdate(ctx, tx, se.entry.AccountNo, se.dbIdx, se.tableIdx)
					if accErr != nil {
						tx.Rollback()
						return fmt.Errorf("get account %s: %w", se.entry.AccountNo, accErr)
					}
					if account == nil {
						tx.Rollback()
						return fmt.Errorf("account not found: %s", se.entry.AccountNo)
					}
					// balanceDelta 与 tccTry 内部计算一致；在 Try 提交后写入 outcomes，
					// Confirm goroutine 直接复用，无需再次 SELECT tcc_transaction。
					delta := computeBalanceDelta(se.entry, isAssetOrExpense(account))
					if tryErr := s.tccTry(ctx, tx, voucherNo, branchID, se.entry, account, se.dbIdx, se.tableIdx); tryErr != nil {
						tx.Rollback()
						return fmt.Errorf("tcc try account=%s: %w", se.entry.AccountNo, tryErr)
					}
					localDeltas[gi] = entryDelta{se.tableIdx, delta}
				}
				if commitErr := tx.Commit().Error; commitErr != nil {
					return fmt.Errorf("commit try db[%d]: %w", dbIdx, commitErr)
				}
				pendingDeltas = localDeltas
				return nil
			})

			if tryErr != nil {
				for _, se := range group {
					outcomes[se.idx].err = tryErr
				}
				return
			}
			// 整组 Try 成功，填充 meta + balanceDelta（供 Cancel 和 Confirm 使用）
			for gi, se := range group {
				outcomes[se.idx].meta = tccBranchMeta{se.dbIdx, pendingDeltas[gi].tableIdx, transactionIDs[se.idx], se.entry}
				outcomes[se.idx].balanceDelta = pendingDeltas[gi].balanceDelta
			}
		}()
	}
	wg.Wait()

	// 汇总 Try 结果
	tried := make([]tccBranchMeta, 0, len(req.Entries))
	var tryErr error
	for _, o := range outcomes {
		if o.err != nil {
			if tryErr == nil {
				tryErr = o.err
			}
		} else {
			tried = append(tried, o.meta)
		}
	}
	if tryErr != nil {
		// 使用脱离请求 ctx 的独立上下文执行清理操作：
		// 若调用方已超时/取消，清理操作仍需完成以释放冻结资金。
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cleanupCancel()

		// 协调者：Try 失败 → 标记 CANCELLED（非致命，失败仅影响 Recovery Worker 判断）
		if s.tccCoordRepo != nil {
			if cErr := s.tccCoordRepo.TransitionToCancelled(cleanupCtx, voucherNo); cErr != nil {
				s.logger.Warn("tcc coordinator cancel transition failed",
					zap.String("voucherNo", voucherNo), zap.Error(cErr))
			}
		}
		s.cancelTried(cleanupCtx, tried)
		return "", nil, fmt.Errorf("tcc try: %w", tryErr)
	}

	// ── 独立 Confirm 上下文（脱离请求 ctx）────────────────────────────────────
	//
	// 问题：若 gRPC 请求在 Try 成功后、Confirm 完成前超时，使用请求 ctx 会导致所有
	// Confirm goroutine 立即失败，coordinator 永久停留 CONFIRMING，须人工介入。
	//
	// 修复：Confirm 阶段使用脱离请求取消的独立上下文（context.WithoutCancel），
	// 并附带 30s 超时，保证 Confirm 操作在调用方断开后仍能完成。
	// 30s > 典型 MySQL 事务耗时（<100ms per shard），足够完成正常 Confirm。
	confirmCtx, confirmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer confirmCancel()

	// ── 协调者：Try 全部成功 → 原子 CAS 进入 CONFIRMING 阶段 ───────────────
	// CAS = UPDATE … WHERE phase = TRYING → CONFIRMING；rows_affected=0 说明
	// recovery worker 已经把 coordinator 改成 CANCELLED 并 cancel 掉了 branch
	// (释放冻结)。此时再继续 Confirm 等于"释放冻结后又应用余额 = 凭空多钱"，
	// 资金会不平。**必须 fail-fast 不再继续 Confirm，让调用方拿到 error 重试。**
	if s.tccCoordRepo != nil {
		if cErr := s.tccCoordRepo.TransitionToConfirming(confirmCtx, voucherNo); cErr != nil {
			s.logger.Error("tcc coordinator confirming CAS failed — aborting confirm to protect fund integrity",
				zap.String("voucherNo", voucherNo), zap.Error(cErr))
			// 致命：返回 error 给上层；不继续 Confirm，避免在已 cancel 的分支上叠加余额变更。
			return "", nil, fmt.Errorf("tcc confirm aborted: coordinator %s no longer in TRYING (recovered/cancelled): %w", voucherNo, cErr)
		}
	}

	// ── Phase 2: 并行 Confirm（每 DB 一个事务，与 Try 相同锁顺序）────────────
	//
	// Confirm 失败极罕见（网络抖动/节点宕机）；失败时不 Cancel，
	// 保留 TRYING 状态由 TccRecoveryWorker 后台重试。
	// 使用 confirmCtx（非请求 ctx）确保 Confirm 在调用方超时后仍能完成。
	confirmErrs := make([]error, len(req.Entries))
	for _, dbIdx := range dbGroupKeys {
		dbIdx, group := dbIdx, dbGroupMap[dbIdx]
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := s.dbManager.GetDB(dbIdx)
			if err != nil {
				for _, se := range group {
					confirmErrs[se.idx] = fmt.Errorf("get db[%d]: %w", dbIdx, err)
					s.logger.Error("tcc confirm: get db failed, needs recovery",
						zap.Error(err), zap.String("voucherNo", voucherNo))
				}
				return
			}

			// 同 Try：整组 Confirm 单事务 + retryOnDeadlock。
			// Confirm 阶段 tccConfirm 内部的 UpdateBranchStatus 幂等（CONFIRMED → CONFIRMED no-op），
			// 重试安全。死锁主要来自 tcc_transaction 的 WHERE branch_id=? UPDATE，跟 Try 同类问题。
			cfmErr := s.retryOnDeadlock(func() error {
				tx := db.WithContext(confirmCtx).Begin()
				if tx.Error != nil {
					return fmt.Errorf("begin tx: %w", tx.Error)
				}
				for _, se := range group {
					branchID := transactionIDs[se.idx]

					account, accErr := s.accountRepo.GetAccountForUpdate(confirmCtx, tx, se.entry.AccountNo, se.dbIdx, se.tableIdx)
					if accErr != nil || account == nil {
						msg := "account not found"
						if accErr != nil {
							msg = accErr.Error()
						}
						tx.Rollback()
						return fmt.Errorf("get account %s: %s", se.entry.AccountNo, msg)
					}
					// outcomes[se.idx].balanceDelta 由 Try 阶段计算并持久化到内存；
					// tccConfirm 将跳过 GetBranchForUpdate SELECT，直接执行余额和流水更新。
					if cfmErr := s.tccConfirm(confirmCtx, tx, branchID, se.entry, account, &bookingParams{
						transactionID:   branchID,
						voucherNo:       voucherNo,
						businessNo:      req.BusinessNo,
						businessType:    req.BusinessType,
						entry:           se.entry,
						currency:        req.Currency,
						transactionDate: transactionDate,
						transactionTime: now,
						description:     req.Description,
						cutDate:         cutDate,
					}, se.dbIdx, se.tableIdx, outcomes[se.idx].balanceDelta, true); cfmErr != nil {
						tx.Rollback()
						return cfmErr
					}
				}
				if commitErr := tx.Commit().Error; commitErr != nil {
					return fmt.Errorf("commit confirm db[%d]: %w", dbIdx, commitErr)
				}
				return nil
			})

			if cfmErr != nil {
				for _, se := range group {
					confirmErrs[se.idx] = cfmErr
				}
				s.logger.Error("tcc confirm failed after retry, needs recovery",
					zap.Error(cfmErr), zap.String("voucherNo", voucherNo))
			}
		}()
	}
	wg.Wait()

	for _, cfmErr := range confirmErrs {
		if cfmErr != nil {
			return "", nil, fmt.Errorf("tcc confirm partial failure (needs recovery): %w", cfmErr)
		}
	}

	// 协调者：所有 Confirm 成功 → 标记 CONFIRMED。
	// cut_date 已在 Create 时写入 + propagate 到所有 entry 的 cut_date 列，
	// 不再需要 finalize fan-out（试算平衡靠 cut_date tag 自然成立）。
	if s.tccCoordRepo != nil {
		if cErr := s.tccCoordRepo.TransitionToConfirmed(confirmCtx, voucherNo); cErr != nil {
			s.logger.Warn("tcc coordinator confirmed transition failed (non-fatal)",
				zap.String("voucherNo", voucherNo), zap.Error(cErr))
		}
	}

	s.logger.Info("tcc booking success",
		zap.String("voucherNo", voucherNo),
		zap.Strings("transactionIDs", transactionIDs),
		zap.String("businessNo", req.BusinessNo),
		zap.Int("dbGroups", len(dbGroupKeys)),
		zap.Int("entries", len(req.Entries)),
	)
	return voucherNo, transactionIDs, nil
}

// fanOutFinalize / shardKey were removed: cut_date is now propagated at write
// time via bookingParams + tcc_coordinator.cut_date, so post-confirm fan-out is
// no longer needed. Trial balance is preserved by every entry sharing the same
// cut_date tag (set once at booking entry).

// tccTry Try 阶段：冻结资金，写入 TCC 分支（TRYING）
// 必须在已开启的事务 tx 中调用（与 GetAccountForUpdate 同一事务）。
func (s *accountingService) tccTry(
	ctx context.Context,
	tx *gorm.DB,
	tccID, branchID string,
	entry AccountingEntry,
	account *model.Account,
	dbIndex, tableIndex int,
) (err error) {
	// 仪表化：观测每个分支 Try 阶段耗时（含锁等待 + UPDATE）
	start := time.Now()
	defer func() { metrics.TccTryDuration.Observe(time.Since(start).Seconds()) }()
	// 幂等：若分支已存在（重试场景），直接返回。
	// 使用 GetBranch（不加 FOR UPDATE）避免 uk_branch_id 的 gap lock 与后续 INSERT 的
	// insert-intent 互相阻塞导致死锁。branchID 预生成全局唯一，并发事务不会争抢同一 ID，
	// 无需行锁保护。
	existing, err := s.tccRepo.GetBranch(ctx, tx, branchID, dbIndex, tableIndex)
	if err != nil {
		return fmt.Errorf("get existing branch: %w", err)
	}
	if existing != nil {
		if existing.Status == model.TccStatusTrying {
			return nil // 已 Try，幂等成功
		}
		return fmt.Errorf("branch already in status %d", existing.Status)
	}

	// 计算余额增量
	balanceDelta := computeBalanceDelta(entry, isAssetOrExpense(account))

	// 平台/中间账户允许负余额，跳过余额检查和冻结
	// 普通账户若余额减少，需检查可用余额并冻结 available_balance
	var frozenAmount int64
	if balanceDelta < 0 && !commonutil.IsAccountCanNegative(account.AccountType) {
		frozenAmount = -balanceDelta
		if account.AvailableBalance < frozenAmount {
			return fmt.Errorf("insufficient available balance: account=%s available=%d required=%d",
				entry.AccountNo, account.AvailableBalance, frozenAmount)
		}
		// 冻结：降低 available_balance（balance 暂不变）
		accountTable := s.router.GetTableName("account", tableIndex)
		result := tx.WithContext(ctx).Table(accountTable).
			Where("account_no = ? AND version = ?", entry.AccountNo, account.Version).
			Updates(map[string]interface{}{
				"available_balance": gorm.Expr("available_balance - ?", frozenAmount),
				"version":           gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			s.logger.Error("tcc try: freeze available_balance failed",
				zap.Error(result.Error),
				zap.String("accountNo", entry.AccountNo),
				zap.Int64("frozenAmount", frozenAmount),
			)
			return fmt.Errorf("freeze available_balance: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			s.logger.Warn("tcc try: version conflict when freezing balance",
				zap.String("accountNo", entry.AccountNo),
				zap.Int64("version", account.Version),
			)
			return fmt.Errorf("freeze available_balance: version conflict on account %s (concurrent modification)", entry.AccountNo)
		}
	}

	// 写入 TCC 分支记录
	branch := &model.TccTransaction{
		TccID:        tccID,
		BranchID:     branchID,
		AccountNo:    entry.AccountNo,
		BalanceDelta: balanceDelta,
		FrozenAmount: frozenAmount,
		Status:       model.TccStatusTrying,
	}
	if err := s.tccRepo.CreateBranch(ctx, tx, branch, dbIndex, tableIndex); err != nil {
		return fmt.Errorf("create tcc branch: %w", err)
	}
	return nil
}

// tccConfirm Confirm 阶段：真正修改 balance，写入流水，更新分支为 CONFIRMED。
// 必须在已开启的事务 tx 中调用（与 GetAccountForUpdate 同一事务）。
//
// deltaKnown：调用方在 Try 阶段是否已计算 balanceDelta。
//   - true（正常路径）：直接使用 knownDelta，跳过 GetBranchForUpdate，
//     并使用 ConfirmBranchFromTrying（WHERE status=TRYING）防止与 RecoveryWorker 竞态。
//   - false（恢复/重试路径）：从 DB 读取分支记录做幂等检查，
//     读取后直接 UpdateBranchStatus（已由 GetBranchForUpdate 锁定且状态已验证）。
//
// 注意：不能用 knownDelta != 0 区分两个路径，因为合法的 balanceDelta 可能为 0
// （借贷相消时），错误地走恢复路径会导致与 RecoveryWorker 竞争 UpdateBranchStatus。
//
// 正常路径操作顺序：
//  1. CreateTransaction（INSERT）
//  2. UPDATE account balance
//  3. ConfirmBranchFromTrying（WHERE status=TRYING）→ 若返回 false 则整个 tx 回滚
//
// ConfirmBranchFromTrying 在最后执行是关键：即使前面的 INSERT/UPDATE 已完成，
// 若分支被 TccRecoveryWorker 取消（状态改为 CANCELLED），本 tx 将回滚所有变更，
// 确保账户余额与分支状态始终一致。
func (s *accountingService) tccConfirm(
	ctx context.Context,
	tx *gorm.DB,
	branchID string,
	entry AccountingEntry,
	account *model.Account,
	params *bookingParams,
	dbIndex, tableIndex int,
	knownDelta int64,
	deltaKnown bool,
) (err error) {
	// 仪表化：Confirm 是 TCC 中最重的一段（INSERT 流水 + UPDATE 余额），
	// 其 p99 直接代表用户感知延迟。
	start := time.Now()
	defer func() { metrics.TccConfirmDuration.Observe(time.Since(start).Seconds()) }()

	var balanceDelta int64

	if deltaKnown {
		// ── 正常路径：Try 阶段已计算 balanceDelta，直接使用，跳过 GetBranchForUpdate ──
		balanceDelta = knownDelta
	} else {
		// ── 恢复/重试路径：需从 DB 读取分支记录做幂等检查 ──
		branch, err := s.tccRepo.GetBranchForUpdate(ctx, tx, branchID, dbIndex, tableIndex)
		if err != nil {
			return fmt.Errorf("get branch: %w", err)
		}
		if branch == nil {
			return fmt.Errorf("tcc branch %s not found", branchID)
		}
		if branch.Status == model.TccStatusConfirmed {
			return nil // 幂等：已 CONFIRMED，直接成功
		}
		if branch.Status == model.TccStatusCancelled {
			return fmt.Errorf("cannot confirm cancelled branch %s", branchID)
		}
		balanceDelta = branch.BalanceDelta
	}

	// account.Balance 来自本事务开头的 GetAccountForUpdate，是当前最新余额
	// （可能与 Try 阶段不同，因为其他并发 Confirm 也在更新同一账户）。
	balanceBefore := account.Balance
	balanceAfter := balanceBefore + balanceDelta

	// 根据账户类型或缓冲记账配置决定记账方式：
	// 1. 平台/中间账户（IsAccountCanNegative）：始终走缓冲路径，减少行锁竞争
	// 2. buffer_account_config 中显式配置的账户：按配置的刷新间隔延迟更新余额
	useBuffer := commonutil.IsAccountCanNegative(account.AccountType) || s.isBufferedAccount(entry.AccountNo)
	bookingType := model.TransactionBookingTypeSync
	if useBuffer {
		bookingType = model.TransactionBookingTypeBuffered
	}

	transaction := &model.AccountTransaction{
		TransactionID:       params.transactionID,
		ParentTransactionID: &params.voucherNo,
		AccountNo:           entry.AccountNo,
		BusinessNo:          params.businessNo,
		BusinessType:        params.businessType,
		DebitAmount:         entry.DebitAmount,
		CreditAmount:        entry.CreditAmount,
		BalanceBefore:       balanceBefore,
		BalanceAfter:        balanceAfter,
		BookingType:         bookingType,
		Currency:            params.currency,
		TransactionDate:     params.transactionDate,
		// transaction_time 必须在持有行锁时取值，使其严格反映余额修改的真实顺序。
		// 若沿用 booking 入口 tccBooking 设置的 params.transactionTime，
		// 并发多笔 Try 排队后 Confirm 真正变更余额时，可能出现 "transaction_time 早
		// 的 tx 实际后变更" 的乱序 → 日切按 transaction_time/transaction_id 排序时
		// 取到错误的 balance_after，导致快照期末余额小于真实余额（试算不平）。
		// transaction_date 仍沿用 params.transactionDate（voucher 日），保证同一凭证
		// 多条分录归同一日。
		TransactionTime:     time.Now(),
		Description:         &params.description,
		Status:              model.TransactionStatusSuccess,
		// CutDate 由 booking 入口 propagate 进来；同 voucher 全部 entry 共享同一值。
		CutDate:             params.cutDate,
		RetryCount:          0,
	}
	if err := s.transactionRepo.CreateTransaction(ctx, tx, transaction, dbIndex, tableIndex); err != nil {
		return fmt.Errorf("create transaction record: %w", err)
	}

	// 缓冲记账模式：流水立即写入，余额增量累计到 buffer 表，
	// 由后台 flush worker 按 flush_scheduled_at 批量刷新，减少对 account 行的锁竞争。
	// flush_scheduled_at 仅在首次 INSERT 时设置（ON DUPLICATE KEY UPDATE 不更新），
	// 形成稳定的任务调度记录；不同账户通过 jitter 错开触发时间，防止批量同时触发。
	if useBuffer {
		interval := s.FlushIntervalForAccount(entry.AccountNo)
		jitter := bufferFlushJitter(entry.AccountNo, interval)
		flushAt := time.Now().Add(interval + jitter)
		s.logger.Debug("tcc confirm: buffered account, accumulating to balance_buffer",
			zap.String("accountNo", entry.AccountNo),
			zap.Int64("delta", balanceDelta),
			zap.String("branchID", branchID),
			zap.Time("flushScheduledAt", flushAt),
		)
		if bufErr := s.bufferRepo.Upsert(ctx, tx, entry.AccountNo, balanceDelta, tableIndex, flushAt); bufErr != nil {
			return fmt.Errorf("upsert balance buffer: %w", bufErr)
		}
	} else {
		// 普通账户：同步更新 balance
		// 若余额增加，同步更新 available_balance（Try 阶段未冻结）；
		// 若余额减少，available_balance 已在 Try 阶段通过冻结扣减，这里只更新 balance。
		accountTable := s.router.GetTableName("account", tableIndex)
		updates := map[string]interface{}{
			"balance": gorm.Expr("balance + ?", balanceDelta),
			"version": gorm.Expr("version + 1"),
		}
		if balanceDelta > 0 {
			updates["available_balance"] = gorm.Expr("available_balance + ?", balanceDelta)
		}
		result := tx.WithContext(ctx).Table(accountTable).
			Where("account_no = ?", entry.AccountNo).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("apply balance delta: %w", result.Error)
		}
		// RowsAffected == 0 说明账户记录不存在（不应发生；Try 阶段已验证账户存在且持行锁）
		if result.RowsAffected == 0 {
			return fmt.Errorf("apply balance delta: account %s not found in shard (db=%d,tbl=%d)",
				entry.AccountNo, dbIndex, tableIndex)
		}
	}

	// 正常路径：使用 ConfirmBranchFromTrying（WHERE status=TRYING）代替 UpdateBranchStatus，
	// 防止极低概率场景（TccRecoveryWorker 在 Confirm 窗口内取消分支）导致余额错误：
	// 若 RowsAffected == 0 说明分支已不在 TRYING 状态，上层 tx 会回滚本次所有变更。
	if deltaKnown {
		ok, cfErr := s.tccRepo.ConfirmBranchFromTrying(ctx, tx, branchID, dbIndex, tableIndex)
		if cfErr != nil {
			return fmt.Errorf("confirm branch status: %w", cfErr)
		}
		if !ok {
			// 分支已被 TccRecoveryWorker 取消或重复确认（极罕见）；上层 tx.Rollback 会撤销本次变更。
			return fmt.Errorf("confirm branch %s: not in TRYING state (may have been cancelled by recovery worker)", branchID)
		}
		return nil
	}
	// 恢复路径：分支已由 GetBranchForUpdate 锁定且状态已验证，直接更新
	return s.tccRepo.UpdateBranchStatus(ctx, tx, branchID, model.TccStatusConfirmed, dbIndex, tableIndex)
}

// tccCancel Cancel 阶段：释放冻结资金，更新分支为 CANCELLED
// 在独立事务中执行，支持空回滚（分支记录不存在时直接成功）。
func (s *accountingService) tccCancel(ctx context.Context, dbIndex, tableIndex int, branchID string, entry AccountingEntry) (err error) {
	// 仪表化：Cancel 通常发生在异常路径（Try 失败 / TCC 超时），
	// p99 上升提示锁竞争或 worker 积压。
	start := time.Now()
	defer func() { metrics.TccCancelDuration.Observe(time.Since(start).Seconds()) }()

	db, err := s.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("get db: %w", err)
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// ── 死锁预防：与 tccConfirm 保持一致的锁顺序（account 先于 tcc_transaction）──
	//
	// tccConfirm 路径：GetAccountForUpdate(account) → ConfirmBranchFromTrying(tcc_transaction)
	// tccCancel  路径：LockAccount(account)          → GetBranchForUpdate(tcc_transaction)
	//
	// 两条路径在同一 DB 的同一账户上争锁时，若锁顺序不同会触发循环等待死锁。
	// 此处主动锁 account 行（即使最终不需要恢复 available_balance），
	// 以统一锁顺序，消除死锁可能。
	accountTable := s.router.GetTableName("account", tableIndex)
	var acctForLock model.Account
	lockResult := tx.WithContext(ctx).Table(accountTable).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("account_no = ?", entry.AccountNo).
		Take(&acctForLock)
	if lockResult.Error != nil && !errors.Is(lockResult.Error, gorm.ErrRecordNotFound) {
		tx.Rollback()
		return fmt.Errorf("lock account %s: %w", entry.AccountNo, lockResult.Error)
	}
	// account 不存在时继续（空回滚保护依然有效，branch 也可能不存在）

	branch, err := s.tccRepo.GetBranchForUpdate(ctx, tx, branchID, dbIndex, tableIndex)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("get branch: %w", err)
	}
	// 空回滚保护：Try 未写入（如 Try 的 DB 连接在写入前失败），直接成功
	if branch == nil || branch.Status == model.TccStatusCancelled {
		return tx.Commit().Error
	}
	if branch.Status == model.TccStatusConfirmed {
		tx.Rollback()
		return fmt.Errorf("cannot cancel already confirmed branch %s", branchID)
	}

	// 恢复冻结的 available_balance（account 行已在上方锁定，无需重复查询）
	if branch.FrozenAmount > 0 {
		result := tx.WithContext(ctx).Table(accountTable).
			Where("account_no = ?", entry.AccountNo).
			Updates(map[string]interface{}{
				"available_balance": gorm.Expr("available_balance + ?", branch.FrozenAmount),
				"version":           gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			tx.Rollback()
			return fmt.Errorf("restore available_balance: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			// 账户不存在（账户锁步骤已返回 ErrRecordNotFound，此处不应触发；若触发说明数据不一致）
			s.logger.Error("CRITICAL: tcc cancel could not restore available_balance — account not found",
				zap.String("branchID", branchID),
				zap.String("accountNo", entry.AccountNo),
				zap.Int64("frozenAmount", branch.FrozenAmount),
			)
		}
	}

	if err := s.tccRepo.UpdateBranchStatus(ctx, tx, branchID, model.TccStatusCancelled, dbIndex, tableIndex); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

// cancelTried Try 失败时，并行 Cancel 所有已成功 Try 的分支
func (s *accountingService) cancelTried(ctx context.Context, tried []tccBranchMeta) {
	var wg sync.WaitGroup
	for _, b := range tried {
		wg.Add(1)
		go func(b tccBranchMeta) {
			defer wg.Done()
			if err := s.tccCancel(ctx, b.dbIndex, b.tableIndex, b.branchID, b.entry); err != nil {
				s.logger.Error("tcc cancel failed",
					zap.Error(err), zap.String("branchID", b.branchID), zap.String("accountNo", b.entry.AccountNo))
			}
		}(b)
	}
	wg.Wait()
}

// ─── 辅助计算 ─────────────────────────────────────────────────────────────────

// isAssetOrExpense 判断账户科目是否为资产/费用类
// 资产/费用：借方增加，贷方减少
// 负债/权益/收入：借方减少，贷方增加
func isAssetOrExpense(account *model.Account) bool {
	return account.AccountCategory == model.AccountCategoryAsset ||
		account.AccountCategory == model.AccountCategoryExpense
}

// computeBalanceDelta 计算该分录对账户余额的净增量（正=增加，负=减少）
// 返回值单位：ISO 最小货币单位 × 100
func computeBalanceDelta(entry AccountingEntry, assetOrExpense bool) int64 {
	if entry.DebitAmount != 0 {
		if assetOrExpense {
			return entry.DebitAmount // 借方：资产增加
		}
		return -entry.DebitAmount // 借方：负债/权益减少
	}
	if assetOrExpense {
		return -entry.CreditAmount // 贷方：资产减少
	}
	return entry.CreditAmount // 贷方：负债/权益增加
}

// ─── 账户管理 ─────────────────────────────────────────────────────────────────

// ID 预留 / 业务 ID 起始区间。对外 API 必须遵守此约束。
//
//	[1, ReservedOwnerIDMax]                       预留给平台 / 中间 / 手续费 / 权益等系统内部账户
//	[UserOwnerIDBase, UserOwnerIDMax]             普通用户账户 (base 1e8)
//	[MerchantOwnerIDBase, MerchantOwnerIDMax]     商户账户 (base 9e8)
//
// 这样保证不同用途账户的 owner_id 在数值空间上完全互不重叠，避免"撞到系统保留 id"
// 之类的误操作与幂等键混淆。调用方可以用业务内小数字 i 做 base+i 快速构造测试 id。
const (
	ReservedOwnerIDMax  int64 = 10_000              // 系统内部账户 owner_id 上限
	UserOwnerIDBase     int64 = 100_000_000         // 用户账户 owner_id 起点 (1e8)
	UserOwnerIDMax      int64 = 899_999_999         // 用户账户 owner_id 上限 (9e8 - 1)
	MerchantOwnerIDBase int64 = 900_000_000         // 商户账户 owner_id 起点 (9e8)
	MerchantOwnerIDMax  int64 = 9_999_999_999_999   // 商户账户 owner_id 上限（留足扩展空间）
)

// isPlatformAccountType 属于系统内部账户（owner_id 必须落在 [1, ReservedOwnerIDMax]）。
func isPlatformAccountType(t model.AccountType) bool {
	switch t {
	case model.AccountTypePlatform,
		model.AccountTypeTransitChannelReceivable,
		model.AccountTypeTransitChannelPayable,
		model.AccountTypeTransactionFee,
		model.AccountTypeChargeFee,
		model.AccountTypeTransit:
		return true
	}
	return false
}

// validateOwnerIDForType 按账户类型校验 owner_id 是否落在允许区间。
// 违反则立即返回错误，防止系统账户与真实用户账户互相串。
func validateOwnerIDForType(userID int64, accountType model.AccountType) error {
	if userID <= 0 {
		return fmt.Errorf("invalid owner_id: %d (must be > 0)", userID)
	}
	switch accountType {
	case model.AccountTypeUser:
		if userID < UserOwnerIDBase || userID > UserOwnerIDMax {
			return fmt.Errorf("user account owner_id %d out of range [%d, %d]; use CreatePlatformAccount for reserved ids",
				userID, UserOwnerIDBase, UserOwnerIDMax)
		}
	case model.AccountTypeMerchant, model.AccountTypeMerchantPendingSettle:
		if userID < MerchantOwnerIDBase || userID > MerchantOwnerIDMax {
			return fmt.Errorf("merchant account owner_id %d out of range [%d, %d]",
				userID, MerchantOwnerIDBase, MerchantOwnerIDMax)
		}
	default:
		// 其他类型（平台/中间/手续费）只能通过 CreatePlatformAccount 创建
		if !isPlatformAccountType(accountType) {
			return fmt.Errorf("unknown account type %d", accountType)
		}
		return fmt.Errorf("account type %d is platform-reserved; use CreatePlatformAccount", accountType)
	}
	return nil
}

// CreateAccount 创建业务账户（用户 / 商户 / 商户待结算）。
//
// 权威来源:account_business_type_info 表。流程:
//   1. 按 (account_business_type) 查 registry。
//      - 找到：要求请求中的 account_type + category 与 registry 完全一致，否则拒绝。
//        防止 client 用合法的 business_type 配错 account_type/category 造成账户方向不符。
//      - 未找到：拒绝（强制所有 business_type 必须先在 meta 表登记）。
//   2. 按 registry 确认后的 account_type 校验 owner_id 是否落在合法区间。
//   3. 通过以后进入 createAccountInternal 幂等创建。
//
// 平台/中间/手续费类型不能通过本方法创建（那是 CreatePlatformAccount / Fleet 的活），
// 因为 owner_id 落段不同，registry 的 account_type 本身会阻止这种误用。
func (s *accountingService) CreateAccount(ctx context.Context, userID int64, accountBusinessType model.AccountBusinessType, accountType model.AccountType, category model.AccountCategory, currency string) (*model.Account, error) {
	// ── ID layout 边界校验（与 payment-util/shadow.EncodeAccountID 的位段对齐）──
	// account_id 19 位 layout: shadow(1)+currency(3)+accountType(2)+globalTbl(2)+businessType(4)+seq(7)
	// accountType 限 0-99，businessType 限 0-9999。超出会让 Encode 失败 → 直接前置拒绝给业务侧更友好错误。
	if int64(accountType) > 99 {
		return nil, fmt.Errorf("account_type %d exceeds layout max 99; expand layout in payment-util/shadow/identity.go before adding new types", accountType)
	}
	if int64(accountBusinessType) > 9999 {
		return nil, fmt.Errorf("business_type %d exceeds layout max 9999; expand layout in payment-util/shadow/identity.go before registering new business types", accountBusinessType)
	}

	// ── 权威校验：从本地 registry 缓存查 business_type；miss 时兜底查一次 DB 并补 cache ──
	// 启动时 ReloadRegistry 已把全部行 load 进来，热路径 0 次 DB 往返。
	info := s.businessTypeLookup(accountBusinessType)
	if info == nil {
		if s.businessTypeRepo == nil {
			return nil, fmt.Errorf("business_type registry not wired; cannot validate account creation")
		}
		// Cache miss：可能是 admin 刚注册但 fan-out 还未到本实例，查一次 DB 补 cache。
		row, err := s.businessTypeRepo.GetByBusinessType(ctx, accountBusinessType)
		if err != nil {
			return nil, fmt.Errorf("lookup business_type %d: %w", accountBusinessType, err)
		}
		if row == nil {
			return nil, fmt.Errorf(
				"business_type %d is not registered in account_business_type_info; "+
					"register it via admin-web 业务类型 页面 first",
				accountBusinessType,
			)
		}
		s.registryMu.Lock()
		s.cachedBusinessTypes[accountBusinessType] = row
		s.registryMu.Unlock()
		info = row
	}
	if info.Enabled != 1 {
		return nil, fmt.Errorf("business_type %d (%s) is disabled", accountBusinessType, info.BusinessTypeCode)
	}
	if info.AccountType != accountType {
		return nil, fmt.Errorf(
			"account_type mismatch for business_type %d (%s): registry=%d request=%d",
			accountBusinessType, info.BusinessTypeCode, info.AccountType, accountType,
		)
	}
	// category 不存表，用 AccountType 派生；请求的 category 必须与派生结果一致
	expectedCategory, err := CategoryForAccountType(accountType)
	if err != nil {
		return nil, err
	}
	if expectedCategory != category {
		return nil, fmt.Errorf(
			"category mismatch for account_type %d: canonical=%s request=%s",
			accountType, expectedCategory, category,
		)
	}

	// ── owner_id 区间校验（按 registry 确认过的 account_type）──
	if err := validateOwnerIDForType(userID, accountType); err != nil {
		return nil, err
	}
	return s.createAccountInternal(ctx, userID, accountBusinessType, accountType, category, currency)
}

// CategoryForAccountType 按约定的 1:1 映射返回 AccountType 对应的 AccountCategory。
//
// 此函数是 category 的**唯一权威推导源**—— account_business_type_info 表不再存储
// category，admin-web / gRPC 需要展示 category 时从这里派生，保证一处改处处改。
//
//   User / Merchant / MerchantPendingSettle  → LIABILITY (业务上用户余额/商户结算都是"平台对其负债")
//   Platform                                 → EQUITY    (平台损益)
//   TransitChannelReceivable                 → ASSET     (渠道应收)
//   TransitChannelPayable                    → LIABILITY (渠道应付)
//   TransactionFee / ChargeFee               → REVENUE   (手续费/服务费收入)
//   Transit                                  → LIABILITY (平台中间账户)
//
// 未知 AccountType 返回空串 + error，调用方必须处理。
func CategoryForAccountType(at model.AccountType) (model.AccountCategory, error) {
	switch at {
	case model.AccountTypeUser, model.AccountTypeMerchant, model.AccountTypeMerchantPendingSettle:
		return model.AccountCategoryLiability, nil
	case model.AccountTypePlatform:
		return model.AccountCategoryEquity, nil
	case model.AccountTypeTransitChannelReceivable:
		return model.AccountCategoryAsset, nil
	case model.AccountTypeTransitChannelPayable, model.AccountTypeTransit:
		return model.AccountCategoryLiability, nil
	case model.AccountTypeTransactionFee, model.AccountTypeChargeFee:
		return model.AccountCategoryRevenue, nil
	}
	return "", fmt.Errorf("unknown AccountType %d (no category mapping)", at)
}

// platformAccountSpec 平台内部账户类型 → (business_type, category) 的 1:1 映射。
// 保持约定一致：同一 AccountType 在全系统各处都用同一个 business_type + category。
// 业务层只需要指定 AccountType 即可，杜绝"填错 category 造出方向不一致的账户"这类错误。
var platformAccountSpec = map[model.AccountType]struct {
	BusinessType model.AccountBusinessType
	Category     model.AccountCategory
}{
	model.AccountTypePlatform: {
		BusinessType: model.AccountBusinessTypePlatformProfitLoss, Category: model.AccountCategoryEquity,
	},
	model.AccountTypeTransitChannelReceivable: {
		BusinessType: model.AccountBusinessTypeTransitChannelReceivable, Category: model.AccountCategoryAsset,
	},
	model.AccountTypeTransitChannelPayable: {
		BusinessType: model.AccountBusinessTypeTransitChannelPayable, Category: model.AccountCategoryLiability,
	},
	model.AccountTypeTransactionFee: {
		BusinessType: model.AccountBusinessTypeTransactionFee, Category: model.AccountCategoryRevenue,
	},
	model.AccountTypeChargeFee: {
		BusinessType: model.AccountBusinessTypeChargeFee, Category: model.AccountCategoryRevenue,
	},
	model.AccountTypeTransit: {
		BusinessType: model.AccountBusinessTypeTransit, Category: model.AccountCategoryLiability,
	},
}

// CreatePlatformAccount 创建系统内部账户（平台 / 中间 / 手续费 / 权益 / 中转）。
//
// 调用方只需指定 reservedID（保留段 owner_id）+ accountType + currency：
// business_type / category 由 platformAccountSpec 确定性映射，避免人为填错方向。
//
// 与 CreateAccount 分开暴露:
//   - 调用方职责更清晰（业务 vs 系统）；
//   - 可以独立做 RBAC（例如只允许 admin-only token 调 CreatePlatformAccount）；
//   - 预防"用业务 CreateAccount 造假系统账户"。
func (s *accountingService) CreatePlatformAccount(ctx context.Context, reservedID int64, accountType model.AccountType, currency string) (*model.Account, error) {
	if reservedID <= 0 || reservedID > ReservedOwnerIDMax {
		return nil, fmt.Errorf("platform owner_id %d out of reserved range [1, %d]", reservedID, ReservedOwnerIDMax)
	}
	spec, ok := platformAccountSpec[accountType]
	if !ok {
		return nil, fmt.Errorf("account type %d is not a platform-internal type; use CreateAccount", accountType)
	}
	return s.createAccountInternal(ctx, reservedID, spec.BusinessType, accountType, spec.Category, currency)
}

// CreatePlatformChannelRequest Fleet 请求：基于**已登记**的 business_type 批量建 100 账户。
//
// 字段都是 int/string，无 enum 约束：
//   - AccountType         平台内部账户类型（4-9）；必须与 registry 中的 account_type 一致
//   - ChannelBusinessType 业务类型数字码；必须预先通过 RegisterBusinessType 登记
//   - Currency            默认 CNY
type CreatePlatformChannelRequest struct {
	AccountType         model.AccountType
	ChannelBusinessType int
	Currency            string
}

// CreatePlatformChannelResult 返回 fleet 创建结果：实际使用的 business_type 数字码 + 100 个账户。
type CreatePlatformChannelResult struct {
	ChannelBusinessType int
	BusinessTypeCode    string
	Accounts            []*model.Account
}

// CreatePlatformAccountFleet 基于**已存在**的 business_type 创建 100 个分片账户。
//
// 业务流程:
//   1. 运营先在"业务类型"页通过 RegisterBusinessType / POST /admin/business-types 登记一条
//      (business_type, code, account_type) —— 这一步只写 meta 表；
//   2. 再到"系统账户"页的 Fleet 面板选中刚登记的 business_type，触发本方法创建 100 账户。
//
// 本方法不再承担"注册"职责：要求 req.ChannelBusinessType 必须已经存在于 registry,
// 且 req.AccountType 与 registry 一致；否则拒绝。category 由 CategoryForAccountType
// 派生。幂等：重复调用只补齐缺失的分片账户。
func (s *accountingService) CreatePlatformAccountFleet(ctx context.Context, req *CreatePlatformChannelRequest) (*CreatePlatformChannelResult, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	if !isPlatformAccountType(req.AccountType) {
		return nil, fmt.Errorf("account type %d is not a platform-internal type", req.AccountType)
	}
	if req.ChannelBusinessType < 1 || req.ChannelBusinessType > 999 {
		return nil, fmt.Errorf("channel_business_type %d out of range [1, 999] (must be pre-registered)", req.ChannelBusinessType)
	}
	if s.businessTypeRepo == nil {
		return nil, fmt.Errorf("business_type registry not wired")
	}

	// 要求已在 registry 登记
	info, err := s.businessTypeRepo.GetByBusinessType(ctx, model.AccountBusinessType(req.ChannelBusinessType))
	if err != nil {
		return nil, fmt.Errorf("lookup business_type %d: %w", req.ChannelBusinessType, err)
	}
	if info == nil {
		return nil, fmt.Errorf(
			"business_type %d not registered; register it on 业务类型 page (POST /admin/business-types) first",
			req.ChannelBusinessType,
		)
	}
	if info.Enabled != 1 {
		return nil, fmt.Errorf("business_type %d (%s) is disabled", req.ChannelBusinessType, info.BusinessTypeCode)
	}
	if info.AccountType != req.AccountType {
		return nil, fmt.Errorf(
			"account_type mismatch for business_type %d: registry=%d request=%d",
			req.ChannelBusinessType, info.AccountType, req.AccountType,
		)
	}
	category, err := CategoryForAccountType(req.AccountType)
	if err != nil {
		return nil, err
	}

	currency := req.Currency
	if currency == "" {
		currency = "PHP"
	}
	accounts := make([]*model.Account, 0, 100)
	var firstErr error
	for i := int64(0); i < 100; i++ {
		acc, err := s.createAccountInternal(ctx, i, model.AccountBusinessType(req.ChannelBusinessType), req.AccountType, category, currency)
		if err != nil {
			s.logger.Warn("CreatePlatformAccountFleet: shard failed, continuing",
				zap.Int64("userID", i), zap.Int("businessType", req.ChannelBusinessType),
				zap.Int("accountType", int(req.AccountType)), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		accounts = append(accounts, acc)
	}
	s.logger.Info("CreatePlatformAccountFleet completed",
		zap.String("code", info.BusinessTypeCode),
		zap.Int("channelBusinessType", req.ChannelBusinessType),
		zap.Int("accountType", int(req.AccountType)),
		zap.Int("created", len(accounts)),
		zap.Bool("partial", firstErr != nil),
	)
	if firstErr != nil && len(accounts) == 0 {
		return nil, firstErr
	}
	return &CreatePlatformChannelResult{
		ChannelBusinessType: req.ChannelBusinessType,
		BusinessTypeCode:    info.BusinessTypeCode,
		Accounts:            accounts,
	}, nil
}

// ListBusinessTypes 返回本地缓存（由 ReloadRegistry 定期刷新或 admin fan-out 立即刷新）。
// 首次启动前 cache 空，退回到 DB 直读，避免还没 load 完就调 admin list 页。
func (s *accountingService) ListBusinessTypes(ctx context.Context) ([]*model.AccountBusinessTypeInfo, error) {
	s.registryMu.RLock()
	if len(s.cachedBusinessTypes) > 0 {
		out := make([]*model.AccountBusinessTypeInfo, 0, len(s.cachedBusinessTypes))
		for _, v := range s.cachedBusinessTypes {
			out = append(out, v)
		}
		s.registryMu.RUnlock()
		// 排序由前端做（或未来加个 sort.Slice）
		return out, nil
	}
	s.registryMu.RUnlock()
	if s.businessTypeRepo == nil {
		return nil, fmt.Errorf("businessTypeRepo not wired")
	}
	return s.businessTypeRepo.List(ctx)
}

// ReloadRegistry 从 meta DB 重新加载 account_business_type_info 和 account_type_info 全表
// 到本地缓存。
//   - 启动时调一次（由 main.go 的 fx.OnStart 或 ConfigSyncWorker 首次 tick 触发）
//   - 增加 business_type 后，admin-web backend 扇出 POST /admin/reload/business-types
//     到所有存活实例，每个实例调这里重新拉取。
// 失败时保留旧缓存（降级 graceful），仅记 warn log；不向上传播。
func (s *accountingService) ReloadRegistry(ctx context.Context) error {
	if s.businessTypeRepo == nil || s.ruleRepo == nil {
		return fmt.Errorf("registry repos not wired")
	}
	bts, err := s.businessTypeRepo.List(ctx)
	if err != nil {
		return fmt.Errorf("reload business_types: %w", err)
	}
	ats, err := s.ruleRepo.ListAccountTypes(ctx)
	if err != nil {
		return fmt.Errorf("reload account_types: %w", err)
	}
	newBT := make(map[model.AccountBusinessType]*model.AccountBusinessTypeInfo, len(bts))
	for _, x := range bts {
		newBT[x.BusinessType] = x
	}
	newAT := make(map[model.AccountType]*model.AccountTypeInfo, len(ats))
	for _, x := range ats {
		newAT[model.AccountType(x.OwnerType)] = x
	}
	s.registryMu.Lock()
	s.cachedBusinessTypes = newBT
	s.cachedAccountTypes = newAT
	s.registryMu.Unlock()
	s.logger.Info("registry reloaded",
		zap.Int("businessTypes", len(newBT)),
		zap.Int("accountTypes", len(newAT)))
	return nil
}

// RetryStuckConfirmingTcc 见接口注释。修复 CONFIRMING 半挂起。
//
// 算法步骤详细说明：
//
//  1. 列出所有 phase∈{TRYING, CONFIRMING} 且 updated_at < before 的 coordinator
//     （ListStuck 既返回 TRYING 也返回 CONFIRMING；这里只处理 CONFIRMING）
//
//  2. 对每个 CONFIRMING coordinator：
//     a) 跨所有分片调 ListBranchesByTccID 列出该 tccID 的所有 branch
//     b) 找出 status=TRYING 的 branch（status=CONFIRMED 的跳过）
//     c) 对每个 TRYING branch：开 tx → 锁账户 → 调 tccConfirm(deltaKnown=false)
//        tccConfirm 内部从 DB 读 branch.BalanceDelta + 状态幂等检查
//     d) 全部 branch CONFIRMED 后 → TransitionToConfirmed
//
//  3. 跳过条件（不碰）：
//     - 所有 branch 都已 CANCELLED（理论上不可能：CONFIRMING 阶段不会有 branch CANCELLED）
//     - 任一 branch tccConfirm 失败 → 该 coordinator 跳过到下次 tick
func (s *accountingService) RetryStuckConfirmingTcc(ctx context.Context, before time.Time) (int, error) {
	if s.tccCoordRepo == nil {
		return 0, nil // 旧部署兼容：未启用 coordinator
	}
	stuck, err := s.tccCoordRepo.ListStuck(ctx, before, 200)
	if err != nil {
		return 0, fmt.Errorf("list stuck coordinators: %w", err)
	}
	confirmedCount := 0
	for _, coord := range stuck {
		if coord.Phase != model.TccPhaseConfirming {
			continue // TRYING 由 TccRecoveryWorker 负责 cancel
		}
		if err := s.recoverOneConfirmingTcc(ctx, coord); err != nil {
			s.logger.Error("retry confirming: per-tcc recovery failed",
				zap.String("tccID", coord.TccID),
				zap.String("businessNo", coord.BusinessNo),
				zap.Error(err))
			continue
		}
		confirmedCount++
	}
	if confirmedCount > 0 {
		s.logger.Warn("retry confirming: recovered stuck CONFIRMING tccs",
			zap.Int("count", confirmedCount),
			zap.Int("scanned", len(stuck)))
	}
	return confirmedCount, nil
}

// recoverOneConfirmingTcc 处理单个 CONFIRMING 协调者：列出所有 branch，对 TRYING 的逐个补 confirm。
func (s *accountingService) recoverOneConfirmingTcc(ctx context.Context, coord *model.TccCoordinator) error {
	tccID := coord.TccID
	// 跨所有分片找该 tccID 的 branches
	var allBranches []*model.TccTransaction
	var branchTableIdx []int
	var branchDBIdx []int
	for _, shard := range s.router.GetAllShards() {
		db, err := s.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return fmt.Errorf("get db[%d]: %w", shard.DBIndex, err)
		}
		branches, err := s.tccRepo.ListBranchesByTccID(ctx, db, shard.TableIndex, tccID)
		if err != nil {
			return fmt.Errorf("list branches db[%d] tbl[%d]: %w", shard.DBIndex, shard.TableIndex, err)
		}
		for range branches {
			branchTableIdx = append(branchTableIdx, shard.TableIndex)
			branchDBIdx = append(branchDBIdx, shard.DBIndex)
		}
		allBranches = append(allBranches, branches...)
	}
	if len(allBranches) == 0 {
		// 无任何 branch — 清理掉协调者（防止反复扫到）
		s.logger.Warn("retry confirming: no branches found for CONFIRMING coordinator, marking confirmed",
			zap.String("tccID", tccID))
		_ = s.tccCoordRepo.TransitionToConfirmed(ctx, tccID)
		return nil
	}

	// 逐个 branch 补 confirm（同分片内可以一次 tx，但跨分片必须分开）
	for i, branch := range allBranches {
		if branch.Status == model.TccStatusConfirmed {
			continue // 已确认，跳过
		}
		if branch.Status == model.TccStatusCancelled {
			// CONFIRMING 阶段出现 CANCELLED 是状态机违规 — 严重，告警让人介入
			s.logger.Error("retry confirming: CANCELLED branch found in CONFIRMING coordinator (state machine violation)",
				zap.String("tccID", tccID),
				zap.String("branchID", branch.BranchID))
			return fmt.Errorf("branch %s is CANCELLED but coordinator is CONFIRMING — manual intervention required", branch.BranchID)
		}
		// status = TRYING — 补 confirm
		if err := s.confirmOneBranchFromRecovery(ctx, branch, branchDBIdx[i], branchTableIdx[i], coord); err != nil {
			return fmt.Errorf("confirm branch %s: %w", branch.BranchID, err)
		}
	}

	// 所有 branch 现已 CONFIRMED → 协调者 transition。
	// cut_date 在原始 booking 入口就写入了 coord + 所有 entry，无需 fan-out。
	if cErr := s.tccCoordRepo.TransitionToConfirmed(ctx, tccID); cErr != nil {
		s.logger.Warn("retry confirming: TransitionToConfirmed failed (non-fatal, branches OK)",
			zap.String("tccID", tccID), zap.Error(cErr))
	}
	return nil
}

// confirmOneBranchFromRecovery 单 branch 补 confirm：
//   - 读 account（GetAccountForUpdate 行锁）
//   - 从 branch.BalanceDelta + account.AccountCategory 反推 entry.DebitAmount/CreditAmount
//   - 调 tccConfirm(deltaKnown=false) — 让它从 DB 重新校验 branch 状态 + 幂等
func (s *accountingService) confirmOneBranchFromRecovery(ctx context.Context, branch *model.TccTransaction, dbIndex, tableIndex int, coord *model.TccCoordinator) error {
	db, err := s.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("get db: %w", err)
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		account, err := s.accountRepo.GetAccountForUpdate(ctx, tx, branch.AccountNo, dbIndex, tableIndex)
		if err != nil {
			return fmt.Errorf("get account %s: %w", branch.AccountNo, err)
		}
		if account == nil {
			return fmt.Errorf("account %s not found", branch.AccountNo)
		}
		// 从 BalanceDelta 反推 debit/credit：
		//  - 资产/费用：positive delta = debit (借方增加，账户变多)
		//  - 负债/权益/收入：positive delta = credit (贷方增加，账户变多)
		amount := branch.BalanceDelta
		if amount < 0 {
			amount = -amount
		}
		var entry AccountingEntry
		entry.AccountNo = branch.AccountNo
		isDebitPositive := commonutil.IsAssetOrExpense(account.AccountCategory)
		isPositiveDelta := branch.BalanceDelta > 0
		if isDebitPositive == isPositiveDelta {
			entry.DebitAmount = amount
		} else {
			entry.CreditAmount = amount
		}
		// transactionDate / transactionTime: 复用 coord.created_at 作为业务日期
		// （生产可考虑给 tcc_coordinator 加 transaction_date 列；此处取近似不影响余额对账）
		// **资金安全关键**：currency 必须从 coord 读，**绝不 hardcode**。
		// 如果 coord.Currency 为空（旧代码创建的数据），fail-loud 拒绝 recovery
		// 而不是用某个 hardcode 默认值（之前 "CNY" 默认导致与原 booking 币种
		// 不一致 → trial balance 按 currency 过滤单边 → 不平）。
		// 旧数据需 ops 手动写入 coord.currency 后才能恢复，避免静默写错币种。
		if coord.Currency == "" {
			s.logger.Error("CRITICAL: tcc recovery refused — coord.currency empty (legacy data, no source of truth for currency)",
				zap.String("tccID", branch.TccID),
				zap.String("businessNo", coord.BusinessNo),
				zap.String("branchID", branch.BranchID))
			return fmt.Errorf("tcc recovery: coord %s has empty currency — refuse to write recovery entry with unknown currency (manual fix required: UPDATE tcc_coordinator SET currency=? WHERE tcc_id=?)", branch.TccID)
		}
		params := &bookingParams{
			transactionID:   branch.BranchID,
			voucherNo:       branch.TccID,
			businessNo:      coord.BusinessNo,
			businessType:    model.BusinessType("RECOVERY"), // 标记本次流水为恢复回填；生产可让 coord 加列保留原 business_type
			entry:           entry,
			currency:        coord.Currency, // 直接读 coord 持久化值，与原 booking 一致
			transactionDate: coord.CreatedAt.Format("2006-01-02"),
			transactionTime: time.Now(),
			description:     "TCC confirm recovery",
			// cut_date 从 coord 读，与原 booking 入口一致 → 试算平衡仍成立
			cutDate:         coord.CutDate,
		}
		// deltaKnown=false → tccConfirm 内部从 branch 读 BalanceDelta + 幂等检查
		if cfmErr := s.tccConfirm(ctx, tx, branch.BranchID, entry, account, params, dbIndex, tableIndex, 0, false); cfmErr != nil {
			return fmt.Errorf("tccConfirm: %w", cfmErr)
		}
		return nil
	})
}

// businessTypeLookup 快照式读本地缓存，miss 则返回 nil。
func (s *accountingService) businessTypeLookup(bt model.AccountBusinessType) *model.AccountBusinessTypeInfo {
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	return s.cachedBusinessTypes[bt]
}


// RegisterBusinessType 见接口注释。只写 registry，不建账户。
// 如需在注册后基于此 business_type 创建 100 分片账户，请另调 CreatePlatformAccountFleet。
//
// 幂等性：相同 business_type_code 重复调用，返回首次注册时的记录（含原 business_type
// id），不会再分配新 id。这与 admin-web 重启 / 多实例同时 onboard 的语义对齐。
func (s *accountingService) RegisterBusinessType(ctx context.Context, accountType model.AccountType, businessTypeCode, description string, businessType int) (*model.AccountBusinessTypeInfo, error) {
	if s.businessTypeRepo == nil {
		return nil, fmt.Errorf("businessTypeRepo not wired")
	}
	if businessTypeCode == "" {
		return nil, fmt.Errorf("business_type_code required")
	}
	if businessType != 0 && (businessType < 1 || businessType > 999) {
		return nil, fmt.Errorf("business_type %d out of range [1, 999]", businessType)
	}
	// 系统内部账户类型 (4-9) 和业务账户类型 (1-3) 都允许，由调用方选择合适的映射。
	if _, err := CategoryForAccountType(accountType); err != nil {
		return nil, err
	}

	// 幂等：先按 code 查现有，命中直接返回（保留首注册的 business_type id）。
	// 否则继续走自动分配 + Register 路径。
	if existing, err := s.businessTypeRepo.GetByCode(ctx, businessTypeCode); err == nil && existing != nil {
		if accountType != 0 && existing.AccountType != accountType {
			return nil, fmt.Errorf("%w: code=%s existing_account_type=%d new_account_type=%d",
				repository.ErrBusinessTypeConflict, businessTypeCode, existing.AccountType, accountType)
		}
		return existing, nil
	}

	// 数字码自动分配（仅当调用方传 0 时）
	bt := businessType
	if bt == 0 {
		assigned, err := s.allocateNextChannelBusinessType(ctx)
		if err != nil {
			return nil, fmt.Errorf("auto-allocate business_type: %w", err)
		}
		bt = assigned
	}

	var desc *string
	if description != "" {
		d := description
		desc = &d
	}
	info, err := s.businessTypeRepo.Register(ctx, &model.AccountBusinessTypeInfo{
		BusinessType:     model.AccountBusinessType(bt),
		BusinessTypeCode: businessTypeCode,
		AccountType:      accountType,
		Description:      desc,
		Enabled:          1,
	})
	if err != nil {
		return nil, err
	}
	// 本实例 cache 立即生效；其他实例由 admin fan-out /admin/reload/business-types 更新。
	s.registryMu.Lock()
	s.cachedBusinessTypes[info.BusinessType] = info
	s.registryMu.Unlock()
	s.logger.Info("RegisterBusinessType ok",
		zap.Int("businessType", bt),
		zap.String("code", businessTypeCode),
		zap.Int("accountType", int(accountType)),
	)
	return info, nil
}

// PlatformAccountSnapshot 一个平台账户 + 其快照（可能为 nil）的组合视图。
type PlatformAccountSnapshot struct {
	Account  *model.Account                 `json:"account"`
	Snapshot *model.AccountBalanceSnapshot  `json:"snapshot,omitempty"`
}

// platformAccountsCacheTTL 短 TTL：admin-web 多 tab 同时刷新场景下，2 秒内
// 重复请求直接命中本地内存，避开 100 路 fan-out + 100 次 buffer overlay。
// 选 2s 是因为：
//   - 一个 fleet 的 100 个账户 + buffer overlay 总耗时大概 200~400ms
//   - 用户在 admin 页面手动 / 自动刷新最快也要 1s 间隔
//   - 2s 内的"过期"对运维场景可以接受（这是观察性数据，不是资金决策）
const platformAccountsCacheTTL = 2 * time.Second

type platformAccountsCacheEntry struct {
	accounts []*model.Account
	expires  time.Time
}

// ListPlatformAccountsByBusinessType 并发拉取 user_id 0..99 对应的 100 个分片账户。
// 不存在的 user_id 静默跳过（返回 nil），不影响其他；按 user_id 升序返回。
//
// 读取时叠加 account_balance_buffer.pending_delta，使平台 fleet 账户在 buffer flush
// 周期（30s + jitter）之前也能立即反映最新记账结果。
//
// 短 TTL 缓存：连续 2 秒内同一 business_type 的请求复用上一次结果，避免 admin-web
// 多 tab / 自动刷新打穿到 100 个 shard 查询。资金决策不读这个接口（只是观察性
// 端点），过期 2s 可接受。
func (s *accountingService) ListPlatformAccountsByBusinessType(ctx context.Context, businessType model.AccountBusinessType, currency string) ([]*model.Account, error) {
	if currency == "" {
		return nil, fmt.Errorf("ListPlatformAccountsByBusinessType: currency is required")
	}
	// cache key 加入 currency：同 business_type 多币种各自独立缓存。
	cacheKey := fmt.Sprintf("%d:%s", int(businessType), currency)
	if v, ok := s.platformAccountsCache.Load(cacheKey); ok {
		entry := v.(*platformAccountsCacheEntry)
		if time.Now().Before(entry.expires) {
			return entry.accounts, nil
		}
	}

	type slot struct {
		acc *model.Account
		err error
	}
	slots := make([]slot, 100)
	sem := make(chan struct{}, 20) // 限制 20 路并发，避免连接池爆
	var wg sync.WaitGroup
	for i := int64(0); i < 100; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// 每个 user_id slot 可能存在多币种账户：按 (user_id, bt, currency) 精确拿。
			accs, err := s.accountRepo.ListAccountsByUserAndBusinessType(ctx, i, businessType, currency)
			var acc *model.Account
			if err == nil && len(accs) > 0 {
				acc = accs[0]
				s.overlayPendingDelta(ctx, acc)
			}
			slots[i] = slot{acc: acc, err: err}
		}()
	}
	wg.Wait()

	out := make([]*model.Account, 0, 100)
	for _, sl := range slots {
		if sl.err != nil {
			s.logger.Warn("ListPlatformAccountsByBusinessType: shard query failed",
				zap.Int("businessType", int(businessType)), zap.String("currency", currency), zap.Error(sl.err))
			continue
		}
		if sl.acc != nil {
			out = append(out, sl.acc)
		}
	}

	s.platformAccountsCache.Store(cacheKey, &platformAccountsCacheEntry{
		accounts: out,
		expires:  time.Now().Add(platformAccountsCacheTTL),
	})
	return out, nil
}

// ListPlatformSnapshotsByBusinessType 拿账户 + 每个账户在 cutDate 的快照。
// 快照找不到返回 nil snapshot（前端展示"当日无活动"即可）。
func (s *accountingService) ListPlatformSnapshotsByBusinessType(ctx context.Context, businessType model.AccountBusinessType, cutDate, currency string) ([]*PlatformAccountSnapshot, error) {
	if cutDate == "" {
		return nil, fmt.Errorf("cutDate is required")
	}
	if currency == "" {
		return nil, fmt.Errorf("currency is required")
	}
	accounts, err := s.ListPlatformAccountsByBusinessType(ctx, businessType, currency)
	if err != nil {
		return nil, err
	}
	// 并发拉取每个账户的快照（跨分片，限制 20 路）
	type pair struct {
		acc  *model.Account
		snap *model.AccountBalanceSnapshot
		err  error
	}
	results := make([]pair, len(accounts))
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup
	for i, acc := range accounts {
		i, acc := i, acc
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			snap, err := s.GetBalanceSnapshot(ctx, acc.AccountNo, cutDate)
			results[i] = pair{acc: acc, snap: snap, err: err}
		}()
	}
	wg.Wait()

	out := make([]*PlatformAccountSnapshot, 0, len(results))
	for _, r := range results {
		// err 时 snap=nil，仍然返回账户本身
		if r.err != nil {
			s.logger.Debug("ListPlatformSnapshotsByBusinessType: snapshot fetch failed",
				zap.String("accountNo", r.acc.AccountNo), zap.Error(r.err))
		}
		out = append(out, &PlatformAccountSnapshot{Account: r.acc, Snapshot: r.snap})
	}
	return out, nil
}

// allocateNextChannelBusinessType 扫描 account_business_type_info，找到 [101, 999] 中第一个
// 未被占用的数字码。运营场景下每次只调一次，无并发风险；即便偶发并发，Register 的
// unique key (business_type) 会报冲突，调用方重试即可。
func (s *accountingService) allocateNextChannelBusinessType(ctx context.Context) (int, error) {
	if s.businessTypeRepo == nil {
		return 0, fmt.Errorf("businessTypeRepo not wired; cannot auto-allocate")
	}
	all, err := s.businessTypeRepo.List(ctx)
	if err != nil {
		return 0, err
	}
	used := make(map[int]struct{}, len(all))
	for _, x := range all {
		used[int(x.BusinessType)] = struct{}{}
	}
	for code := 101; code <= 999; code++ {
		if _, ok := used[code]; !ok {
			return code, nil
		}
	}
	return 0, fmt.Errorf("no free channel_business_type in [101, 999]")
}

// createAccountInternal 去掉 owner_id 校验的共享创建逻辑（幂等 + 写库）。
func (s *accountingService) createAccountInternal(ctx context.Context, userID int64, accountBusinessType model.AccountBusinessType, accountType model.AccountType, category model.AccountCategory, currency string) (*model.Account, error) {
	// 幂等性按 (user_id, business_type, currency) 三元组判断 —— 这也是 account 表
	// uk_user_business_type 的实际唯一键。早期实现只按 (user_id, business_type)
	// 查重，导致"同一用户改个币种再建"被误判成已存在，直接把旧币种账户返回给
	// 调用方（排查案例：传 USD 却拿到 PHP）。
	existing, err := s.accountRepo.ListAccountsByUserAndBusinessType(ctx, userID, accountBusinessType, currency)
	if err != nil {
		return nil, fmt.Errorf("check account existence: %w", err)
	}
	if len(existing) > 0 {
		return existing[0], nil
	}

	accountNo, err := s.generateAccountNo(ctx, userID, accountType, accountBusinessType, currency)
	if err != nil {
		return nil, fmt.Errorf("generateAccountNo: %w", err)
	}
	account := &model.Account{
		AccountNo:           accountNo,
		UserID:              userID,
		AccountType:         accountType,
		AccountCategory:     category,
		AccountBusinessType: accountBusinessType,
		Currency:            currency,
		Balance:             0,
		FrozenBalance:       0,
		AvailableBalance:    0,
		Status:              model.AccountStatusActive,
		Version:             0,
	}
	if err := s.accountRepo.CreateAccount(ctx, account); err != nil {
		return nil, fmt.Errorf("create account: %w", err)
	}
	return account, nil
}

// GetAccount 根据账户号查询账户
func (s *accountingService) GetAccount(ctx context.Context, accountNo string) (*model.Account, error) {
	acc, err := s.accountRepo.GetAccountByNo(ctx, accountNo)
	if err != nil || acc == nil {
		return acc, err
	}
	s.overlayPendingDelta(ctx, acc)
	return acc, nil
}

// GetAccountByUserAndBusinessType 根据 userId + businessType 查询账户
func (s *accountingService) GetAccountByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType) (*model.Account, error) {
	acc, err := s.accountRepo.GetAccountByUserAndBusinessType(ctx, userID, businessType)
	if err != nil || acc == nil {
		return acc, err
	}
	s.overlayPendingDelta(ctx, acc)
	return acc, nil
}

// ListAccountsByUserAndBusinessType 列出该 (userID, businessType) 下全部币种账户。
func (s *accountingService) ListAccountsByUserAndBusinessType(ctx context.Context, userID int64, businessType model.AccountBusinessType, currency string) ([]*model.Account, error) {
	accounts, err := s.accountRepo.ListAccountsByUserAndBusinessType(ctx, userID, businessType, currency)
	if err != nil {
		return nil, err
	}
	for _, acc := range accounts {
		s.overlayPendingDelta(ctx, acc)
	}
	return accounts, nil
}

// overlayPendingDelta 把 account_balance_buffer 未 flush 的增量叠加到读取到的账户上。
// 仅影响返回给调用方的结构体，不回写 DB。
//
// 只为启用缓冲记账的账户做 buffer 表查询，判定标准和写路径
// （HybridDoubleEntryBooking 里的 useBuffer）保持一致：
//   - 可负值账户（平台 / 中间账户，AccountType ≥ 4）默认走 buffer；
//   - 或显式配置在 buffer_account_config 里的账户（可覆盖用户/商户账户，罕见）。
// 对默认的用户/商户账户，不再多一次 buffer 表 SELECT。
func (s *accountingService) overlayPendingDelta(ctx context.Context, acc *model.Account) {
	if acc == nil || s.bufferRepo == nil || s.router == nil {
		return
	}
	if !commonutil.IsAccountCanNegative(acc.AccountType) && !s.isBufferedAccount(acc.AccountNo) {
		return
	}
	dbIdx, tableIdx := s.router.RouteByAccountNo(acc.AccountNo)
	delta, err := s.bufferRepo.GetPendingDelta(ctx, dbIdx, tableIdx, acc.AccountNo)
	if err != nil || delta == 0 {
		return
	}
	acc.Balance += delta
	acc.AvailableBalance += delta
}

// GetBalanceSnapshot 查询余额快照
func (s *accountingService) GetBalanceSnapshot(ctx context.Context, accountNo string, date string) (*model.AccountBalanceSnapshot, error) {
	dbIndex, tableIndex := s.router.RouteByAccountNo(accountNo)
	tableName := s.router.GetTableName("account_balance_snapshot", tableIndex)
	db, err := s.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, err
	}
	var snap model.AccountBalanceSnapshot
	query := db.WithContext(ctx).Table(tableName).Where("account_no = ?", accountNo)
	if date != "" {
		query = query.Where("snapshot_date = ?", date)
	} else {
		query = query.Order("snapshot_date DESC")
	}
	if err := query.First(&snap).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("query balance snapshot: %w", err)
	}
	return &snap, nil
}

// ─── ID 生成 ──────────────────────────────────────────────────────────────────

// generateVoucherNo 生成凭证号，按位编码 layout（payment-util/shadow.EncodeID）：
//   shadow(1) | idType(3) | globalTbl(2) | seq(13)
// idType=IDTypeVoucher (001)。globalTbl 来自 businessNo 路由，保证凭证与业务订单同片。
// seq 由号段 idgen 提供，严格单调递增不依赖时钟。
func (s *accountingService) generateVoucherNo(ctx context.Context, businessNo string) (string, error) {
	_, globalTblIdx := s.router.RouteByNumericStr(businessNo)
	seq, err := s.idGen.NextID(ctx, idgen.BizTagVoucher)
	if err != nil {
		return "", fmt.Errorf("generateVoucherNo: %w", err)
	}
	id, err := shadow.EncodeIDStr(ctx, shadow.IDTypeVoucher, globalTblIdx, seq)
	if err != nil {
		return "", fmt.Errorf("generateVoucherNo encode: %w", err)
	}
	return id, nil
}

// generateTransactionID 生成流水号，按位编码 layout：
//   shadow(1) | idType(3) | globalTbl(2) | seq(13)
// idType=IDTypeTransaction (002)。globalTbl 来自 accountNo 路由，保证流水与账户同片。
func (s *accountingService) generateTransactionID(ctx context.Context, accountNo string) (string, error) {
	_, globalTblIdx := s.router.RouteByAccountNo(accountNo)
	seq, err := s.idGen.NextID(ctx, idgen.BizTagTransaction)
	if err != nil {
		return "", fmt.Errorf("generateTransactionID: %w", err)
	}
	id, err := shadow.EncodeIDStr(ctx, shadow.IDTypeTransaction, globalTblIdx, seq)
	if err != nil {
		return "", fmt.Errorf("generateTransactionID encode: %w", err)
	}
	return id, nil
}

// generateAccountNo 按位编码生成账户号（19 位 int64 的十进制字符串形式）。
// layout（payment-util/shadow.EncodeAccountID）：
//   shadow(1) | currency(3) | accountType(2) | globalTbl(2) | businessType(4) | seq(7)
//
// - currency 走 ISO 4217 数字码（"PHP"→608），通过 payment-util/money 转换
// - globalTbl 从 user_id 路由出来，保证账户与用户同分片
// - seq 由 BizTagAccount 号段提供
//
// 同一 (currency, accountType, globalTbl, businessType) 组合最多 1000 万账户（seq 7 位）；
// 跨 (currency, accountType, businessType) 不抢 seq 段位 → 即使热门 PHP USER 段消耗 1000 万，
// USD MERCHANT 仍然干净。
//
// 唯一性：layout 自身保证同段位组合下 seq 单调递增 → uk_account_no 永远不撞。
func (s *accountingService) generateAccountNo(ctx context.Context, userID int64, accountType model.AccountType, businessType model.AccountBusinessType, currencyCode string) (string, error) {
	_, globalTblIdx := s.router.RouteByUserID(userID)
	currencyNum, err := money.NumericCode(currencyCode)
	if err != nil {
		return "", fmt.Errorf("generateAccountNo: %w", err)
	}
	seq, err := s.idGen.NextID(ctx, idgen.BizTagAccount)
	if err != nil {
		return "", fmt.Errorf("generateAccountNo idgen: %w", err)
	}
	id, err := shadow.EncodeAccountID(ctx, currencyNum, int(accountType), globalTblIdx, int(businessType), seq)
	if err != nil {
		return "", fmt.Errorf("generateAccountNo encode: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// ─── 校验 ─────────────────────────────────────────────────────────────────────

// MaxEntryAmount 单条分录金额上限（ISO minor unit × 100）。
// 1e15 ≈ 10 万亿 minor units,折合 1000 亿 CNY；真实业务永远不会触及。
// 设此上限是为了给聚合 (totalDebit / totalCredit 合计，全分片汇总)
// 留 9×1e3 的安全余量，防止极端输入触发 int64 溢出。
const MaxEntryAmount int64 = 1_000_000_000_000_000

func (s *accountingService) validateEntries(entries []AccountingEntry) error {
	if len(entries) < 2 {
		return fmt.Errorf("at least 2 entries required for double entry booking")
	}
	var totalDebit, totalCredit int64
	// 同一笔 booking 内同一账户只允许出现一次。
	// 历史教训：多分录在同一账户上 borrow + lend 不仅会触发 TCC Try 的同账号自死锁
	// （SELECT FOR UPDATE 同一行两次），即便能跑下来，事后对账也极易把方向拼错；
	// 业务侧应在外层先聚合到一条 entry 而不是塞两条。
	seenAccounts := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.AccountNo == "" {
			return fmt.Errorf("entry account_no is required")
		}
		if _, dup := seenAccounts[entry.AccountNo]; dup {
			return fmt.Errorf("duplicate account_no in single booking: %s (aggregate to one entry before submitting)", entry.AccountNo)
		}
		seenAccounts[entry.AccountNo] = struct{}{}

		if entry.DebitAmount < 0 || entry.CreditAmount < 0 {
			return fmt.Errorf("entry amount must be non-negative: debit=%d credit=%d", entry.DebitAmount, entry.CreditAmount)
		}
		if entry.DebitAmount > MaxEntryAmount || entry.CreditAmount > MaxEntryAmount {
			return fmt.Errorf("entry amount exceeds MaxEntryAmount(%d): debit=%d credit=%d",
				MaxEntryAmount, entry.DebitAmount, entry.CreditAmount)
		}
		if entry.DebitAmount != 0 && entry.CreditAmount != 0 {
			return fmt.Errorf("entry cannot have both debit and credit")
		}
		if entry.DebitAmount == 0 && entry.CreditAmount == 0 {
			return fmt.Errorf("entry must have debit or credit amount")
		}
		// 检查累加溢出：int64 max ≈ 9.2e18，单条 ≤ 1e15，理论上 9200 条才会溢出；
		// 这里仍按字面意义检查，保持防御性。
		if totalDebit > math.MaxInt64-entry.DebitAmount || totalCredit > math.MaxInt64-entry.CreditAmount {
			return fmt.Errorf("booking totals overflow int64 (too many entries or too large amounts)")
		}
		totalDebit += entry.DebitAmount
		totalCredit += entry.CreditAmount
	}
	if totalDebit != totalCredit {
		return fmt.Errorf("debit and credit must be equal: debit=%d, credit=%d", totalDebit, totalCredit)
	}
	return nil
}

// validateCurrency 验证请求币种是否受支持
func validateCurrency(curr string) error {
	if !currency.IsSupported(curr) {
		return fmt.Errorf("unsupported currency: %s", curr)
	}
	return nil
}

// tccBranchMeta 记录已完成 Try 的分支信息，用于 Cancel 回滚
type tccBranchMeta struct {
	dbIndex, tableIndex int
	branchID            string
	entry               AccountingEntry
}

// ─── bookingParams（Confirm 阶段流水记录用）────────────────────────────────────

type bookingParams struct {
	transactionID   string
	voucherNo       string
	businessNo      string
	businessType    model.BusinessType
	entry           AccountingEntry
	currency        string
	transactionDate string
	transactionTime time.Time
	description     string
	// cutDate 日切归属日（YYYY-MM-DD）。booking 入口处 computeCutDate 一次性
	// 确定，propagate 到所有 per-shard Confirm tx + tcc_coordinator → 同一 voucher
	// 的所有 entry 必然共享同一 cut_date → 试算平衡天然成立。
	cutDate         string
}

// ─── 死锁重试 ─────────────────────────────────────────────────────────────

// deadlockMaxRetries 单个 TCC 事务的最大重试次数。
// 3 次 ≈ 10ms + 20ms + 40ms ≈ 70ms 总上限，已足够吸收 99%+ 的瞬时死锁；
// 仍失败则交由上层 transaction_order retry_count 兜底。
const deadlockMaxRetries = 3

// isRetriableMySQLErr 判断是否为 MySQL 可重试瞬时错误。
// 这些错误在设计上都是"失败 + 锁/资源已释放"的可恢复瞬时状态，上层重试即可:
//
//   - 1213 (ER_LOCK_DEADLOCK, SQLSTATE 40001): 死锁，被选为 victim 的事务已 rollback。
//   - 1205 (ER_LOCK_WAIT_TIMEOUT):             等锁超时（默认 50s），事务通常被 rollback
//     （或依据 innodb_rollback_on_timeout）。热点账户下十分常见。
//   - 1040 (ER_CON_COUNT_ERROR):               连接数超限；短暂退避后通常能获得新连接。
//   - 1317 (ER_QUERY_INTERRUPTED):             查询被中断（如 KILL、连接 reset），重试安全。
//   - 1614 (ER_XA_RBDEADLOCK):                 XA 场景的死锁，极少见但语义与 1213 相同。
//
// 保持向后兼容：旧名 isDeadlockError 作为 wrapper 保留。
func isRetriableMySQLErr(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	switch me.Number {
	case 1213, 1205, 1040, 1317, 1614:
		return true
	}
	return false
}

// isDeadlockError 仅匹配 1213，保留给明确只想处理 deadlock 的调用点。
func isDeadlockError(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1213
	}
	return false
}

// retryOnDeadlock 在 MySQL 可重试瞬时错误（见 isRetriableMySQLErr）时按 jitter 指数退避
// 重试 fn，最多 deadlockMaxRetries 次。fn 必须在返回错误前完成事务的 Rollback——
// retry 不负责清理未完成事务。不可重试错误立即返回。
//
// 方法名保留 "OnDeadlock" 是历史原因；实际吸收的错误集合现在更广（1213/1205/1040/1317/1614）。
func (s *accountingService) retryOnDeadlock(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < deadlockMaxRetries; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetriableMySQLErr(err) {
			return err
		}
		// 10ms → 20ms → 40ms，加 0~半程 jitter 打散并发 victim 同步重试
		base := time.Duration(10<<attempt) * time.Millisecond
		jitter := time.Duration(rand.Int63n(int64(base/2) + 1))
		if s.logger != nil {
			s.logger.Debug("retrying after transient mysql error",
				zap.Int("attempt", attempt+1),
				zap.Duration("backoff", base+jitter),
				zap.Error(err))
		}
		time.Sleep(base + jitter)
	}
	metrics.DeadlockRetryExhaustedTotal.Inc()
	return fmt.Errorf("transient mysql error retry exhausted after %d attempts: %w", deadlockMaxRetries, lastErr)
}
