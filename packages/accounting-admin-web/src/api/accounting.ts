import { request } from './client';
import type {
  Account,
  AccountBalanceSnapshot,
  AccountBusinessType,
  AccountTypeInfoRow,
  AdjustBalanceRequest,
  AdjustBalanceResponse,
  BufferAccountConfig,
  BufferFlushLevel,
  BusinessTypeInfo,
  PlatformAccountSnapshotRow,
  CreateAccountRequest,
  CreateTransactionRequest,
  DayCutHistoryResponse,
  DoubleEntryBookingRequest,
  DoubleEntryBookingResult,
  HotAccountConfig,
  ReloadResponse,
  ServiceInstance,
  SnapshotDatesResponse,
  StuckBranchesResponse,
  TccStatusResponse,
  TransactionListResponse,
  TransactionResult,
  TrialBalanceResult,
} from '../types/accounting';

// ─── 账户 API ─────────────────────────────────────────────────────────────────

/** 创建账户 */
export function createAccount(data: CreateAccountRequest): Promise<Account> {
  return request({ method: 'POST', url: '/v1/accounts', data });
}

/** 根据账户号查询账户 */
export function getAccount(accountNo: string): Promise<Account> {
  return request({ method: 'GET', url: `/v1/accounts/${accountNo}` });
}

/** 根据 account_no 查询（query 参数） */
export function getAccountByQuery(accountNo: string): Promise<Account> {
  return request({ method: 'GET', url: '/v1/accounts', params: { account_no: accountNo } });
}

/** 根据 userId + businessType 查询账户（返回该 (user_id, business_type) 下全部币种）。
 *  单币种用户会返回 1 条，多币种用户会返回多条。前端负责让用户在列表里选币种。
 *  可选 currency 过滤到单币种。 */
export function getAccountByUserAndBusinessType(
  userId: number,
  accountBusinessType: AccountBusinessType,
  currency?: string,
): Promise<{ accounts: Account[]; count: number }> {
  const params: Record<string, unknown> = { user_id: userId, account_business_type: accountBusinessType };
  if (currency) params.currency = currency;
  return request({ method: 'GET', url: '/v1/accounts', params });
}

// ─── 复式记账 API（内部直接调用）────────────────────────────────────────────

/** 复式记账 */
export function doubleEntryBooking(
  data: DoubleEntryBookingRequest,
): Promise<DoubleEntryBookingResult> {
  return request({ method: 'POST', url: '/v1/bookings', data });
}

// ─── Transaction API（product_code + event_code 驱动）────────────────────────

/** 创建交易（通过规则配置驱动记账） */
export function createTransaction(
  data: CreateTransactionRequest,
): Promise<TransactionResult> {
  return request({ method: 'POST', url: '/v1/transactions', data });
}

/** 重试失败的交易 */
export function retryTransaction(orderNo: string): Promise<TransactionResult> {
  return request({ method: 'POST', url: `/v1/transactions/${orderNo}/retry` });
}

// ─── 余额快照 API ─────────────────────────────────────────────────────────────

/** 查询余额快照（date 不传则返回最新） */
export function getBalanceSnapshot(
  accountNo: string,
  date?: string,
): Promise<AccountBalanceSnapshot> {
  return request({
    method: 'GET',
    url: `/v1/snapshots/${accountNo}`,
    params: date ? { date } : {},
  });
}

// ─── TCC 维护 API ─────────────────────────────────────────────────────────────

/** 查询 TCC 事务所有分支状态 */
export function getTccStatus(tccId: string): Promise<TccStatusResponse> {
  return request({ method: 'GET', url: `/v1/tcc/${encodeURIComponent(tccId)}` });
}

/** 查询超时未完成（TRYING）的 TCC 分支 */
export function listStuckTcc(timeoutMinutes = 5, limit = 50): Promise<StuckBranchesResponse> {
  return request({ method: 'GET', url: '/v1/tcc/stuck', params: { timeout_minutes: timeoutMinutes, limit } });
}

/** 取消 TCC 事务下所有 TRYING 分支 */
export function cancelTcc(tccId: string): Promise<{ tcc_id: string; result: string }> {
  return request({ method: 'POST', url: `/v1/tcc/${encodeURIComponent(tccId)}/cancel` });
}

/** 取消单条 TRYING 分支 */
export function cancelTccBranch(branchId: string, accountNo: string): Promise<{ branch_id: string; result: string }> {
  return request({
    method: 'POST',
    url: `/v1/tcc/branches/${encodeURIComponent(branchId)}/cancel`,
    params: { account_no: accountNo },
  });
}

// ─── 日切 API ─────────────────────────────────────────────────────────────────

/** 触发日切（也可作为重跑入口：会将指定日期所有分片状态重置为 PENDING 后重新执行） */
/** 触发日切。currency 必填——日切按币种独立执行，不接受跨币种汇总。 */
export function triggerDayCut(cutDate: string, currency: string): Promise<{ cut_date: string; currency: string; status: string }> {
  return request({ method: 'POST', url: '/v1/day-cut', data: { cut_date: cutDate, currency } });
}

/** 查询历史日切记录（所有日期聚合状态） */
export function getDayCutHistory(): Promise<DayCutHistoryResponse> {
  return request({ method: 'GET', url: '/v1/day-cut/history' });
}

// ─── 交易流水 API ─────────────────────────────────────────────────────────────

export interface TransactionListParams {
  account_no?: string;
  business_no?: string;
  transaction_id?: string;
  start_date?: string;
  end_date?: string;
  page?: number;
  page_size?: number;
}

/** 查询交易流水列表 */
export function getTransactionList(params: TransactionListParams): Promise<TransactionListResponse> {
  return request({ method: 'GET', url: '/v1/transactions', params });
}

// ─── 调账 API ─────────────────────────────────────────────────────────────────

/** 提交调账申请 */
export function adjustBalance(data: AdjustBalanceRequest): Promise<AdjustBalanceResponse> {
  return request({ method: 'POST', url: '/v1/adjustment', data });
}

// ─── 试算平衡 API ─────────────────────────────────────────────────────────────

/** 执行试算平衡。currency 必填——按币种独立执行。 */
export function runTrialBalance(snapshotDate: string, currency: string): Promise<TrialBalanceResult> {
  return request({ method: 'POST', url: '/v1/trial-balance', data: { snapshot_date: snapshotDate, currency } });
}

/** 查询所有有快照数据的日期列表 */
export function listSnapshotDates(): Promise<SnapshotDatesResponse> {
  return request({ method: 'GET', url: '/v1/trial-balance/dates' });
}

// ─── 健康检查 ─────────────────────────────────────────────────────────────────

export function healthCheck(): Promise<{ status: string }> {
  return request({ method: 'GET', url: '/health' });
}

// ─── 热点账户管理 API ─────────────────────────────────────────────────────────

/** 查询所有热点账户配置 */
export function listHotAccounts(): Promise<HotAccountConfig[]> {
  return request({ method: 'GET', url: '/v1/hot-accounts' });
}

/** 新增热点账户配置 */
export function createHotAccount(data: { account_no: string; description?: string }): Promise<HotAccountConfig> {
  return request({ method: 'POST', url: '/v1/hot-accounts', data });
}

/** 更新热点账户配置 */
export function updateHotAccount(id: number, data: { enabled: boolean; description?: string }): Promise<void> {
  return request({ method: 'PUT', url: `/v1/hot-accounts/${id}`, data });
}

/** 删除热点账户配置 */
export function deleteHotAccount(id: number): Promise<void> {
  return request({ method: 'DELETE', url: `/v1/hot-accounts/${id}` });
}

/** 热重载热点账户白名单（全部实例） */
export function reloadHotAccounts(): Promise<ReloadResponse> {
  return request({ method: 'POST', url: '/v1/hot-accounts/reload' });
}

// ─── 系统内部账户（平台账户）管理 API ─────────────────────────────────────────

/** 平台账户类型枚举。每个 AccountType 对应一个确定的 Category(在前端/后端各自派生)。 */
export const PLATFORM_ACCOUNT_TYPES = [
  { value: 4, label: '平台损益账户 (Platform, EQUITY)' },
  { value: 5, label: '中间账户-渠道应收款 (Transit Channel Receivable, ASSET)' },
  { value: 6, label: '中间账户-渠道应付款 (Transit Channel Payable, LIABILITY)' },
  { value: 7, label: '平台手续费账户 (Transaction Fee, REVENUE)' },
  { value: 8, label: '平台服务费账户 (Charge Fee, REVENUE)' },
  { value: 9, label: '平台中间账户 (Transit, LIABILITY)' },
] as const;

/** 业务账户类型（对应 account_type_info.is_platform = 0）*/
export const BUSINESS_ACCOUNT_TYPES = [
  { value: 1, label: '用户账户 (User, LIABILITY)' },
  { value: 2, label: '商户账户 (Merchant, LIABILITY)' },
  { value: 3, label: '商户待结算账户 (Merchant Pending Settle, LIABILITY)' },
] as const;

/** 全部 AccountType（含业务账户 1-3 + 平台账户 4-9），用于下拉显示。*/
export const ALL_ACCOUNT_TYPES = [
  ...BUSINESS_ACCOUNT_TYPES,
  ...PLATFORM_ACCOUNT_TYPES,
] as const;

/** 数值集合：判断某 account_type 是否平台类型。
 *  默认用硬编码 fallback（4-9），app 启动后 loadAccountTypeRegistry()
 *  会替换成从 /v1/account-types 拉到的真实 is_platform 映射，
 *  以支持未来在 DB 里新增 AccountType 而无需改前端代码。
 */
export let PLATFORM_ACCOUNT_TYPE_VALUES = new Set<number>(
  PLATFORM_ACCOUNT_TYPES.map(t => t.value),
);
export let BUSINESS_ACCOUNT_TYPE_VALUES = new Set<number>(
  BUSINESS_ACCOUNT_TYPES.map(t => t.value),
);
export const isPlatformAccountType = (at: number) => PLATFORM_ACCOUNT_TYPE_VALUES.has(at);

/** 从后端拉 account_type_info，用 is_platform 覆盖本地 fallback 常量。
 *  由顶层 App（或任何入口）在挂载时调一次；失败则保留 fallback。
 */
export async function loadAccountTypeRegistry(): Promise<AccountTypeInfoRow[]> {
  const rows = await request<AccountTypeInfoRow[]>({ method: 'GET', url: '/v1/account-types' });
  const platform = new Set<number>();
  const business = new Set<number>();
  for (const r of rows) {
    if (r.is_platform === 1) platform.add(r.owner_type);
    else business.add(r.owner_type);
  }
  if (platform.size > 0) PLATFORM_ACCOUNT_TYPE_VALUES = platform;
  if (business.size > 0) BUSINESS_ACCOUNT_TYPE_VALUES = business;
  return rows;
}

/** 前端 AccountType → AccountCategory 数字枚举派生（与后端 CategoryForAccountType 保持一致）。
 *  值必须是 enum AccountCategory 的数字（1..5），后端 gRPC CreateAccountRequest.category 是 int32。
 *  展示字符串用 CATEGORY_LABEL[n] 映射。
 */
export const CATEGORY_BY_ACCOUNT_TYPE: Record<number, number> = {
  1: 2, 2: 2, 3: 2,       // User / Merchant / MerchantPendingSettle → LIABILITY (2)
  4: 3,                   // Platform → EQUITY (3)
  5: 1,                   // TransitChannelReceivable → ASSET (1)
  6: 2,                   // TransitChannelPayable → LIABILITY (2)
  7: 4,                   // TransactionFee → REVENUE (4)
  8: 4,                   // ChargeFee → REVENUE (4)
  9: 2,                   // Transit → LIABILITY (2)
};

/** Category 数字 → 显示字符串 */
export const CATEGORY_LABEL: Record<number, string> = {
  1: 'ASSET',
  2: 'LIABILITY',
  3: 'EQUITY',
  4: 'REVENUE',
  5: 'EXPENSE',
};

/** AccountType 数字 → 中文名（显示用）*/
export const ACCOUNT_TYPE_LABEL: Record<number, string> = {
  1: '用户账户', 2: '商户账户', 3: '商户待结算账户',
  4: '平台损益账户', 5: '中间账户-渠道应收款', 6: '中间账户-渠道应付款',
  7: '平台手续费账户', 8: '平台服务费账户', 9: '平台中间账户',
};

export interface CreatePlatformAccountReq {
  reserved_id: number;
  account_type: number;
  currency?: string;
}

/**
 * Fleet 创建:要求 business_type 必须已通过 RegisterBusinessType 登记。
 * account_type 必须与 registry 中一致,否则服务端拒绝。
 */
export interface CreatePlatformFleetReq {
  account_type: number;
  channel_business_type: number;
  currency?: string;
}

export interface CreatePlatformFleetResp {
  account_type: number;
  business_type_code: string;
  channel_business_type: number;
  currency: string;
  created: number;
  accounts: Account[];
}

/** 注册 business_type（只写 registry,不建账户）。*/
export interface RegisterBusinessTypeReq {
  account_type: number;
  business_type_code: string;
  description?: string;
  business_type?: number; // 0 或省略 → 服务端自动分配 [101, 999]
}

export interface PlatformBalancesResp {
  business_type: number;
  count: number;
  accounts: Account[];
}

export interface PlatformSnapshotsResp {
  business_type: number;
  date: string;
  count: number;
  rows: PlatformAccountSnapshotRow[];
}

/** 单点创建系统账户（一个分片） */
export function createPlatformAccount(data: CreatePlatformAccountReq): Promise<Account> {
  return request({ method: 'POST', url: '/v1/platform-accounts', data });
}

/** 批量为一个渠道创建 100 个系统账户（需先 registerBusinessType） */
export function createPlatformAccountFleet(data: CreatePlatformFleetReq): Promise<CreatePlatformFleetResp> {
  return request({ method: 'POST', url: '/v1/platform-accounts/fleet', data });
}

/** 列出 account_business_type_info 所有记录（channel registry） */
export function listBusinessTypes(): Promise<BusinessTypeInfo[]> {
  return request({ method: 'GET', url: '/v1/business-types' });
}

/** 注册一条 business_type（只写 registry,不建账户） */
export function registerBusinessType(data: RegisterBusinessTypeReq): Promise<BusinessTypeInfo> {
  return request({ method: 'POST', url: '/v1/business-types', data });
}

/** 查询 (business_type, currency) 的 100 个平台账户 + 当前余额。currency 必填。 */
export function getPlatformBalances(businessType: number, currency: string): Promise<PlatformBalancesResp> {
  return request({
    method: 'GET',
    url: '/v1/platform-accounts/balances',
    params: { business_type: businessType, currency },
  });
}

// ─── TCC 归档 ──────────────────────────────────────────────────────────────

export interface TccArchiveConfig {
  interval_seconds: number;
  retention_days: number;
  batch_size: number;
}

export interface TccArchiveRunResp {
  status: string;
  message: string;
}

export function getTccArchiveConfig(): Promise<TccArchiveConfig> {
  return request({ method: 'GET', url: '/v1/tcc-archive/config' });
}

export function runTccArchiveNow(): Promise<TccArchiveRunResp> {
  return request({ method: 'POST', url: '/v1/tcc-archive/run' });
}

/** 查询 (business_type, currency) 在某 cut_date 的 100 个平台账户快照。currency 必填。 */
export function getPlatformSnapshots(businessType: number, date: string, currency: string): Promise<PlatformSnapshotsResp> {
  return request({
    method: 'GET',
    url: '/v1/platform-accounts/snapshots',
    params: { business_type: businessType, date, currency },
  });
}

// ─── 缓冲记账账户管理 API ─────────────────────────────────────────────────────

/** 查询所有缓冲记账账户配置 */
export function listBufferAccounts(): Promise<BufferAccountConfig[]> {
  return request({ method: 'GET', url: '/v1/buffer-accounts' });
}

/** 新增缓冲记账账户配置 */
export function createBufferAccount(data: {
  account_no: string;
  flush_interval_level: BufferFlushLevel;
  description?: string;
}): Promise<BufferAccountConfig> {
  return request({ method: 'POST', url: '/v1/buffer-accounts', data });
}

/** 更新缓冲记账账户配置 */
export function updateBufferAccount(id: number, data: {
  enabled: boolean;
  flush_interval_level: BufferFlushLevel;
  description?: string;
}): Promise<void> {
  return request({ method: 'PUT', url: `/v1/buffer-accounts/${id}`, data });
}

/** 删除缓冲记账账户配置 */
export function deleteBufferAccount(id: number): Promise<void> {
  return request({ method: 'DELETE', url: `/v1/buffer-accounts/${id}` });
}

/** 热重载缓冲记账账户配置 */
export function reloadBufferAccounts(): Promise<ReloadResponse> {
  return request({ method: 'POST', url: '/v1/buffer-accounts/reload' });
}

// ─── 服务实例 API ─────────────────────────────────────────────────────────────

/** 获取所有活跃服务实例列表 */
export function listServiceInstances(): Promise<ServiceInstance[]> {
  return request({ method: 'GET', url: '/v1/service-instances' });
}

/** 热重载热点账户白名单（指定实例或全部） */
export function reloadHotAccountsToInstance(instanceId?: string): Promise<ReloadResponse> {
  const url = instanceId
    ? `/v1/hot-accounts/reload?instance_id=${encodeURIComponent(instanceId)}`
    : '/v1/hot-accounts/reload';
  return request({ method: 'POST', url });
}

/** 热重载缓冲记账配置（指定实例或全部） */
export function reloadBufferAccountsToInstance(instanceId?: string): Promise<ReloadResponse> {
  const url = instanceId
    ? `/v1/buffer-accounts/reload?instance_id=${encodeURIComponent(instanceId)}`
    : '/v1/buffer-accounts/reload';
  return request({ method: 'POST', url });
}

// ─── 系统通用配置中心（meta DB / system_config）──────────────────────────────

export interface SystemConfigItem {
  config_key: string;
  value_json: string;        // 始终是合法 JSON：可为 "abc" / 123 / [...] / {...}
  value_type: string;        // string / int / bool / json，给前端编辑器 hint
  description: string;
  updated_by?: string;
  updated_at?: string;
  created_at?: string;
}

/** 列出所有系统配置 */
export function listSystemConfig(): Promise<SystemConfigItem[]> {
  return request({ method: 'GET', url: '/v1/config' });
}

/** 新增 / 更新一条配置（自动 fanout reload 到所有实例） */
export function upsertSystemConfig(
  config_key: string,
  value_json: string,
  value_type: string,
  description: string,
  updated_by?: string,
): Promise<unknown> {
  return request({
    method: 'POST',
    url: '/v1/config',
    data: { config_key, value_json, value_type, description, updated_by },
  });
}

/** 删除配置 */
export function deleteSystemConfig(key: string): Promise<unknown> {
  return request({ method: 'DELETE', url: `/v1/config/${encodeURIComponent(key)}` });
}

/** 手动触发所有实例 reload（一般 upsert/delete 已自动 fanout，按钮兜底用） */
export function reloadSystemConfig(): Promise<unknown> {
  return request({ method: 'POST', url: '/v1/config/reload' });
}

// ─── TCC CONFIRMING 半挂起立即恢复（loadtest 后不等 5min 阈值）────────────────

export interface TccRetryConfirmResp {
  recovered: number;
  threshold_seconds: number;
  instance_id: string;
  message: string;
}

/** 立即重试所有 phase=CONFIRMING 且 updated_at < thresholdSeconds 的 TCC。
 *  thresholdSeconds = 0 → 不管多新的都重试（适合 loadtest 结束后马上调）。
 */
export function tccRetryConfirmNow(thresholdSeconds = 0): Promise<TccRetryConfirmResp> {
  return request({
    method: 'POST',
    url: '/v1/tcc/retry-confirm',
    data: { threshold_seconds: thresholdSeconds },
  });
}

// ─── Redis 热账户重建（disaster recovery）─────────────────────────────────────

export interface RebuildEntry {
  account_no: string;
  balance_before: string;   // "" 表示 cache miss
  balance_after: string;    // 目标余额（int64 storage units, 字符串）
  source: string;           // "account" | "transaction_journal"
  journal_cutoff?: string;  // 仅 source=transaction_journal 时
  skipped: boolean;
  reason?: string;
}

export interface RebuildReport {
  as_of: string;            // RFC3339 时间戳，"" = now
  dry_run: boolean;
  total: number;
  updated: number;
  skipped: number;
  failed: number;
  duration: string;
  entries?: RebuildEntry[];
}

export interface RebuildRequest {
  /** "5m" / "1h" 相对时长，或 RFC3339 时间戳；空 = 用 account 表当前余额 */
  as_of?: string;
  /** 指定账户列表；空 = 重建全部启用的热账户 */
  account_nos?: string[];
  /** true 时只算 diff 不写 Redis */
  dry_run?: boolean;
}

/** 触发 Redis 热账户重建（POST /v1/redis/rebuild → gRPC RebuildHotAccounts）。 */
export function rebuildHotAccounts(req: RebuildRequest): Promise<RebuildReport> {
  return request({ method: 'POST', url: '/v1/redis/rebuild', data: req });
}

// ─── 轮换账户管理（rotation feature）API ───────────────────────────────────

import type {
  RotationLogicalAccountsResponse,
  RotationInstanceHistoryView,
  RotationInstanceDetail,
  RotationManualOpRequest,
  RotationManualOpResponse,
} from '../types/accounting';

/**
 * 列出所有 logical_account 的当前 active 状态。
 * @param prefix 过滤 logical_account_key 前缀（如 "channel-payable:"）
 * @param limit 最多返回多少行（默认 200）
 */
export function listRotationLogicalAccounts(
  prefix?: string,
  limit?: number,
): Promise<RotationLogicalAccountsResponse> {
  const params: Record<string, unknown> = {};
  if (prefix) params.prefix = prefix;
  if (limit) params.limit = limit;
  return request({ method: 'GET', url: '/v1/rotation/logical-accounts', params });
}

/** 查询某 logical_account 下所有 instance 历史 + 余额。 */
export function getRotationInstanceHistory(
  logicalAccountKey: string,
): Promise<RotationInstanceHistoryView> {
  return request({
    method: 'GET',
    url: '/v1/rotation/instance-history',
    params: { logical_account_key: logicalAccountKey },
  });
}

/** 查询单 instance 详情。 */
export function getRotationInstanceDetail(accountNo: string): Promise<RotationInstanceDetail> {
  return request({
    method: 'GET',
    url: '/v1/rotation/instance-detail',
    params: { account_no: accountNo },
  });
}

/** 立即切换：当前 active → draining，provisioned → active。需先 provision。 */
export function rotationManualSwitch(
  req: RotationManualOpRequest,
): Promise<RotationManualOpResponse> {
  return request({ method: 'POST', url: '/v1/rotation/manual-switch', data: req });
}

/** 立即预创建下一期 provisioned instance（不切换）。 */
export function rotationManualProvision(
  req: RotationManualOpRequest,
): Promise<RotationManualOpResponse> {
  return request({ method: 'POST', url: '/v1/rotation/manual-provision', data: req });
}
