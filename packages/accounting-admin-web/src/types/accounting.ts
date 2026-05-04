// ─── 枚举 ──────────────────────────────────────────────────────────────────────

export enum AccountType {
  USER = 1,       // 用户账户
  MERCHANT = 2,   // 商户账户
  MERCHANT_PENDING_SETTLE = 3, // 商户待结算账户
  PLATFORM = 4,   // 平台损益账户
  TRANSIT_CHANNEL_RECEIVABLE = 5, // 中间渠道应收账户
  TRANSIT_CHANNEL_PAYABLE = 6, // 中间渠道应付账户
  TRANSACTION_FEE = 7, // 商户待结算余额账户
  CHARGE_FEE = 8, // 手续费账户
  TRANSIT = 9,    // 中间账户
}
 

export enum AccountCategory {
  ASSET = 1,      // 资产
  LIABILITY = 2,  // 负债
  EQUITY = 3,     // 所有者权益
  REVENUE = 4,    // 收入
  EXPENSE = 5,    // 费用
}

export enum AccountStatus {
  DISABLED = 0,
  ACTIVE = 1,
  FROZEN = 2,
}

export enum AccountBusinessType {
  USER_BALANCE = 1,             // 用户余额账户
  MERCHANT_BALANCE = 2,         // 商户结算账户
  MERCHANT_PENDING_SETTLE = 3,  // 商户待结算余额账户
  PLATFORM_PROFIT = 4,          // 平台利润账户
  TRANSIT_CHANNEL_RECEIVABLE = 5, // 中间账户渠道应收款
  TRANSIT_CHANNEL_PAYABLE = 6,    // 中间账户渠道应付款
  TRANSACTION_FEE = 7,          // 手续费账户
  CHARGE_FEE  = 8,              // 服务费账户
  TRANSIT = 9,                 // 中间账户
}

export enum BusinessType {
  TRANSFER = 1,
  PAYMENT = 2,
  REFUND = 3,
  WITHDRAW = 4,
  DEPOSIT = 5,
  COMMISSION = 6,
}

export enum BookingType {
  NORMAL_BOOKING = 1,
  BUFFER_BOOKING = 2,
}


export enum TransactionOrderStatus {
  PENDING = 0,
  PROCESSING = 1,
  SUCCESS = 2,
  FAILED = 3,
}

export const PartyType = {
  USER: 'user',
  MERCHANT: 'merchant',
  PLATFORM: 'platform',
} as const;
export type PartyType = (typeof PartyType)[keyof typeof PartyType];

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

export interface Account {
  id: number;
  account_no: string;
  user_id: number;
  account_type: AccountType;
  account_category: AccountCategory;
  account_business_type: AccountBusinessType;
  currency: string;
  balance: string;
  frozen_balance: string;
  available_balance: string;
  status: AccountStatus;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface AccountTransaction {
  id: number;
  transaction_id: string;
  parent_transaction_id?: string;
  account_no: string;
  business_no: string;
  business_type: BusinessType;
  debit_amount: string;
  credit_amount: string;
  balance_before: string;
  balance_after: string;
  currency: string;
  transaction_date: string;
  transaction_time: string;
  description?: string;
  status: number;
  booking_type: number; // 1=实时记账 2=缓冲记账
}

export interface AccountBalanceSnapshot {
  id: number;
  account_no: string;
  snapshot_date: string;
  beginning_balance: string;
  ending_balance: string;
  total_debit: string;
  total_credit: string;
  transaction_count: number;
  currency: string;
}

export interface TransactionOrder {
  id: number;
  order_no: string;
  product_code: string;
  event_code: string;
  from_party_id: number;
  from_party_type: string;
  to_party_id: number;
  to_party_type: string;
  amount: string;
  currency: string;
  status: TransactionOrderStatus;
  retry_count: number;
  max_retry_count: number;
  voucher_no?: string;
  error_message?: string;
  created_at: string;
  updated_at: string;
}

// ─── TCC ─────────────────────────────────────────────────────────────────────

export enum TccStatus {
  TRYING    = 0,
  CONFIRMED = 1,
  CANCELLED = 2,
}

export const TCC_STATUS_LABEL: Record<TccStatus, string> = {
  [TccStatus.TRYING]:    'TRYING',
  [TccStatus.CONFIRMED]: 'CONFIRMED',
  [TccStatus.CANCELLED]: 'CANCELLED',
}

export const TCC_STATUS_COLOR: Record<TccStatus, string> = {
  [TccStatus.TRYING]:    'orange',
  [TccStatus.CONFIRMED]: 'green',
  [TccStatus.CANCELLED]: 'default',
}

export interface TccBranch {
  id: number;
  tcc_id: string;
  branch_id: string;
  account_no: string;
  balance_delta: string;
  frozen_amount: string;
  status: TccStatus;
  db_index: number;
  table_index: number;
  created_at: string;
  updated_at: string;
}

export interface TccStatusResponse {
  tcc_id: string;
  overall_status: 'CONFIRMED' | 'CANCELLED' | 'TRYING' | 'PARTIAL';
  branch_count: number;
  branches: TccBranch[];
}

export interface StuckBranchesResponse {
  count: number;
  branches: TccBranch[];
}

// ─── 请求/响应结构 ────────────────────────────────────────────────────────────

export interface ApiResponse<T = unknown> {
  code: number;
  message: string;
  data?: T;
}

// 账户
export interface CreateAccountRequest {
  user_id: number;
  account_type: AccountType;
  category: AccountCategory;
  account_business_type: AccountBusinessType;
  currency?: string;
}

// 复式记账
export interface AccountingEntry {
  account_no: string;
  debit_amount?: string;
  credit_amount?: string;
  description?: string;
}

export interface DoubleEntryBookingRequest {
  business_no: string;
  business_type: BusinessType;
  currency?: string;
  description?: string;
  entries: AccountingEntry[];
}

export interface DoubleEntryBookingResult {
  voucher_no: string;
  transaction_ids: string[];
}

// Transaction API
export interface CreateTransactionRequest {
  order_no: string;
  product_code: string;
  event_code: string;
  from_party_id: number;
  from_party_type: PartyType;
  to_party_id: number;
  to_party_type: PartyType;
  amount: string;
  currency?: string;
  max_retry?: number;
  extra?: Record<string, unknown>;
}

export interface TransactionResult {
  order_no: string;
  status: TransactionOrderStatus;
  voucher_no?: string;
  error_message?: string;
}

// ─── 试算平衡 ─────────────────────────────────────────────────────────────────

export interface TrialBalanceCategorySummary {
  category: string;      // ASSET / LIABILITY / EQUITY / REVENUE / EXPENSE
  type: number;          // AccountType 数字
  account_count: number;
  sum_beginning: string;
  sum_ending: string;
  sum_debit: string;
  sum_credit: string;
}

export interface TrialBalanceResult {
  snapshot_date: string;
  /** 该次试算平衡所针对的币种（admin-web backend 透传请求里的 currency）。 */
  currency?: string;
  total_debit: string;
  total_credit: string;
  is_balanced: boolean;
  imbalance: string;
  asset_ending_balance: string;
  liability_ending_balance: string;
  equity_ending_balance: string;
  revenue_ending_balance: string;
  expense_ending_balance: string;
  is_equation_valid: boolean;
  equation_diff: string;
  summaries: TrialBalanceCategorySummary[];
}

// ─── 交易流水分页响应 ─────────────────────────────────────────────────────────

export interface TransactionListResponse {
  list: AccountTransaction[];
  total: number;
  page: number;
  page_size: number;
}

// ─── 调账响应 ─────────────────────────────────────────────────────────────────

export interface AdjustBalanceRequest {
  account_no: string;
  adjustment_type: string;
  amount: string;
  is_increase: boolean;
  reason: string;
  operator?: string;
  approval_no: string;
}

export interface AdjustBalanceResponse {
  transaction_id: string;
  voucher_no: string;
  balance_before: string;
  balance_after: string;
}

// ─── 日切历史 ─────────────────────────────────────────────────────────────────

export interface DayCutHistoryEntry {
  cut_date: string;
  run_id: number;
  /** 该次 run 的币种过滤；空 = 历史 run（未带 currency filter）。 */
  currency?: string;
  total_shards: number;
  pending: number;
  processing: number;
  completed: number;
  failed: number;
}

export interface DayCutHistoryResponse {
  entries: DayCutHistoryEntry[];
}

// ─── 试算平衡历史日期 ─────────────────────────────────────────────────────────

export interface SnapshotDatesResponse {
  dates: string[];
}

// ─── 热点账户配置 ─────────────────────────────────────────────────────────────

export interface HotAccountConfig {
  id: number;
  account_no: string;
  enabled: boolean;
  description: string;
  created_at: string;
  updated_at: string;
}

export interface HotAccountListResponse {
  items: HotAccountConfig[];
}

// ─── 缓冲记账账户配置 ─────────────────────────────────────────────────────────

export type BufferFlushLevel = 1 | 5 | 10 | 60 | 1440;

export const BUFFER_FLUSH_LEVEL_LABELS: Record<BufferFlushLevel, string> = {
  1:    '1 分钟',
  5:    '5 分钟',
  10:   '10 分钟',
  60:   '1 小时',
  1440: '24 小时',
};

export const BUFFER_FLUSH_LEVELS: BufferFlushLevel[] = [1, 5, 10, 60, 1440];

export interface BufferAccountConfig {
  id: number;
  account_no: string;
  flush_interval_level: BufferFlushLevel;
  enabled: boolean;
  description: string;
  created_at: string;
  updated_at: string;
}

export interface BufferAccountListResponse {
  items: BufferAccountConfig[];
}

// ─── 服务实例 ─────────────────────────────────────────────────────────────────

export interface ServiceInstance {
  instance_id: string;
  host: string;
  http_admin_port: number;
  grpc_port: number;
  last_heartbeat: string;
  started_at: string;
}

export interface ReloadInstanceResult {
  instance_id: string;
  count: number;
  error?: string;
}

export interface ReloadResponse {
  message: string;
  count: number;
  instances: ReloadInstanceResult[];
}

// ─── 系统内部账户 / Business Type Registry ──────────────────────────────────────

/** account_meta.account_type_info 的一条记录（1-9 + 未来新增的 AccountType）。
 *  是否平台内部类型不再靠数字范围硬判断，前端从后端拉此表后按 is_platform 过滤。
 */
export interface AccountTypeInfoRow {
  id: number;
  account_type: string;           // 文本码，如 "PLATFORM_TRANSIT"
  account_type_name: string;      // 中文名
  account_type_desc?: string;
  owner_type: number;             // 数字枚举（对应 account.account_type / AccountBusinessType.AccountType）
  is_platform: number;            // 1=平台内部, 0=业务账户
  balance_direction: string;      // "C" | "D"
  description?: string;
}

/** account_meta.account_business_type_info 的一条记录（channel registry）。
 *  注意：category 不从后端返回——由 account_type 通过 CATEGORY_BY_ACCOUNT_TYPE 派生；
 *  channel_code 列已从 schema 移除（用 business_type_code 区分渠道）。
 */
export interface BusinessTypeInfo {
  id: number;
  business_type: number;
  business_type_code: string;
  account_type: number;
  description: string | null;
  enabled: number;
  created_at?: string;
  updated_at?: string;
}



/** 平台账户 + 对应日切快照（来自 /v1/platform-accounts/snapshots 的 rows） */
export interface PlatformAccountSnapshotRow {
  account: Account;
  snapshot: AccountBalanceSnapshot | null;
}

