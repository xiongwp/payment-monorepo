// src/api/risk.ts —— 风控运营平台 API client
//
// 后端 BFF 把请求代理到 risk-manage 的 admin HTTP 端点，对前端是个普通的
// `/api/risk/*` JSON 接口。
import { request } from './client'

// ── Reviews（人工审核队列）─────────────────────────────────────────

export type ReviewStatus = 'pending' | 'in_review' | 'escalated' | 'approved' | 'rejected'
export type ReviewAction = 'approve' | 'reject'

export interface ReviewNote {
  actor: string
  body: string
  created_at: string
}

export interface ReviewItem {
  id: string                 // = audit.decision_id
  merchant_id: string
  customer_id: string
  payment_intent_id: string
  amount: number
  currency: string
  risk_score: number
  reasons: string[]
  status: ReviewStatus
  created_at: string
  sla_deadline?: string
  assigned_to?: string
  assigned_at?: string
  escalate_level?: number
  decided_at?: string
  decided_by?: string
  decide_reason?: string
  notes?: ReviewNote[]
}

export const listReviews = (params: { status?: ReviewStatus; limit?: number; offset?: number }) =>
  request<{ items: ReviewItem[]; total: number }>({
    url: '/risk/reviews', method: 'GET', params,
  })

export const getReview = (id: string) =>
  request<ReviewItem>({ url: `/risk/reviews/${id}`, method: 'GET' })

export const decideReview = (body: {
  id: string; action: ReviewAction; actor: string; reason?: string;
}) => request<ReviewItem>({ url: '/risk/reviews/decide', method: 'POST', data: body })

// 批量审核（卡测试攻击 / 同模式 fraud 一次性 reject N 条）
export interface BulkDecideResult {
  total: number
  success: number
  not_pending: number
  failed: number
  results: { id: string; status: 'success' | 'not_pending' | 'error'; error?: string }[]
}

export const decideReviewBulk = (body: {
  ids: string[]; action: ReviewAction; actor: string; reason?: string;
}) => request<BulkDecideResult>({ url: '/risk/reviews/decide-bulk', method: 'POST', data: body })

// ── Case workbench（claim / release / escalate / note / queues） ───

export const claimReview = (body: { id: string; actor: string }) =>
  request<ReviewItem>({ url: '/risk/reviews/claim', method: 'POST', data: body })

export const releaseReview = (body: { id: string; actor: string }) =>
  request<ReviewItem>({ url: '/risk/reviews/release', method: 'POST', data: body })

export const escalateReview = (body: { id: string; actor: string; reason?: string }) =>
  request<ReviewItem>({ url: '/risk/reviews/escalate', method: 'POST', data: body })

export const addReviewNote = (body: { id: string; actor: string; body: string }) =>
  request<ReviewItem>({ url: '/risk/reviews/note', method: 'POST', data: body })

export const myAssignedReviews = (params: { actor: string; status?: ReviewStatus; limit?: number }) =>
  request<ReviewItem[]>({ url: '/risk/reviews/by-assignee', method: 'GET', params })

export const overdueReviews = (params: { limit?: number }) =>
  request<ReviewItem[]>({ url: '/risk/reviews/overdue', method: 'GET', params })

// ── Dashboard 聚合 ─────────────────────────────────────────────────

export interface DashboardSummary {
  now: string
  rule_count: number
  queue: {
    pending: number
    in_review: number
    escalated: number
    approved: number
    rejected: number
    overdue: number
  }
  champion_model?: string
  challenger_models?: string[]
  decisions_by_verdict: Record<string, number>
  decisions_sample_size: number
}

export const dashboardSummary = () =>
  request<DashboardSummary>({ url: '/risk/dashboard/summary', method: 'GET' })

// ── 召回 / 准确率指标 ─────────────────────────────────────────

export interface RecallStats {
  window_days: number
  labeled_samples: number
  actual_fraud: number
  actual_legit: number
  true_positives: number
  false_positives: number
  true_negatives: number
  false_negatives: number
  precision_at_block: number
  recall_at_block: number
  false_positive_rate: number
  f1_score: number
}

export const recallStats = () =>
  request<RecallStats>({ url: '/risk/dashboard/recall', method: 'GET' })

// ── 规则 mode 切换（enforce ↔ shadow，规则预测试） ───────────

export const setRuleMode = (body: { id: string; shadow: boolean }) =>
  request<{ id: string; mode: string }>({
    url: '/risk/rules/mode', method: 'POST', data: body,
  })

// ── 规则编辑（list / update / delete / audit log） ────────────
// engine.RuleDef 一对一镜像。config 是 json string（运营在 UI 直接编辑 raw
// JSON，避免 schema 漂移）；后端跑 BuildRule 校验，UI 把 400 错误展示出来。

export interface RolloutConfig {
  enable_pct?: number
  bucket_seed?: string
}

export interface RuleDef {
  id: string
  name: string
  type: string
  decision: string
  enabled: boolean
  mode: string          // "enforce" | "shadow"
  weight: number
  config_json?: string  // raw JSON，运营 UI 直接编辑
  rollout?: RolloutConfig
}

export const listRules = () =>
  request<{ items: RuleDef[] }>({ url: '/risk/rules/list', method: 'GET' })

export const updateRule = (rule: RuleDef) =>
  request<{ id: string; action: 'create' | 'update' }>({
    url: '/risk/rules/update', method: 'POST', data: rule,
  })

export const deleteRule = (body: { id: string; reason?: string }) =>
  request<{ id: string; deleted: boolean }>({
    url: '/risk/rules/delete', method: 'POST', data: body,
  })

export interface RuleSimulateResult {
  sample: number
  hits: number
  hit_rate: number
  would_newly_block: number
  would_keep_block: number
  hits_with_outcome: number
  hits_true_fraud: number
  estimated_precision: number
  notes?: string[]
}

export const simulateRule = (rule: RuleDef, sample?: number) =>
  request<RuleSimulateResult>({
    url: '/risk/rules/simulate',
    method: 'POST',
    data: rule,
    params: sample ? { sample } : undefined,
  })

// ── 规则集 YAML 导入 / 导出（跨环境推送） ─────────────

export interface RuleDiffEntry {
  rule_id: string
  action: 'create' | 'update' | 'delete' | 'unchanged'
  before?: unknown
  after?: unknown
}

export interface RuleImportResult {
  dry_run: boolean
  imported: number
  diff: RuleDiffEntry[]
}

// exportRulesYAML 直接拿 raw YAML text（让用户复制 / 下载）。client.ts 用
// baseURL='/api'，nginx 代理到 BFF；这里直接走 fetch + 同 baseURL 拿 raw text。
export const exportRulesYAML = async (): Promise<string> => {
  const r = await fetch('/api/risk/rules/export', { credentials: 'include' })
  if (!r.ok) throw new Error(`export failed: HTTP ${r.status}`)
  return r.text()
}

export const importRulesYAML = (yaml: string, dryRun: boolean) =>
  request<RuleImportResult>({
    url: '/risk/rules/import',
    method: 'POST',
    data: yaml,
    headers: { 'Content-Type': 'text/yaml' },
    params: { dry_run: dryRun ? 'true' : 'false' },
  })

export interface RuleAuditEntry {
  occurred_at: string
  action: string         // "create" | "update" | "delete" | "mode_change" | "reload"
  actor: string
  rule_id?: string
  before?: unknown
  after?: unknown
  reason?: string
  metadata?: Record<string, unknown>
}

export const listRuleAudit = (params: { rule_id?: string; limit?: number }) =>
  request<{ items: RuleAuditEntry[] }>({
    url: '/risk/rules/audit', method: 'GET', params,
  })

// ── 规则 KPI（silence / precision / ROI） ─────────────────────

export interface RuleInsight {
  rule_id: string
  last_hit_at: string         // RFC3339；从未命中时是零值时间
  days_since_hit: number      // -1 = 从未命中
  is_silent: boolean
  hits_in_window: number
  precision: number           // 0 = label 不足 5 条，没算
  label_count: number
  fraud_count: number
  roi: number                 // hits × precision
}

export const ruleInsights = () =>
  request<{ silence_window_days: number; rules: RuleInsight[] }>({
    url: '/risk/rules/insights', method: 'GET',
  })

// ── 规则 co-fire 矩阵（找冗余 + 冲突） ──────────────────

export interface RuleOverlapPair {
  rule_a: string
  rule_b: string
  hits_a: number
  hits_b: number
  both: number
  jaccard: number
  a_implies_b: number  // P(B | A)
  b_implies_a: number  // P(A | B)
  verdict_a?: string
  verdict_b?: string
  is_conflict: boolean
}

export const ruleOverlap = (params: { min_both?: number }) =>
  request<{ min_both: number; sample: number; pairs: RuleOverlapPair[] }>({
    url: '/risk/rules/overlap', method: 'GET', params,
  })

// ── Cohort 分析（按商户 / 国家 / 支付方式拆分） ─────────────

export type CohortGroupBy = 'merchant_id' | 'country' | 'payment_method'

export interface CohortStats {
  key: string
  total: number
  block: number
  allow: number
  block_rate: number
  labeled_total: number
  actual_fraud: number
  actual_fraud_rate: number
  blocked_fraud: number
  blocked_legit: number
  precision_at_block: number
}

export const dashboardCohort = (params: { group_by?: CohortGroupBy; min_total?: number }) =>
  request<{
    group_by: string;
    min_total: number;
    sample: number;
    cohorts: CohortStats[];
  }>({ url: '/risk/dashboard/cohort', method: 'GET', params })

export type CohortBucket = 'hour' | 'day' | 'week'

export interface CohortTimeBucket {
  bucket_start: string
  total: number
  block: number
  block_rate: number
  labeled_total: number
  actual_fraud: number
  actual_fraud_rate: number
}

export const dashboardCohortTimeseries = (params: {
  group_by?: CohortGroupBy; key?: string; bucket?: CohortBucket
}) =>
  request<{
    group_by: string;
    key: string;
    bucket: CohortBucket;
    sample: number;
    series: CohortTimeBucket[];
  }>({ url: '/risk/dashboard/cohort/timeseries', method: 'GET', params })

// ── ML A/B 显著性检验（champion vs challenger） ───────────────

export type ABRecommendation = 'promote' | 'hold' | 'drop'

export interface ChallengerReport {
  name: string
  labeled_samples: number
  champion_auc: number
  challenger_auc: number
  auc_diff: number
  ci_low: number
  ci_high: number
  recommendation: ABRecommendation
  recommend_reason: string
}

export const mlscoreABTest = (params: { min_labeled?: number; bootstrap?: number }) =>
  request<{ min_labeled: number; bootstrap: number; report: ChallengerReport[] }>({
    url: '/risk/mlscore/abtest', method: 'GET', params,
  })

// ── Challenger 管理（注册 / 列表 / promote / drop） ──────────

export interface ChallengerListResp {
  scoring_disabled: boolean
  champion?: string
  challengers?: { name: string; model_ver?: string }[]
}

// Logistic 模型权重（跟 mlscore.FeatureWeights 一一对应）
export interface FeatureWeights {
  HighAmount: number
  IPProxy: number
  IPVPN: number
  IPDataCenter: number
  IPCountryMismatch: number
  NoFingerprint: number
  HeadlessRenderer: number
  LowConcurrency: number
  RapidCheckout: number
  NoMouseEntropy: number
  BotTypingRhythm: number
  NoKeystrokes: number
  HighRiskCountry: number
}

export interface ChallengerRegisterReq {
  name: string
  model_ver?: string
  intercept: number
  weights: FeatureWeights
  platt_a?: number
  platt_b?: number
}

export const listChallengers = () =>
  request<ChallengerListResp>({ url: '/risk/mlscore/challengers', method: 'GET' })

export const registerChallenger = (body: ChallengerRegisterReq) =>
  request<{ status: string; name: string }>({
    url: '/risk/mlscore/challengers/register', method: 'POST', data: body,
  })

export const promoteChallenger = (name: string) =>
  request<{ status: string; champion: string; demoted: string }>({
    url: '/risk/mlscore/challengers/promote', method: 'POST', data: { name },
  })

export const dropChallenger = (name: string) =>
  request<{ status: string }>({
    url: '/risk/mlscore/challengers/drop', method: 'POST', data: { name },
  })

// ── ML 降级开关（运营紧急工具） ────────────────────────────

export interface MLOverrideStatus {
  disabled: boolean
  force_score: number
  reason: string
  set_at: string
  set_by: string
  is_active: boolean
}

export const getMLOverride = () =>
  request<MLOverrideStatus>({ url: '/risk/mlscore/override', method: 'GET' })

export const setMLOverride = (body: {
  disabled: boolean; force_score: number; reason: string
}) => request<{ status: string }>({
  url: '/risk/mlscore/override/set', method: 'POST', data: body,
})

export const clearMLOverride = () =>
  request<{ status: string }>({
    url: '/risk/mlscore/override/clear', method: 'POST',
  })

// ── Outcomes（反馈闭环）────────────────────────────────────────────

export type OutcomeSource = 'review_human' | 'dispute' | 'merchant_confirm'

export interface Outcome {
  decision_id: string
  source: OutcomeSource
  is_fraud: boolean
  at: string
  actor?: string
  notes?: string
}

export const recentOutcomes = (limit = 100) =>
  request<{ items: Outcome[]; total: number }>({
    url: '/risk/outcomes/recent', method: 'GET', params: { limit },
  })

export const getOutcomes = (decisionID: string) =>
  request<Outcome[]>({ url: `/risk/outcomes/${decisionID}`, method: 'GET' })

export const recordOutcome = (body: {
  decision_id: string; source: OutcomeSource; is_fraud: boolean;
  actor?: string; notes?: string;
}) => request<unknown>({ url: '/risk/outcomes', method: 'POST', data: body })

// ── Decisions（审计日志）───────────────────────────────────────────

export interface DecisionRow {
  decision_id: string
  occurred_at: string
  verdict: 'ALLOW' | 'REVIEW' | 'DENY'
  risk_score: number
  risk_level: string
  hit_rules: string[]
  eval_duration_ms: number
}

// ── 单笔决策解释（再跑一遍当前规则集） ─────────────────

export interface DecisionHit {
  rule_id: string
  rule_name: string
  decision: string
  detail: string
}

export interface ExplainResp {
  decision_id: string
  original: {
    verdict: string
    risk_score: number
    hits: DecisionHit[]
    occurred_at: string
  }
  replayed: {
    verdict: string
    risk_score: number
    hits: DecisionHit[]
    shadow_hits?: DecisionHit[]
  }
  input: {
    payment_intent_id?: string
    merchant_id?: string
    customer_id?: string
    amount?: number
    currency?: string
    payment_method?: string
    country?: string
    ip_address?: string
    device_id?: string
    metadata?: Record<string, string>
  }
}

export const explainDecision = (decisionID: string) =>
  request<ExplainResp>({
    url: '/risk/decisions/explain', method: 'POST', data: { decision_id: decisionID },
  })

// 完整审计行（包含 input snapshot）；search 返回的元素
export interface AuditRow {
  decision_id: string
  occurred_at: string
  verdict: string
  risk_score: number
  risk_level: string
  hits?: { rule_id: string; rule_name: string; decision: string; detail: string }[]
  shadow_hits?: { rule_id: string; rule_name: string; decision: string; detail: string }[]
  ml_score?: number
  ml_model_ver?: string
  eval_duration_ms?: number
  input?: {
    payment_intent_id?: string
    merchant_id?: string
    customer_id?: string
    amount?: number
    currency?: string
    payment_method?: string
    country?: string
    ip_address?: string
    device_id?: string
  }
}

export const searchDecisions = (params: {
  merchant_id?: string;
  customer_id?: string;
  ip?: string;
  verdict?: string;
  since?: string;
  until?: string;
  limit?: number;
}) => request<{ items: AuditRow[]; total: number }>({
  url: '/risk/decisions/search', method: 'GET', params,
})

// ── 审计链完整性验证（合规 / 取证） ────────────────

export interface AuditChainVerifyResp {
  ok: boolean
  verified: number
  failed_at_index?: number
  error?: string
  error_field?: string
  want?: string
  got?: string
}

export const verifyAuditChain = (limit = 1000) =>
  request<AuditChainVerifyResp>({
    url: '/risk/audit/chain/verify', method: 'POST', data: { limit },
  })

// ── whoami: 当前 admin token role (RBAC UI) ────────────────────

export type AdminRole = 'read' | 'write' | 'danger' | ''

export interface WhoamiResp {
  authenticated: boolean
  role: AdminRole
  key_id: string
}

export const whoami = () => request<WhoamiResp>({ url: '/risk/whoami', method: 'GET' })

export const listDecisions = (limit = 100) =>
  request<{ items: DecisionRow[]; total: number }>({
    url: '/risk/decisions', method: 'GET', params: { limit },
  })

// ── 外部反欺诈信号 (Sift / MaxMind / IPQS) push + cache ────────

export type ExtSignalProvider = 'sift' | 'maxmind' | 'ipqs' | 'onfido'

export interface ExtSignalResult {
  Provider: string
  Entity: { Type: string; Key: string }
  Score: number
  Reasons?: string[]
  RawJSON?: string
  FetchedAt: string
}

export const pushExtSignal = (body: {
  provider: ExtSignalProvider | string
  entity_type: string
  entity_key: string
  score: number
  reasons?: string[]
  raw_json?: string
}) => request<{ status: string }>({
  url: '/risk/extsignal/push', method: 'POST', data: body,
})

export const getExtSignal = (params: {
  provider: string; type: string; key: string
}) => request<{ hit: boolean; result: ExtSignalResult }>({
  url: '/risk/extsignal/get', method: 'GET', params,
})

export const extSignalStats = () =>
  request<{ size: number }>({ url: '/risk/extsignal/stats', method: 'GET' })
