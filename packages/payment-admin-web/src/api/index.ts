import { request } from './client'
import type {
  DashboardSummary,
  DecryptResponse,
  EncryptResponse,
  ListKeysResponse,
  ListOrdersResponse,
  ChargeSummary,
  RefundSummary,
  PaymentIntentDetail,
  ProbeRouteResponse,
  WebhookTestResponse,
} from './types'

// ─── dashboard ──────────────────────────────────────────────────
export const getDashboard = () =>
  request<DashboardSummary>({ url: '/dashboard/summary', method: 'GET' })

// ─── orders ─────────────────────────────────────────────────────
export const listOrders = (params: { mch_id?: string; page?: number; page_size?: number }) =>
  request<ListOrdersResponse>({ url: '/orders', method: 'GET', params })

export const getOrder = (id: string) =>
  request<PaymentIntentDetail>({ url: `/orders/${id}`, method: 'GET' })

export const listOrderCharges = (id: string) =>
  request<{ items: ChargeSummary[] }>({ url: `/orders/${id}/charges`, method: 'GET' })

export const listOrderRefunds = (id: string) =>
  request<{ items: RefundSummary[] }>({ url: `/orders/${id}/refunds`, method: 'GET' })

// ─── channels ───────────────────────────────────────────────────
export const probeRoute = (body: {
  country: string
  payment_method: string
  amount: number
  currency: string
  merchant?: string
}) => request<ProbeRouteResponse>({ url: '/channels/routes/probe', method: 'POST', data: body })

export const testWebhook = (body: {
  adapter: string
  headers: Record<string, string>
  body: string
}) => request<WebhookTestResponse>({ url: '/channels/webhook-test', method: 'POST', data: body })

// ─── kms ────────────────────────────────────────────────────────
export const listKeys = () => request<ListKeysResponse>({ url: '/kms/keys', method: 'GET' })

export const kmsEncrypt = (body: { plaintext: string; context: string; key_id?: string }) =>
  request<EncryptResponse>({ url: '/kms/encrypt', method: 'POST', data: body })

export const kmsDecrypt = (body: { ciphertext: string; context: string }) =>
  request<DecryptResponse>({ url: '/kms/decrypt', method: 'POST', data: body })

// ─── mock merchant app ─────────────────────────────────────────
// 形状镜像 order-core PaymentIntent（BFF 取了子集 → piDetail JSON）
export interface AppNextAction {
  id: string
  action_type: string
  payment_intent_id: string
  charge_id: string
  status: string
  payload: Record<string, string>
  expires_at: number
}

export interface AppConfirmResponse {
  payment_intent: import('./types').PaymentIntentDetail
  charge?: import('./types').ChargeSummary
  next_action?: AppNextAction
}

export const appCreateIntent = (body: {
  mch_id: string
  mch_order_no?: string
  amount: number
  currency?: string
  country?: string
  description?: string
  payment_method_types?: string[]
  customer_id?: string
  return_url?: string
  idempotency_key?: string
  metadata?: Record<string, string>
}) =>
  request<import('./types').PaymentIntentDetail>({
    url: '/app/create-intent',
    method: 'POST',
    data: body,
  })

export const appConfirm = (body: { id: string; payment_method: string }) =>
  request<AppConfirmResponse>({ url: '/app/confirm', method: 'POST', data: body })

export const appRetrieveIntent = (id: string) =>
  request<import('./types').PaymentIntentDetail>({
    url: `/app/intent/${id}`,
    method: 'GET',
  })

// ─── merchants + KYC ────────────────────────────────────────────

export interface Merchant {
  id: string
  name: string
  legal_name?: string
  country: string
  business_type: string
  tax_id?: string
  contact_email: string
  contact_phone?: string
  website?: string
  mcc?: string
  webhook_url?: string
  kyc_status: string
  kyc_level: number
  kyc_reason?: string
  kyc_reviewer?: string
  kyc_reviewed_at_ms?: number
  risk_tier: string
  rate_limit_rps: number
  settle_currency: string
  settle_method?: string
  settle_account?: string
  settle_bank?: string
  settle_holder?: string
  status: string
  metadata?: Record<string, string>
  created_ms: number
  updated_ms: number
}

export interface MerchantListResponse {
  items: Merchant[]
  total: number
}

export interface CreateMerchantResponse {
  merchant: Merchant
  live_secret_key: string
  test_secret_key: string
  webhook_secret: string
  _warning?: string
}

export interface MerchantKycAudit {
  id: number
  merchant_id: string
  from_status: string
  to_status: string
  reason?: string
  actor?: string
  created_ms: number
}

export interface MerchantKycDocument {
  id: string
  merchant_id: string
  doc_type: string
  doc_number?: string
  file_url: string
  mime_type?: string
  size_bytes?: number
  uploaded_by?: string
  review_status: string
  review_note?: string
  expires_at_ms?: number
  created_ms: number
  updated_ms: number
}

export const listMerchants = (params: { status?: string; kyc_status?: string; limit?: number; offset?: number }) =>
  request<MerchantListResponse>({ url: '/merchants', method: 'GET', params })

export const getMerchant = (id: string) =>
  request<Merchant>({ url: `/merchants/${id}`, method: 'GET' })

export const createMerchant = (body: Partial<Merchant>) =>
  request<CreateMerchantResponse>({ url: '/merchants', method: 'POST', data: body })

export const updateMerchant = (id: string, fields: Record<string, string>) =>
  request<Merchant>({ url: `/merchants/${id}`, method: 'PATCH', data: fields })

// rotate-key 是 one-shot 显示明文：双击会签发两把 key 但 UI 只能展示第二把
// → 第一把丢失但已生效 → 商户旧 key 全 401。BFF 端 5min idempotencyCache 已
// 兜底（merchants_idempotency_test.go 覆盖），但前端必须传 idempotency_key
// 才会命中。这里 freeze 一个 UUID 在 caller 站点；caller 应该在 onClick 时
// 用 useRef / useState 把 key 锁住整轮 retry，不要每次重新生成。
//
// 兼容老 caller：第二个参数没传 → 自动 random 一个（仍能防 axios 内部 retry
// 双发；防不了同一 caller 的多次 setState 触发的重复调用）。
export const rotateMerchantKey = (
  id: string, kind: 'live' | 'test', idempotencyKey?: string,
) =>
  request<{ plaintext: string; _warning?: string }>({
    url: `/merchants/${id}/rotate-key`,
    method: 'POST',
    data: {
      kind,
      idempotency_key: idempotencyKey ?? newIdempotencyKey(),
    },
  })

export type KycAction =
  | 'submit' | 'review' | 'approve' | 'reject'
  | 'request_more_info' | 'suspend' | 'unsuspend' | 'terminate'

// 同 rotate-key 的双击/重传防御：BFF 端按 (id, action, idempotency_key) cache
// 5min。terminate / approve 这类有商户邮件副作用的动作尤其重要。
export const kycTransition = (
  id: string,
  action: KycAction,
  body: { actor?: string; reason?: string; idempotencyKey?: string },
) =>
  request<Merchant>({
    url: `/merchants/${id}/kyc/${action}`,
    method: 'POST',
    data: {
      actor: body.actor,
      reason: body.reason,
      idempotency_key: body.idempotencyKey ?? newIdempotencyKey(),
    },
  })

// newIdempotencyKey 生成一把强随机 UUID 当 idempotency_key。caller 应该在
// onClick 时分配一次然后重用整轮 retry（比如 axios 自动重试），不要每次
// reactive 重新生成——那样每次重新生成相当于没接 idem。
//
// 用 crypto.randomUUID() 优先（modern browsers/Node 19+）；兜底走 Math.random
// 拼 v4。前端代码必须在 secure context（HTTPS）才有 window.crypto.randomUUID。
function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  // fallback: timestamp + random，足够防 double-click（不需要密码学强度）
  return (
    Date.now().toString(36) + '-' +
    Math.random().toString(36).slice(2, 10) + '-' +
    Math.random().toString(36).slice(2, 10)
  )
}

export const listMerchantDocuments = (id: string) =>
  request<MerchantKycDocument[]>({ url: `/merchants/${id}/documents`, method: 'GET' })

export const addMerchantDocument = (id: string, body: Partial<MerchantKycDocument>) =>
  request<MerchantKycDocument>({ url: `/merchants/${id}/documents`, method: 'POST', data: body })

export const reviewMerchantDocument = (docID: string, body: { status: string; note?: string }) =>
  request<{ ok: boolean }>({ url: `/merchants/documents/${docID}/review`, method: 'POST', data: body })

export const listMerchantAudits = (id: string, limit = 100) =>
  request<MerchantKycAudit[]>({ url: `/merchants/${id}/audits`, method: 'GET', params: { limit } })

// ─── admin audit log ─────────────────────────────────────────────

export interface AuditEntry {
  id: number
  actor: string
  actor_ip?: string
  action: string
  target_type?: string
  target_id?: string
  http_method?: string
  http_path?: string
  http_status?: number
  request_body?: string
  response_code?: string
  response_msg?: string
  duration_ms?: number
  created_ms: number
}

export interface AuditListResponse {
  items: AuditEntry[]
  total: number
}

export const listAudit = (params: {
  actor?: string
  action?: string
  target_type?: string
  target_id?: string
  since_ms?: number
  until_ms?: number
  limit?: number
  offset?: number
}) => request<AuditListResponse>({ url: '/audit', method: 'GET', params })

// ─── user-merchant-core audit chain ─────────────────────────────
// 独立于老的 order-core /audit；每次 merchant mutation 自动写，带链式 row_hash。
export interface UserMerchantAuditEntry {
  id: number
  actor: string
  actor_ip?: string
  method: string
  target_id?: string
  request_body?: string
  status_code: string
  response_err?: string
  duration_ms?: number
  trace_id?: string
  prev_hash?: string
  row_hash: string
  created_ms: number
}

export const listUserMerchantAudit = (params: {
  actor?: string
  target?: string
  limit?: number
  offset?: number
}) =>
  request<{ entries: UserMerchantAuditEntry[] }>({
    url: '/user-merchant/audits',
    method: 'GET',
    params,
  })

// ─── webhook deliveries (outbound) ───────────────────────────────

export interface WebhookDelivery {
  id: number
  merchant_id: string
  event_id: string
  event_type: string
  payload: string
  url: string
  status: string
  http_status: number
  attempts: number
  max_attempts: number
  last_error?: string
  next_retry_ms?: number
  created_ms: number
  updated_ms: number
}

export interface WebhookDeliveryListResponse {
  items: WebhookDelivery[]
  total: number
}

export const listWebhookDeliveries = (params: { merchant_id?: string; status?: string; limit?: number; offset?: number }) =>
  request<WebhookDeliveryListResponse>({ url: '/webhooks/deliveries', method: 'GET', params })

export const retryWebhookDelivery = (id: number) =>
  request<{ delivery: WebhookDelivery }>({ url: `/webhooks/deliveries/${id}/retry`, method: 'POST' })

export const testWebhookSend = (body: { merchant_id: string; event_type?: string; payload?: string }) =>
  request<{ delivery: WebhookDelivery }>({ url: '/webhooks/test-send', method: 'POST', data: body })

// ─── ledger (wave H) ─────────────────────────────────────────────

export interface GLAccount {
  id: string
  name: string
  type: string
  owner_type: string
  owner_id?: string
  currency: string
  debit_balance: number
  credit_balance: number
  net_balance: number
  version: number
  status: string
  created_ms: number
  updated_ms: number
}

export interface GLEntry {
  id: number
  txn_id: string
  account_id: string
  debit_amount: number
  credit_amount: number
  currency: string
  memo?: string
  created_ms: number
}

export interface GLTransaction {
  id: string
  event_type: string
  ref_type?: string
  ref_id?: string
  total_debit: number
  total_credit: number
  memo?: string
  reverses?: string
  created_ms: number
  entries?: GLEntry[]
}

export const listLedgerAccounts = (params: { owner_type?: string; owner_id?: string; limit?: number; offset?: number }) =>
  request<{ accounts: GLAccount[]; total: number }>({ url: '/ledger/accounts', method: 'GET', params })

export const getLedgerAccount = (id: string) =>
  request<GLAccount>({ url: `/ledger/accounts/${id}`, method: 'GET' })

export const listLedgerEntries = (params: { account_id: string; since_ms?: number; until_ms?: number; limit?: number; offset?: number }) =>
  request<{ entries: GLEntry[]; total: number }>({ url: '/ledger/entries', method: 'GET', params })

export const listLedgerTransactions = (params: { event_type?: string; ref_type?: string; ref_id?: string; limit?: number; offset?: number }) =>
  request<{ transactions: GLTransaction[]; total: number }>({ url: '/ledger/transactions', method: 'GET', params })

export const getLedgerTransaction = (id: string) =>
  request<GLTransaction>({ url: `/ledger/transactions/${id}`, method: 'GET' })

// ─── disputes (wave D) ───────────────────────────────────────────

export type DisputeStatus =
  | 'DISPUTE_STATUS_NEEDS_RESPONSE'
  | 'DISPUTE_STATUS_UNDER_REVIEW'
  | 'DISPUTE_STATUS_WON'
  | 'DISPUTE_STATUS_LOST'
  | 'DISPUTE_STATUS_WARNING_CLOSED'
  | 'DISPUTE_STATUS_CHARGE_REFUNDED'
  | 'DISPUTE_STATUS_CANCELED'

export interface Dispute {
  id: string
  payment_intent_id: string
  charge_id: string
  merchant_id: string
  channel: string
  channel_dispute_id?: string
  status: DisputeStatus
  amount: number
  currency: string
  reason: string
  reason_detail?: string
  evidence_due_at_ms?: number
  evidence?: Record<string, string>
  decided_at_ms?: number
  outcome_amount?: number
  auto_refund_charge: boolean
  metadata?: Record<string, string>
  created_ms: number
  updated_ms: number
}

export interface DisputeEvent {
  id: number
  dispute_id: string
  payment_intent_id: string
  from_status: string
  to_status: string
  source: string
  actor?: string
  note?: string
  payload?: Record<string, string>
  created_ms: number
}

export const listDisputes = (params: { merchant_id?: string; status?: DisputeStatus; limit?: number; offset?: number }) =>
  request<{ disputes: Dispute[]; total: number }>({ url: '/disputes', method: 'GET', params })

export const getDispute = (piID: string, id: string) =>
  request<Dispute>({ url: `/disputes/${piID}/${id}`, method: 'GET' })

export const listDisputeEvents = (piID: string, id: string, limit = 100) =>
  request<DisputeEvent[]>({ url: `/disputes/${piID}/${id}/events`, method: 'GET', params: { limit } })

export const submitDisputeEvidence = (piID: string, id: string, body: { actor: string; evidence: Record<string, string> }) =>
  request<Dispute>({ url: `/disputes/${piID}/${id}/evidence`, method: 'POST', data: body })

export const concedeDispute = (piID: string, id: string, body: { actor: string; note?: string }) =>
  request<Dispute>({ url: `/disputes/${piID}/${id}/concede`, method: 'POST', data: body })

export const cancelDispute = (piID: string, id: string, body: { actor: string; note?: string }) =>
  request<Dispute>({ url: `/disputes/${piID}/${id}/cancel`, method: 'POST', data: body })

// Simulate webhook events (useful for ops/QA on mock channel)
export const simulateDisputeEvent = (piID: string, id: string, body: { action: 'under_review' | 'won' | 'lost' | 'warning_closed'; outcome_amount?: number }) =>
  request<Dispute>({ url: `/disputes/${piID}/${id}/simulate`, method: 'POST', data: body })

// ─── merchant channel secrets (wave G) ───────────────────────────

export interface MerchantChannelSecret {
  id: number
  merchant_id: string
  channel: string
  field_name: string
  masked_hint?: string
  version: number
  created_by?: string
  created_ms: number
  updated_ms: number
}

export const listMerchantSecrets = (merchantID: string, channel?: string) =>
  request<{ secrets: MerchantChannelSecret[] }>({
    url: `/merchants/${merchantID}/secrets`, method: 'GET',
    params: channel ? { channel } : {},
  })

export const putMerchantSecret = (merchantID: string, body: {
  channel: string; field_name: string; plaintext: string; actor?: string;
}) =>
  request<{ secret: MerchantChannelSecret }>({
    url: `/merchants/${merchantID}/secrets`, method: 'POST', data: body,
  })

export const deleteMerchantSecret = (merchantID: string, channel: string, fieldName: string) =>
  request<{ ok: boolean }>({
    url: `/merchants/${merchantID}/secrets/${channel}/${fieldName}`, method: 'DELETE',
  })
