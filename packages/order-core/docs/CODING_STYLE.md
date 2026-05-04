# 三仓库代码骨架与风格统一规范

`order-core` / `payment-core` / `payment-channel` 三个仓库的目录结构、命名约定、
代码风格保持一致，方便相互翻代码、复用工具。

---

## 1. 目录结构（三仓通用模板）

> **存储分层**：
> - `order-core`：分库分表落业务实体（订单 / 支付意图 / 退款 / 通知日志 /
>   差错单）。
> - `payment-core`：**无状态、无 DB**，只是路由 + 调用 + 规范化的薄层。
> - `payment-channel`：分库分表落**渠道调用流水** + **幂等表**：
>   - **调用流水**：每次向第三方（Stripe / GCash / Maya / GrabPay / ...）
>     发出请求、收到响应、收到 webhook 都要落一行原始 request / response
>     快照（含 HTTP method、URL、headers、body、status、latency、签名、
>     错误堆栈）。这是审计、对账、投诉举证、复盘排错的唯一可信源。
>   - **幂等表**：同一个 `(adapter, idempotency_key)` 进来，不论是网络
>     抖动重发还是 worker 重试，都直接返回首次调用的渠道响应快照，**绝
>     不向第三方重复下单**。`idempotency_key` 由上游 `payment-core` 透
>     传（`order-core` 的 `pi_id` + 操作类型 hash 后即可），写入用唯一
>     索引 `UNIQUE(adapter, idem_key)` 兜底，写冲突 → 直接读旧记录。
>   - 同时落 token / mandate 映射、重试调度。
>   - 分片策略与 `order-core` 一致（10 库 × 10 表 = 100 分片），分片键
>     用 `payment_intent_id`（即 `pi_*`）保证同一笔支付在两仓落同一个
>     逻辑分片号，便于跨仓 join 排查。
>
> 因此下表中带 `*` 的目录 **只在 `order-core` 与 `payment-channel` 出现**，
> `payment-core` 不需要这些目录。

```
<repo>/
├── api/proto/<name>/v1/<name>.proto    单一 proto，按 service 切分 message
├── cmd/
│   ├── server/main.go                  fx.New 装配 + Invoke
│   └── grpc-client/main.go             CLI 调试工具
├── config/
│   ├── config.yaml                     本地默认
│   └── config.docker.yaml              docker-compose 用
├── database/                          *order-core / payment-channel：分库分表 + 元数据
│   ├── <repo>db/templates/schema.sql   分片表模板（${TABLE} 占位符）
│   ├── <repo>db/scripts/generate.sh    生成 0..9_init.sql
│   ├── <repo>db/init/                  生成产物
│   ├── metadb/init/init.sql            非分片元数据（leaf_alloc 等）
│   └── migrations/                     后续 schema 变更（带 apply.sh）
├── internal/
│   ├── channel/                        对外接口（依赖倒置：本仓库定义，下游实现）
│   ├── crypto/                         字段加密（落库前 / 签名都用）
│   ├── domain/                         实体 + 状态机 + 错误
│   ├── idgen/                         *order-core / payment-channel：Leaf Segment id
│   ├── metrics/                        Prometheus 指标
│   ├── repo/                          *order-core / payment-channel：分片仓储 + DBManager
│   ├── server/                         gRPC adapter + interceptors
│   ├── service/                        业务编排 + workers
│   └── sharding/                      *order-core / payment-channel：分片路由器
├── docs/                               架构 / 接入 / 风格文档
├── docker-compose.yml                  一键起栈
├── Dockerfile
├── Makefile
├── README.md
├── go.mod
└── .gitignore
```

三仓的差异：
- `payment-core`：**只负责集成 `payment-channel`**。职责仅有
  「按 `payment_method` / 国家 / 商户路由 → 选 `payment-channel` adapter →
  调用 → 把结果规范化回 `order-core` 协议」。所有业务状态都在 `order-core`、
  渠道流水都在 `payment-channel`，本层完全无状态、无 DB；配置驱动、热加载
  即可。仍保留 `internal/channel/`（定义 `RoutedAcquirer` interface），
  由 `payment-channel` 实现。
- `payment-channel`：每个第三方渠道一个 adapter，按 `payment_intent_id`
  分库分表落渠道侧实体（`acquirer_tx` / `webhook_raw` / `idem_key` /
  `token_mapping`），分片配置 10×10 与 `order-core` 一致；落库前后做签名 /
  解密 / 重试调度。只实现 `payment-core/internal/channel/RoutedAcquirer`，
  自己不向外定义 interface。

每个仓库的 `internal/channel/` 内容不同（见各仓 README），但其它目录意义相同。

---

## 2. 命名约定

### Go package
- package 名小写、单数（`domain` / `repo` / `service` / `channel`）
- 不用下划线

### Type
- 实体：`PaymentIntent` / `Charge` / `Refund` / `PayAction`
- service：`PaymentIntentService` interface + `piService` 私有 struct
- repo：`PaymentIntentRepository` interface + `piRepo` 私有 struct
- 输入：`CreatePaymentIntentInput`、`PayInput`
- 错误：`Err<XxxNotFound>` / `ErrValidation` / `ErrInvalidTransition`

### gRPC method
- `Create / Retrieve / Update / Cancel / List`（Stripe 风格）
- 不要混用 `Get / Read / Find`，统一 `Retrieve`
- 不要混用 `Delete / Remove`，统一 `Cancel`（业务上很少真正物理删）

### SQL 表
- 主表 `<entity>_XX` 100 张分片
- 非分片元数据表写实际名（`leaf_alloc`）
- 列名 snake_case：`payment_intent_id`、`expired_at`

### ID 前缀
| 实体 | 前缀 | 例 |
|---|---|---|
| PaymentIntent | `pi_` | `pi_437123456` |
| Charge | `ch_` | `ch_437123457` |
| Refund | `re_` | `re_437123458` |
| PayAction | `act_` | `act_437123459` |
| NotifyLog | `nl_` | `nl_437123460` |
| Customer (payment-core) | `cus_` | |
| PaymentMethod (payment-core) | `pm_` | |
| Acquirer Tx (payment-channel) | `aq_` | |

ID 格式：`<prefix>_<dbIdx:1d><tblIdx:02d><seq>` —— 前 3 字符携带分片信息。

---

## 3. 状态机统一

- domain/<entity>.go 里定义 `<Entity>Status` 常量集
- 同文件给出 `Valid<Entity>Transitions map[Status]map[Status]struct{}`
- 同文件给出 `Can<Entity>Transition(from, to) bool` 函数
- service 里所有状态变更都先 `if !CanXxxTransition(...)` 校验

```go
// internal/domain/<entity>.go
type FooStatus string
const (...)

var ValidFooTransitions = map[FooStatus]map[FooStatus]struct{}{...}

func CanFooTransition(from, to FooStatus) bool {...}
```

测试同文件：`internal/domain/<entity>_test.go`。

---

## 4. 分片路由统一

每个仓库都用同一份 `internal/sharding/router.go`（直接复制即可，不抽公共库）：

- 10 库 × 10 表 = 100 全局分片
- `RouteByString(s)` FNV-1a 64-bit
- `RouteByPrefixedID(id)` 解析 `<prefix>_<db><tbl>...`
- `FormatID(prefix, db, tbl, seq)` 构造 ID
- `GetTableName(base, tbl)` → `base_NN`

测试：`internal/sharding/router_test.go`。

---

## 5. fx 装配模板

`cmd/server/main.go` 统一用 `go.uber.org/fx`：

```go
func main() {
    metrics.Register()
    app := fx.New(
        fx.Provide(
            loadConfig,
            newLogger,
            newRouter,
            newDBManager,
            newIDGen,
            // 渠道 / 注册表
            newChannelRegistry,
            // repos
            repoXxx, repoYyy,
            // services
            svcXxx, svcYyy,
            // server / workers
            newServer,
            newWorkerXxx,
        ),
        fx.Invoke(startGRPC, startWorkerXxx, startMetricsHTTP),
    )
    app.Run()
}
```

---

## 6. gRPC server 模板

```go
type Server struct { orderv1.UnimplementedXxxServer; ... }

type Deps struct { ...; Logger *zap.Logger }

func NewServer(d Deps) *Server { ... }

func (s *Server) ListenAndServe(ctx, port) error {
    gs := grpc.NewServer(grpc.ChainUnaryInterceptor(
        RecoverInterceptor(s.logger),
        MetricsInterceptor(),
        RateLimitInterceptor(rps, burst),
        AuthInterceptor(tokens, s.logger),
    ))
    orderv1.RegisterXxxServer(gs, s)
    health.NewServer().SetServingStatus(...)
    reflection.Register(gs)
    go func(){ <-ctx.Done(); gs.GracefulStop() }()
    return gs.Serve(lis)
}
```

interceptors 文件名 `internal/server/interceptors.go`，内容：
- `RecoverInterceptor` panic 保护
- `MetricsInterceptor` `:9090/metrics`
- `RateLimitInterceptor` `golang.org/x/time/rate`
- `AuthInterceptor` bearer token

多 service 用 forwarder struct 避免 Go 方法名冲突（见 `internal/server/grpc.go` 的
`ChargeForwarder` / `RefundForwarder` / `WebhookForwarder`）。

---

## 7. repo 模板

```go
type FooRepository interface {
    Create(ctx, *Foo) error
    Get(ctx, parentID, id) (*Foo, error)
    UpdateFields(ctx, parentID, id, map[string]any) (*Foo, error)
    ListByParent(ctx, parentID) ([]*Foo, error)
    // 跨分片扫描（cron worker 用）
    ListXxxDue(ctx, time.Time, limit int) ([]*Foo, error)
}

type fooRepo struct {
    mgr    *Manager
    router *sharding.Router
}

func NewFooRepository(mgr, r) FooRepository { ... }

// shardOf 必须接收 ctx 并通过 router.TableName(ctx, ...) 拼表名，
// 让 shadow 流量自动落到 _shadow 后缀的影子表。
func (r *fooRepo) shardOf(ctx context.Context, parentID string) (*gorm.DB, string, error) {
    dbIdx, tblIdx := r.router.RouteByPrefixedID(parentID)
    db, _ := r.mgr.GetShard(dbIdx)
    return db, r.router.TableName(ctx, "foo", tblIdx), nil
}
```

> **不要** 直接用 `router.GetTableName(...)`（只返主表），新代码必须走
> `router.TableName(ctx, ...)`。前者已被标记 deprecated，仅供 schema migrator
> 等启动期工具使用。

GORM 日志一律走 `internal/repo/gorm_logger.go::ZapGormLogger`，
启动时 `repo.SetSQLLogger(logger.Named("sql"))`。

---

## 8. service 模板

```go
type FooService interface {
    Create(ctx, *CreateFooInput) (*Foo, error)
    DoXxx(ctx, id, ...) (*Foo, error)
    ...
}

type fooService struct {
    repo  repo.FooRepository
    deps  ...
    logger *zap.Logger
}

func NewFooService(repo, deps, logger) FooService { ... }

// 业务方法都是 ctx 优先 + 校验先行
func (s *fooService) DoXxx(ctx, id, ...) (*Foo, error) {
    if id == "" {
        return nil, fmt.Errorf("%w: id required", domain.ErrValidation)
    }
    foo, err := s.repo.Get(ctx, id)
    if err != nil { return nil, err }
    if !domain.CanFooTransition(foo.Status, target) {
        return nil, fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, foo.Status, target)
    }
    return s.repo.UpdateFields(ctx, id, map[string]any{"status": target})
}
```

worker 文件 `internal/service/<entity>_workers.go`：

```go
type XxxWorker struct { ...; interval time.Duration; logger *zap.Logger }

func NewXxxWorker(...) *XxxWorker { ... }

func (w *XxxWorker) Start(ctx context.Context) {
    t := time.NewTicker(w.interval)
    defer t.Stop()
    runOnce()
    for {
        select { case <-ctx.Done(): return; case <-t.C: runOnce() }
    }
}
```

---

## 9. 错误风格

domain/<entity>.go 顶部声明 sentinel error：

```go
var (
    ErrFooNotFound      = errors.New("foo not found")
    ErrFooInvalidState  = errors.New("foo invalid state")
)
```

service 包装：

```go
return fmt.Errorf("%w: detail %s", domain.ErrFooNotFound, foo.ID)
```

grpc adapter 用 `mapError(err) error` 把 sentinel 翻译为 codes.NotFound / InvalidArgument / FailedPrecondition / AlreadyExists / Internal。

---

## 10. 字段加密

敏感字段（card token / OTP / password hash / client_secret）：
- 落库前 `cipher.Seal()`
- 取出后 `cipher.Open()`
- prod 用 `crypto.NewAESGCM(kmsKey)`，dev 用 `crypto.NoopCipher{}`

key 通过 KMS / Vault 注入；不入仓库不入配置。

---

## 11. 测试

最少要有的测试：

- `internal/domain/<entity>_test.go` 状态机所有合法 / 非法迁移
- `internal/sharding/router_test.go` 路由一致性
- `internal/crypto/crypto_test.go` 加解密往返

集成测试若需要 MySQL 用 `docker-compose -f docker-compose.test.yml up -d` 起。

---

## 12. 提交规范

- 每个 commit 解决一个独立目标
- commit message 用英文 + 详细 body：
  - 第一行 ≤ 72 字符的 imperative 标题
  - 空行
  - 详细说明：动机、改了什么、注意事项
  - 末行附 session URL（Claude Code 自动附）
- 不要混合"功能开发"与"格式调整"，独立 commit
- 重大架构改动前先在 docs/ARCHITECTURE.md 落一笔

---

## 13. 三仓 channel 接口（依赖倒置）

| 仓库 | `internal/channel/` 内容 |
|---|---|
| `order-core` | `PaymentChannel` interface（被 payment-core 实现）；`Notifier` interface |
| `payment-core` | `RoutedAcquirer` interface（被 payment-channel 各 adapter 实现） |
| `payment-channel` | 不定义 interface，只实现 `payment-core/internal/channel/RoutedAcquirer` |

每一层都按 "本层的 interface + 下层的实现" 解耦，互不直接 import。
