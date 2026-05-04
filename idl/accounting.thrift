namespace go accounting

// =============================================================================
// accounting.thrift
//
// Canonical IDL for the accounting service. All other services (payment-core,
// order-core, user-merchant-core, etc.) generate Kitex Thrift clients from
// this file rather than maintaining their own copy.
//
// The former `accounting-grpc-api` protobuf repo is DEPRECATED; this Thrift
// IDL is the single source of truth.
//
// Design notes
// ------------
// * Amounts are strings (decimal) across the wire to avoid float/int64
//   precision issues; all monetary fields use strings.
// * Account identity key is (userId, accountBusinessType, currency). For
//   platform/channel accounts the "userId" field holds the platform-owned
//   owner id (reserved id space documented below).
// * Owner id space:
//     users:                 [100000000, 899999999]
//     merchants:             [900000000, ...]
//     platform/channel accts: [1, 10000] (reserved)
// * accountBusinessType is a raw i32 (NOT a compile-time enum). Values are
//   registered at runtime via accounting-admin-web and stored in
//   account_meta.account_business_type_info. Callers should look up / cache
//   the numeric id and pass it as-is. A seed set (USER_BALANCE, MERCHANT_*,
//   TRANSIT_CHANNEL_*, TRANSACTION_FEE, CHARGE_FEE, PLATFORM_PNL, etc.) lives
//   in the metadb seed; channel-specific types (e.g. GCASH_RECEIVABLE) are
//   created through the admin UI.
// =============================================================================

// ---- Enums (compile-time, small cardinality) -------------------------------

// AccountType 账户类型（与 Go 实现保持一致：9 个取值）
enum AccountType {
    UNSPECIFIED                = 0,
    USER                       = 1,  // 用户账户
    MERCHANT                   = 2,  // 商户结算账户
    MERCHANT_PENDING_SETTLE    = 3,  // 商户待结算账户
    PLATFORM                   = 4,  // 平台损益账户
    TRANSIT_CHANNEL_RECEIVABLE = 5,  // 中间账户渠道应收款
    TRANSIT_CHANNEL_PAYABLE    = 6,  // 中间账户渠道应付款
    TRANSACTION_FEE            = 7,  // 平台手续费账户
    CHARGE_FEE                 = 8,  // 平台服务费账户
    TRANSIT                    = 9,  // 通用中间账户
}

// AccountCategory 账户分类（会计科目）
enum AccountCategory {
    CATEGORY_UNSPECIFIED = 0,
    ASSET                = 1,
    LIABILITY            = 2,
    EQUITY               = 3,
    REVENUE              = 4,
    EXPENSE              = 5,
}

// AccountStatus 账户状态
enum AccountStatus {
    STATUS_UNSPECIFIED = 0,
    DISABLED           = 1,
    ACTIVE             = 2,
    FROZEN             = 3,
}

// BusinessType 记账业务类型
enum BusinessType {
    BUS_UNSPECIFIED = 0,
    TRANSFER        = 1,
    PAYMENT         = 2,
    REFUND          = 3,
    WITHDRAW        = 4,
    DEPOSIT         = 5,
    COMMISSION      = 6,
}

// ExecutionMode 执行模式
enum ExecutionMode {
    MODE_UNSPECIFIED = 0,
    SYNC             = 1,
    ASYNC            = 2,
    BATCH            = 3,
}

// TransactionStatus 交易状态
enum TransactionStatus {
    TX_FAILED     = 0,
    TX_SUCCESS    = 1,
    TX_PROCESSING = 2,
}

// ---- Data model ------------------------------------------------------------

struct Account {
    1: required string          accountNo,             // 账户号 {dbIdx:1d}{globalTableIdx:02d}{userId}-{businessType}
    2: required i64             userId,
    3: required AccountType     accountType,
    4: required AccountCategory category,
    5: required string          currency,
    6: required string          balance,               // 余额（字符串）
    7: required string          frozenBalance,
    8: required string          availableBalance,
    9: required AccountStatus   status,
    10: required i64            version,
    11: required i64            createdAt,             // ms
    12: required i64            updatedAt,             // ms
    13: required i32            accountBusinessType,   // 动态注册值，见文件头注释
}

struct AccountingEntry {
    1: required string accountNo,
    2: required string debitAmount,
    3: required string creditAmount,
    4: optional string description,
}

struct AccountTransaction {
    1: required string             transactionId,
    2: optional string             parentTransactionId,
    3: required string             accountNo,
    4: required string             businessNo,
    5: required BusinessType       businessType,
    6: required string             debitAmount,
    7: required string             creditAmount,
    8: required string             balanceBefore,
    9: required string             balanceAfter,
    10: required string            currency,
    11: required string            transactionDate,
    12: required i64               transactionTime,       // ms
    13: optional string            description,
    14: required TransactionStatus status,
    15: required i32               bookingType,           // 1=实时 2=缓冲
}

struct BalanceSnapshot {
    1: required string accountNo,
    2: required string snapshotDate,
    3: required string beginningBalance,
    4: required string endingBalance,
    5: required string totalDebit,
    6: required string totalCredit,
    7: required i32    transactionCount,
}

// TCC 分支信息
struct TccBranchInfo {
    1: required string tccId,           // = voucherNo
    2: required string branchId,        // = transactionId
    3: required string accountNo,
    4: required string balanceDelta,
    5: required string frozenAmount,
    6: required i32    status,          // 0=TRYING 1=CONFIRMED 2=CANCELLED
    7: required i32    dbIndex,
    8: required i32    tableIndex,
    9: required i64    createdAt,
    10: required i64   updatedAt,
}

// 试算平衡分组汇总（按 category + accountType）
struct TrialBalanceCategorySummary {
    1: required string category,        // ASSET / LIABILITY / ...
    2: required i32    accountType,
    3: required i64    accountCount,
    4: required string sumBeginning,
    5: required string sumEnding,
    6: required string sumDebit,
    7: required string sumCredit,
}

// 业务类型元数据（账户业务类型注册信息）
struct AccountBusinessTypeInfo {
    1: required i32             businessType,     // i32 编码（动态分配）
    2: required string          name,             // 名称 (e.g. "GCASH_RECEIVABLE")
    3: required string          displayName,      // 展示名 (e.g. "Gcash 应收账户")
    4: required AccountType     accountType,      // 关联的账户类型
    5: required AccountCategory category,         // 关联的会计科目
    6: optional string          description,
    7: required bool            isSystem,         // true=系统内置，false=用户创建
    8: required i64             createdAt,
    9: required i64             updatedAt,
}

// ---- Account management ---------------------------------------------------

struct CreateAccountRequest {
    1: required i64            userId,
    2: required AccountType    accountType,
    3: required AccountCategory category,
    4: required string         currency,
    5: optional string         description,
    6: required i32            accountBusinessType,  // 动态注册值
}

struct CreateAccountResponse {
    1: required i32     code,
    2: required string  message,
    3: optional Account account,
}

struct UserIdAndBusinessTypeQuery {
    1: required i64 userId,
    2: required i32 accountBusinessType,
}

// 查询账户：accountNo 或 (userId + businessType) 二选一
struct GetAccountRequest {
    1: optional string                     accountNo,
    2: optional UserIdAndBusinessTypeQuery userIdAndBusinessType,
}

struct GetAccountResponse {
    1: required i32     code,
    2: required string  message,
    3: optional Account account,
}

struct FreezeAccountRequest {
    1: required string accountNo,
    2: optional string reason,
    3: optional string operator,
}
struct FreezeAccountResponse {
    1: required i32    code,
    2: required string message,
}

struct UnfreezeAccountRequest {
    1: required string accountNo,
    2: optional string reason,
    3: optional string operator,
}
struct UnfreezeAccountResponse {
    1: required i32    code,
    2: required string message,
}

// ---- AccountBusinessType 动态管理（admin 侧使用） --------------------------

struct RegisterAccountBusinessTypeRequest {
    1: required string          name,
    2: required string          displayName,
    3: required AccountType     accountType,
    4: required AccountCategory category,
    5: optional string          description,
}

struct RegisterAccountBusinessTypeResponse {
    1: required i32                     code,
    2: required string                  message,
    3: optional AccountBusinessTypeInfo info,
}

struct ListAccountBusinessTypesRequest {
    1: optional AccountType accountType,   // 可选过滤
}

struct ListAccountBusinessTypesResponse {
    1: required i32                           code,
    2: required string                        message,
    3: optional list<AccountBusinessTypeInfo> items,
}

struct GetAccountBusinessTypeRequest {
    1: optional i32    businessType,   // 按 id 查
    2: optional string name,           // 或按 name 查（二选一）
}

struct GetAccountBusinessTypeResponse {
    1: required i32                     code,
    2: required string                  message,
    3: optional AccountBusinessTypeInfo info,
}

// ---- Booking --------------------------------------------------------------

struct DoubleEntryBookingRequest {
    1: required string                businessNo,
    2: required BusinessType          businessType,
    3: required list<AccountingEntry> entries,
    4: required string                currency,
    5: optional string                description,
    6: optional ExecutionMode         mode,
    7: required string                requestId,           // 幂等键（必填）
}

struct DoubleEntryBookingResponse {
    1: required i32          code,
    2: required string       message,
    3: optional string       voucherNo,
    4: optional list<string> transactionIds,
    5: optional bool         async,
}

struct BatchBookingRequest {
    1: required list<DoubleEntryBookingRequest> requests,
    2: optional bool                            parallel,
}

struct BatchBookingResponse {
    1: required i32                              code,
    2: required string                           message,
    3: required i32                              success,
    4: required i32                              failed,
    5: required i32                              total,
    6: optional list<DoubleEntryBookingResponse> results,
}

// 热/冷路径自动路由，预生成 voucherNo/transactionIds 保证严格幂等
struct HybridDoubleEntryBookingRequest {
    1: required string                requestId,
    2: required string                businessNo,
    3: required BusinessType          businessType,
    4: required list<AccountingEntry> entries,
    5: required string                currency,
    6: optional string                description,
}

struct HybridDoubleEntryBookingResponse {
    1: required i32          code,
    2: required string       message,
    3: optional string       voucherNo,
    4: optional list<string> transactionIds,
    5: required bool         idempotentHit,  // true=命中幂等缓存，返回历史结果
}

struct AtomicBatchBookingEntry {
    1: required string                requestId,
    2: required string                businessNo,
    3: required BusinessType          businessType,
    4: required list<AccountingEntry> entries,
    5: required string                currency,
    6: optional string                description,
}

// 全部成功或全部回滚，先持久化再执行
struct AtomicBatchBookingRequest {
    1: required string                        batchRequestId,
    2: required string                        batchBusinessNo,
    3: required list<AtomicBatchBookingEntry> requests,
    4: optional string                        description,
}

struct AtomicBatchBookingItemResult {
    1: required string       requestId,
    2: optional string       voucherNo,
    3: optional list<string> transactionIds,
    4: required i32          code,
    5: optional string       errorMessage,
}

struct AtomicBatchBookingResponse {
    1: required i32                                code,
    2: required string                             message,
    3: optional string                             batchId,
    4: required bool                               allSuccess,
    5: required i32                                total,
    6: optional list<AtomicBatchBookingItemResult> results,
}

// 按 productCode + sceneCode 驱动的高阶资金流水
struct MoneyFlowRequest {
    1: required string             productCode,
    2: required string             sceneCode,
    3: required string             businessNo,
    4: required string             amount,
    5: required string             currency,
    6: required map<string,string> participants,   // role -> accountNo
    7: optional map<string,string> extParams,
}

struct MoneyFlowResponse {
    1: required i32          code,
    2: required string       message,
    3: optional string       voucherNo,
    4: optional list<string> transactionIds,
}

// ---- Query ----------------------------------------------------------------

struct GetTransactionRequest {
    1: optional string transactionId,
    2: optional string businessNo,
    3: optional string accountNo,
    4: optional string startDate,
    5: optional string endDate,
    6: optional i32    pageNum,
    7: optional i32    pageSize,
}

struct GetTransactionResponse {
    1: required i32                      code,
    2: required string                   message,
    3: optional list<AccountTransaction> transactions,
    4: optional i32                      total,
}

struct GetBalanceSnapshotRequest {
    1: required string accountNo,
    2: optional string snapshotDate,
}

struct GetBalanceSnapshotResponse {
    1: required i32             code,
    2: required string          message,
    3: optional BalanceSnapshot snapshot,
}

// ---- Admin ----------------------------------------------------------------

struct TriggerDayCutRequest {
    1: required string cutDate,
}

struct TriggerDayCutResponse {
    1: required i32    code,
    2: required string message,
}

struct AdjustBalanceRequest {
    1: required string accountNo,
    2: required string adjustmentType,
    3: required string amount,
    4: required bool   isIncrease,
    5: required string reason,
    6: required string operator,
    7: optional string approvalNo,
}

struct AdjustBalanceResponse {
    1: required i32    code,
    2: required string message,
    3: optional string transactionId,
    4: optional string voucherNo,
    5: optional string balanceBefore,
    6: optional string balanceAfter,
}

// ---- TCC maintenance ------------------------------------------------------

struct GetTccStatusRequest { 1: required string tccId }
struct GetTccStatusResponse {
    1: required i32                 code,
    2: required string              message,
    3: required string              tccId,
    4: required string              overallStatus,  // CONFIRMED / CANCELLED / TRYING / PARTIAL
    5: required i32                 branchCount,
    6: optional list<TccBranchInfo> branches,
}

struct ListStuckTccRequest {
    1: optional i32 timeoutMinutes,
    2: optional i32 limit,
}
struct ListStuckTccResponse {
    1: required i32                 code,
    2: required string              message,
    3: required i32                 count,
    4: optional list<TccBranchInfo> branches,
}

struct CancelTccRequest { 1: required string tccId }
struct CancelTccResponse {
    1: required i32    code,
    2: required string message,
    3: required string tccId,
    4: required string result,
}

struct CancelTccBranchRequest {
    1: required string branchId,
    2: required string accountNo,
}
struct CancelTccBranchResponse {
    1: required i32    code,
    2: required string message,
    3: required string branchId,
    4: required string result,
}

// ---- Trial balance --------------------------------------------------------

struct RunTrialBalanceRequest {
    1: required string snapshotDate,
}

struct RunTrialBalanceResponse {
    1: required i32                               code,
    2: required string                            message,
    3: required string                            snapshotDate,
    4: optional list<TrialBalanceCategorySummary> summaries,

    5: required string totalDebit,
    6: required string totalCredit,
    7: required bool   isBalanced,
    8: required string imbalance,

    9: required string  assetEndingBalance,
    10: required string liabilityEndingBalance,
    11: required string equityEndingBalance,
    12: required string revenueEndingBalance,
    13: required string expenseEndingBalance,

    14: required bool   isEquationValid,
    15: required string equationDiff,
}


struct DayCutHistoryEntry {
  1: string cut_date
  2: i32 run_id
  3: i32 total_shards
  4: i32 pending
  5: i32 processing
  6: i32 completed
  7: i32 failed
}


struct ListDayCutHistoryResponse {
  1: i32 code
  2: string message
  3: list<DayCutHistoryEntry> entries
}


struct ListDayCutHistoryRequest {
  // 对应 proto 的空 message
}



// ===== 状态枚举（建议强制定义）=====
enum DayCutStatus {
  UNKNOWN = 0,
  PROCESSING = 1,
  SUCCESS = 2,
  FAILED = 3
}

// ===== 核心数据结构 =====
struct DayCutHistoryEntry {
  1: string id
  2: string mid
  3: string business
  4: DayCutStatus status
  5: i64 start_time
  6: i64 end_time
  7: string remark
  8: i64 created_at
  9: i64 updated_at
}

// ===== Request =====
struct ListDayCutHistoryRequest {
  1: optional i32 page_size        // 默认 20，最大 100
  2: optional string page_token    // 游标分页（强烈推荐）

  3: optional i64 start_time
  4: optional i64 end_time

  5: optional DayCutStatus status
  6: optional string mid
  7: optional string business
}

// ===== Response =====
struct ListDayCutHistoryResponse {
  1: i32 code
  2: string message

  3: list<DayCutHistoryEntry> entries

  // 分页信息（关键）
  4: optional string next_page_token
  5: optional bool has_more
}





// ---- Service --------------------------------------------------------------

service AccountingService {
    // Account management
    CreateAccountResponse   CreateAccount  (1: CreateAccountRequest   req),
    GetAccountResponse      GetAccount     (1: GetAccountRequest      req),
    FreezeAccountResponse   FreezeAccount  (1: FreezeAccountRequest   req),
    UnfreezeAccountResponse UnfreezeAccount(1: UnfreezeAccountRequest req),

    // AccountBusinessType dynamic registry (admin-web uses these)
    RegisterAccountBusinessTypeResponse RegisterAccountBusinessType(1: RegisterAccountBusinessTypeRequest req),
    ListAccountBusinessTypesResponse    ListAccountBusinessTypes   (1: ListAccountBusinessTypesRequest    req),
    GetAccountBusinessTypeResponse      GetAccountBusinessType     (1: GetAccountBusinessTypeRequest      req),

    // Booking
    DoubleEntryBookingResponse       DoubleEntryBooking      (1: DoubleEntryBookingRequest       req),
    BatchBookingResponse             BatchBooking            (1: BatchBookingRequest             req),
    HybridDoubleEntryBookingResponse HybridDoubleEntryBooking(1: HybridDoubleEntryBookingRequest req),
    AtomicBatchBookingResponse       AtomicBatchBooking      (1: AtomicBatchBookingRequest       req),
    MoneyFlowResponse                MoneyFlow               (1: MoneyFlowRequest                req),

    // Query
    GetTransactionResponse     GetTransaction    (1: GetTransactionRequest     req),
    GetBalanceSnapshotResponse GetBalanceSnapshot(1: GetBalanceSnapshotRequest req),

    // Admin
    TriggerDayCutResponse TriggerDayCut(1: TriggerDayCutRequest req),
    AdjustBalanceResponse AdjustBalance(1: AdjustBalanceRequest req),

    // TCC maintenance
    GetTccStatusResponse    GetTccStatus   (1: GetTccStatusRequest    req),
    ListStuckTccResponse    ListStuckTcc   (1: ListStuckTccRequest    req),
    CancelTccResponse       CancelTcc      (1: CancelTccRequest       req),
    CancelTccBranchResponse CancelTccBranch(1: CancelTccBranchRequest req),

    // Trial balance
    RunTrialBalanceResponse RunTrialBalance(1: RunTrialBalanceRequest req),
    ListDayCutHistoryResponse  ListDayCutHistory (1: ListDayCutHistoryRequest req),
    ListSnapshotDatesResponse  ListSnapshotDates (1: ListSnapshotDatesRequest req),

}
