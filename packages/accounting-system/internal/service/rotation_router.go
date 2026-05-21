package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/repository"
)

// ============================================================================
// Booking Router — 方向 B 实现
//
// 业务方调记账接口时不指定 account_no，而是指定 logical_account_key + flow_id。
//
// 方向 B 核心：
//   - tx_account_anchor 按 account_no 分片（与 transaction、account 同片）
//   - flow_anchor_route 按 flow_id 分片，承担 (flow_id, LA_id) → account_no 路由
//   - 首次锚定 = 两步事务（先 route 后 anchor），不同分片各自本地提交
//   - 后续记账 = 单分片本地事务（anchor + transaction 同片）
//   - 收敛 job = 单分片查询（按 account_no 直接查 anchor 表）
//
// Router 责任：
//   1. 解析 logical_account（5s 缓存 + singleflight 防击穿）
//   2. 通过 FlowAnchorRoute 查 flow 锁定的 account_no
//   3. 处理跨源继承（refund-of / reverse-of）
//   4. phase 守卫（含 frozen TCC Cancel 例外）
//   5. 返回 Resolution 给 caller，包含 RoutePlan + AnchorPlan
//
// 调用方使用 Resolution：
//   - 若 RoutePlan.Op == RouteOpInsert：先写 routing 表（flow_id 分片）
//   - 然后写 anchor + transaction（account_no 分片同一本地事务）
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §5
//
// 关键不变量（强制）：
//   I-R1 同一 (flow_id, logical_account_id) 永远落到同一 instance（routing 表保证）
//   I-R2 anchor 跨期通过 migration_chain 跟随；chain depth > 5 → 进入 quarantined
//   I-R3 路由层未注册的 logical_account_key 绝不 lazy create
//   I-R4 phase=frozen 仅允许 TCC Cancel 例外写入（anchor 必须存在且 status=trying）
// ============================================================================

// BookingDirection 借/贷方向。
type BookingDirection int8

const (
	BookingDirectionDebit  BookingDirection = 1
	BookingDirectionCredit BookingDirection = 2
)

func (d BookingDirection) IsValid() bool {
	return d == BookingDirectionDebit || d == BookingDirectionCredit
}

// BookingType 业务记账类型。
type BookingType int8

const (
	BookingTypeNormal     BookingType = 0
	BookingTypeTCCTry     BookingType = 1
	BookingTypeTCCConfirm BookingType = 2
	BookingTypeTCCCancel  BookingType = 3
)

// ResolveRequest 调用方传给 Router 的解析请求。
type ResolveRequest struct {
	LogicalAccountKey string
	FlowID            string
	BookingType       BookingType
	OccurredAt        time.Time

	// 退款/红冲二选一（互斥）：业务方根据手里有什么信息选择
	OriginalFlowID        string // 方向 1：直接传源 flow_id
	OriginalTransactionID string // 方向 2：传源 tx_id（router 内部解析）

	// 任一 Original* 非空时必填
	ReuseSource model.AnchorReuseSource

	Direction BookingDirection
}

func (r *ResolveRequest) Validate() error {
	if r == nil {
		return errors.New("ResolveRequest is nil")
	}
	if r.LogicalAccountKey == "" {
		return errors.New("logical_account_key required")
	}
	if r.FlowID == "" {
		return errors.New("flow_id required")
	}
	if !r.Direction.IsValid() {
		return fmt.Errorf("invalid direction: %d", r.Direction)
	}
	if r.OriginalFlowID != "" && r.OriginalTransactionID != "" {
		return errors.New("OriginalFlowID and OriginalTransactionID are mutually exclusive")
	}
	if (r.OriginalFlowID != "" || r.OriginalTransactionID != "") &&
		r.ReuseSource != model.AnchorReuseSourceRefundOf &&
		r.ReuseSource != model.AnchorReuseSourceReverseOf {
		return fmt.Errorf("Original* set but ReuseSource must be refund-of or reverse-of, got %s",
			r.ReuseSource)
	}
	return nil
}

// Resolution 路由解析结果。caller 用 AccountNo + AnchorGlobalTableIndex 完成 anchor + transaction
// 写入；用 RoutePlan + RouteGlobalTableIndex 完成 routing 写入（如需要）。
type Resolution struct {
	LogicalAccount *model.LogicalAccount

	// AccountNo flow 锁定的 instance（已跟随 migration chain）。
	AccountNo string

	// AnchorGlobalTableIndex anchor 表分片号（由 RouteByAccountNo 决定）。
	AnchorGlobalTableIndex int

	// RouteGlobalTableIndex routing 表分片号（由 hashStringMod100(flow_id) 决定）。
	RouteGlobalTableIndex int

	// ExistingAnchor 命中的 anchor（自身命中或复用源命中后跟随到的）。nil = 需新建。
	ExistingAnchor *model.TxAccountAnchor

	// IsLegacy logical_account.rotation_enabled=0；caller 应走旧 API 路径（不写 anchor/route）。
	IsLegacy bool

	// AnchorPlan anchor 表写入计划。
	AnchorPlan *AnchorPlan

	// RoutePlan routing 表写入计划。nil = 无需写 routing（已存在）。
	RoutePlan *RoutePlan
}

// AnchorPlan caller 在 anchor 所在分片本地事务内执行的操作。
type AnchorPlan struct {
	Op AnchorOp

	// NewAnchor 当 Op=Insert 时使用。
	NewAnchor *model.TxAccountAnchor

	// UpdateTargetID 当 Op=UpdatePosting 时使用。
	UpdateTargetID        int64
	UpdateExpectedVersion int64
	UpdateNewMask         model.AnchorDirectionMask
}

type AnchorOp int8

const (
	AnchorOpInsert        AnchorOp = 1
	AnchorOpUpdatePosting AnchorOp = 2
)

// RoutePlan caller 在 routing 所在分片本地事务内执行的操作。
type RoutePlan struct {
	Op RouteOp

	// NewRoute 当 Op=Insert 时使用。
	NewRoute *model.FlowAnchorRoute
}

type RouteOp int8

const (
	RouteOpInsert RouteOp = 1
)

// ============================================================================
// Router
// ============================================================================

type Router interface {
	Resolve(ctx context.Context, req *ResolveRequest) (*Resolution, error)
}

type LogicalAccountReader interface {
	GetByKey(ctx context.Context, key string) (*model.LogicalAccount, error)
	GetByID(ctx context.Context, id int64) (*model.LogicalAccount, error)
}

type AnchorReader interface {
	GetByFlowAndAccount(ctx context.Context, flowID string, accountNo string) (*model.TxAccountAnchor, error)
	RouteByAccountNo(accountNo string) (dbIndex, globalTableIndex int)
}

type RouteReader interface {
	GetByFlowAndLogical(ctx context.Context, flowID string, logicalAccountID int64) (*model.FlowAnchorRoute, error)
	RouteByFlowID(flowID string) (dbIndex, globalTableIndex int)
}

type AccountReaderForRouter interface {
	GetByAccountNo(ctx context.Context, accountNo string) (*model.Account, error)
}

// TransactionReaderForRouter 方向 2 用：tx_id → flow_id 解析。可为 nil 表示仅支持方向 1。
type TransactionReaderForRouter interface {
	GetFlowIDByTransactionID(ctx context.Context, transactionID string) (string, error)
}

type router struct {
	logicals     LogicalAccountReader
	anchors      AnchorReader
	routes       RouteReader
	accounts     AccountReaderForRouter
	transactions TransactionReaderForRouter
	clock        func() time.Time

	cache *logicalAccountCache
}

// NewRouter 构造默认路由器。
func NewRouter(
	logicals LogicalAccountReader,
	anchors AnchorReader,
	routes RouteReader,
	accounts AccountReaderForRouter,
	transactions TransactionReaderForRouter,
	clock func() time.Time,
) Router {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &router{
		logicals:     logicals,
		anchors:      anchors,
		routes:       routes,
		accounts:     accounts,
		transactions: transactions,
		clock:        clock,
		cache:        newLogicalAccountCache(5 * time.Second),
	}
}

// Resolve 见接口。
func (r *router) Resolve(ctx context.Context, req *ResolveRequest) (*Resolution, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	// Step 1: 加载 logical_account
	la, err := r.loadLogicalAccount(ctx, req.LogicalAccountKey)
	if err != nil {
		return nil, err
	}
	if !la.IsRotating() {
		return &Resolution{
			LogicalAccount: la,
			IsLegacy:       true,
		}, nil
	}

	// Step 2: 查本 flow 的 routing 记录
	selfRoute, err := r.routes.GetByFlowAndLogical(ctx, req.FlowID, la.ID)
	if err != nil {
		return nil, fmt.Errorf("router: get self route: %w", err)
	}

	if selfRoute != nil {
		// 已锁定：通过 route.AccountNo 找 anchor
		return r.resolveExistingFlow(ctx, req, la, selfRoute)
	}

	// Step 3: routing 不存在 → 首次锚定
	// 3a. 若是退款/红冲：通过源 flow 的 routing 继承 account_no
	srcAccountNo, reuseSource, srcRouteChainDepth, err := r.resolveSourceInheritance(ctx, req, la)
	if err != nil {
		return nil, err
	}

	// 3b. 否则用 current_active_account_no
	if srcAccountNo == "" {
		if la.CurrentActiveAccountNo == nil || *la.CurrentActiveAccountNo == "" {
			return nil, fmt.Errorf("%w: logical=%s", model.ErrNoActiveInstance, la.LogicalAccountKey)
		}
		srcAccountNo = *la.CurrentActiveAccountNo
	}

	// 4. 校验目标 account 的 phase 允许接收
	acc, err := r.accounts.GetByAccountNo(ctx, srcAccountNo)
	if err != nil {
		return nil, fmt.Errorf("router: get account=%s: %w", srcAccountNo, err)
	}
	if acc == nil {
		return nil, fmt.Errorf("%w: target account_no=%s not found",
			model.ErrNoActiveInstance, srcAccountNo)
	}
	// 新锚定：要求 active；继承的源 anchor 可能在 draining
	if reuseSource == model.AnchorReuseSourcePrimary && !acc.AcceptsNewAnchoring() {
		return nil, fmt.Errorf("%w: target phase=%s not accepting new anchoring",
			model.ErrNoActiveInstance, acc.LifecyclePhase)
	}
	if reuseSource != model.AnchorReuseSourcePrimary {
		// 继承场景：允许 active/draining，frozen 上的 TCC Cancel 例外见下面
		if err := r.checkPhaseGuard(acc, req.BookingType); err != nil {
			return nil, err
		}
	}

	// 5. 构造 RoutePlan + AnchorPlan
	now := r.clock()
	_, anchorGtbl := r.anchors.RouteByAccountNo(srcAccountNo)
	_, routeGtbl := r.routes.RouteByFlowID(req.FlowID)

	newRoute := &model.FlowAnchorRoute{
		FlowID:              req.FlowID,
		LogicalAccountID:    la.ID,
		AccountNo:           srcAccountNo,
		MigrationChainDepth: srcRouteChainDepth, // 继承源的迁移深度（若有）
		CreatedAt:           now,
	}

	srcAnchorID := int64(0)
	newAnchor := &model.TxAccountAnchor{
		FlowID:           req.FlowID,
		LogicalAccountID: la.ID,
		AccountNo:        srcAccountNo,
		AnchoredAt:       now,
		LastPostingAt:    now,
		PostingCount:     1,
		Status:           model.AnchorStatusTrying,
		ReuseSource:      reuseSource,
	}
	switch req.Direction {
	case BookingDirectionDebit:
		newAnchor.DirectionMask = newAnchor.DirectionMask.WithDebit()
	case BookingDirectionCredit:
		newAnchor.DirectionMask = newAnchor.DirectionMask.WithCredit()
	}
	if req.BookingType == BookingTypeNormal {
		newAnchor.Status = model.AnchorStatusActive
	}
	if reuseSource != model.AnchorReuseSourcePrimary && srcAnchorID > 0 {
		newAnchor.ReuseSourceAnchorID = &srcAnchorID
	}

	return &Resolution{
		LogicalAccount:         la,
		AccountNo:              srcAccountNo,
		AnchorGlobalTableIndex: anchorGtbl,
		RouteGlobalTableIndex:  routeGtbl,
		IsLegacy:               false,
		AnchorPlan: &AnchorPlan{
			Op:        AnchorOpInsert,
			NewAnchor: newAnchor,
		},
		RoutePlan: &RoutePlan{
			Op:       RouteOpInsert,
			NewRoute: newRoute,
		},
	}, nil
}

// resolveExistingFlow 已有 routing → 走 UpdatePosting 路径
func (r *router) resolveExistingFlow(
	ctx context.Context, req *ResolveRequest, la *model.LogicalAccount, route *model.FlowAnchorRoute,
) (*Resolution, error) {
	accountNo := route.AccountNo
	anchor, err := r.anchors.GetByFlowAndAccount(ctx, req.FlowID, accountNo)
	if err != nil {
		return nil, fmt.Errorf("router: get anchor by (flow, account): %w", err)
	}
	if anchor == nil {
		return nil, fmt.Errorf("router: routing record exists but anchor missing (flow=%s, account=%s) — likely incomplete first-time anchor write",
			req.FlowID, accountNo)
	}

	// 跟随 migration chain（如有）
	finalAccountNo := anchor.AccountNo
	if anchor.IsMigrated() && anchor.MigratedToAccountNo != nil && *anchor.MigratedToAccountNo != "" {
		finalAccountNo = *anchor.MigratedToAccountNo
	}

	acc, err := r.accounts.GetByAccountNo(ctx, finalAccountNo)
	if err != nil {
		return nil, fmt.Errorf("router: get final account=%s: %w", finalAccountNo, err)
	}
	if acc == nil {
		return nil, fmt.Errorf("router: final account=%s not found", finalAccountNo)
	}
	if err := r.checkPhaseGuard(acc, req.BookingType); err != nil {
		return nil, err
	}

	_, anchorGtbl := r.anchors.RouteByAccountNo(finalAccountNo)
	_, routeGtbl := r.routes.RouteByFlowID(req.FlowID)

	newMask := anchor.DirectionMask
	switch req.Direction {
	case BookingDirectionDebit:
		newMask = newMask.WithDebit()
	case BookingDirectionCredit:
		newMask = newMask.WithCredit()
	}

	return &Resolution{
		LogicalAccount:         la,
		AccountNo:              finalAccountNo,
		AnchorGlobalTableIndex: anchorGtbl,
		RouteGlobalTableIndex:  routeGtbl,
		ExistingAnchor:         anchor,
		IsLegacy:               false,
		AnchorPlan: &AnchorPlan{
			Op:                    AnchorOpUpdatePosting,
			UpdateTargetID:        anchor.ID,
			UpdateExpectedVersion: anchor.Version,
			UpdateNewMask:         newMask,
		},
		// RoutePlan nil — routing 已存在
	}, nil
}

// resolveSourceInheritance 处理退款/红冲源 flow 继承。
// 返回 (源 flow 锁定的 account_no, reuseSource, chain_depth, error)。
// 若本请求不是 refund/reverse，返回 ("", Primary, 0, nil)。
func (r *router) resolveSourceInheritance(
	ctx context.Context, req *ResolveRequest, la *model.LogicalAccount,
) (string, model.AnchorReuseSource, int8, error) {
	// 解析源 flow_id
	srcFlowID := req.OriginalFlowID
	if srcFlowID == "" && req.OriginalTransactionID != "" {
		if r.transactions == nil {
			return "", model.AnchorReuseSourcePrimary, 0,
				errors.New("OriginalTransactionID provided but TransactionReader not configured")
		}
		resolved, err := r.transactions.GetFlowIDByTransactionID(ctx, req.OriginalTransactionID)
		if err != nil {
			return "", model.AnchorReuseSourcePrimary, 0,
				fmt.Errorf("resolve flow_id from transaction_id=%s: %w", req.OriginalTransactionID, err)
		}
		if resolved == "" {
			return "", model.AnchorReuseSourcePrimary, 0,
				fmt.Errorf("source transaction not found: transaction_id=%s", req.OriginalTransactionID)
		}
		srcFlowID = resolved
	}
	if srcFlowID == "" {
		return "", model.AnchorReuseSourcePrimary, 0, nil
	}

	// 查源 routing
	srcRoute, err := r.routes.GetByFlowAndLogical(ctx, srcFlowID, la.ID)
	if err != nil {
		return "", model.AnchorReuseSourcePrimary, 0,
			fmt.Errorf("get source flow route: %w", err)
	}
	if srcRoute == nil {
		return "", model.AnchorReuseSourcePrimary, 0,
			fmt.Errorf("source flow %s has no anchor on logical_account=%s",
				srcFlowID, la.LogicalAccountKey)
	}
	return srcRoute.AccountNo, req.ReuseSource, srcRoute.MigrationChainDepth, nil
}

// ============================================================================
// Phase guard / logical account loading / cache — 复用之前实现
// ============================================================================

func (r *router) loadLogicalAccount(ctx context.Context, key string) (*model.LogicalAccount, error) {
	if la, ok := r.cache.get(key, r.clock()); ok {
		return la, nil
	}
	la, err := r.cache.loadOnce(key, func() (*model.LogicalAccount, error) {
		return r.logicals.GetByKey(ctx, key)
	}, r.clock())
	if err != nil {
		return nil, err
	}
	return la, nil
}

func (r *router) checkPhaseGuard(acc *model.Account, bookingType BookingType) error {
	phase := acc.LifecyclePhase

	// 例外：frozen + TCC Cancel
	if phase == model.LifecyclePhaseFrozen && bookingType == BookingTypeTCCCancel {
		return nil
	}

	if phase.AcceptsFollowupPosting() {
		return nil
	}

	return fmt.Errorf("%w: account_no=%s phase=%s booking_type=%d",
		model.ErrPhaseGuardRejected, acc.AccountNo, phase, bookingType)
}

// ============================================================================
// Logical account cache — 5s TTL + singleflight
// ============================================================================

type logicalAccountCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]*lacEntry
	flights map[string]*lacFlight
}

type lacEntry struct {
	la       *model.LogicalAccount
	expireAt time.Time
}

type lacFlight struct {
	wg  sync.WaitGroup
	la  *model.LogicalAccount
	err error
}

func newLogicalAccountCache(ttl time.Duration) *logicalAccountCache {
	return &logicalAccountCache{
		ttl:     ttl,
		entries: make(map[string]*lacEntry),
		flights: make(map[string]*lacFlight),
	}
}

func (c *logicalAccountCache) get(key string, now time.Time) (*model.LogicalAccount, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if now.After(e.expireAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.la, true
}

func (c *logicalAccountCache) loadOnce(
	key string, loader func() (*model.LogicalAccount, error), now time.Time,
) (*model.LogicalAccount, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Before(e.expireAt) {
		c.mu.Unlock()
		return e.la, nil
	}
	if fl, ok := c.flights[key]; ok {
		c.mu.Unlock()
		fl.wg.Wait()
		if fl.err != nil {
			return nil, fl.err
		}
		return fl.la, nil
	}
	fl := &lacFlight{}
	fl.wg.Add(1)
	c.flights[key] = fl
	c.mu.Unlock()

	la, err := loader()

	c.mu.Lock()
	delete(c.flights, key)
	if err == nil && la != nil {
		c.entries[key] = &lacEntry{la: la, expireAt: now.Add(c.ttl)}
	}
	fl.la = la
	fl.err = err
	c.mu.Unlock()
	fl.wg.Done()
	return la, err
}

func (c *logicalAccountCache) invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// ============================================================================
// Repo → reader adapters
// ============================================================================

type anchorReaderAdapter struct{ repo repository.AnchorRepository }

func (a *anchorReaderAdapter) GetByFlowAndAccount(ctx context.Context, flowID string, accountNo string) (*model.TxAccountAnchor, error) {
	return a.repo.GetByFlowAndAccount(ctx, flowID, accountNo)
}
func (a *anchorReaderAdapter) RouteByAccountNo(accountNo string) (int, int) {
	return a.repo.RouteByAccountNo(accountNo)
}

type routeReaderAdapter struct{ repo repository.FlowAnchorRouteRepository }

func (a *routeReaderAdapter) GetByFlowAndLogical(ctx context.Context, flowID string, laID int64) (*model.FlowAnchorRoute, error) {
	return a.repo.GetByFlowAndLogical(ctx, flowID, laID)
}
func (a *routeReaderAdapter) RouteByFlowID(flowID string) (int, int) {
	return a.repo.RouteByFlowID(flowID)
}

// NewRouterFromRepos 工厂：用现成 repo 构造路由器（生产路径）。
func NewRouterFromRepos(
	logicalRepo repository.LogicalAccountRepository,
	anchorRepo repository.AnchorRepository,
	routeRepo repository.FlowAnchorRouteRepository,
	accountReader AccountReaderForRouter,
	transactionReader TransactionReaderForRouter,
	clock func() time.Time,
) Router {
	return NewRouter(
		logicalRepo,
		&anchorReaderAdapter{repo: anchorRepo},
		&routeReaderAdapter{repo: routeRepo},
		accountReader,
		transactionReader,
		clock,
	)
}
