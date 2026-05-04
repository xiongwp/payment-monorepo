// 与 backend/internal/handler 的 JSON shape 对齐；字段少一点简洁展示用。

export interface PaymentIntentSummary {
  id: string
  amount: number
  currency: string
  status: string
  mch_id?: string
  mch_order_no?: string
  business_id?: string
  description?: string
  capture_method?: string
  created: number
}

export interface PaymentIntentDetail extends PaymentIntentSummary {
  amount_subtotal?: number
  amount_coupon?: number
  amount_points?: number
  customer_id?: string
  idempotency_key?: string
  confirmation_method?: string
  metadata?: Record<string, string>
}

export interface ChargeSummary {
  id: string
  payment_intent_id: string
  amount: number
  amount_captured: number
  amount_refunded: number
  currency: string
  status: string
  payment_method: string
  created: number
}

export interface RefundSummary {
  id: string
  payment_intent_id: string
  charge_id: string
  amount: number
  currency: string
  status: string
  reason: string
  created: number
}

export interface ListOrdersResponse {
  items: PaymentIntentSummary[]
  total: number
  page: number
  page_size: number
}

export interface KMSKey {
  key_id: string
  algorithm: string
  created_at: number
  active: boolean
}

export interface ListKeysResponse {
  items: KMSKey[]
  active_key_id: string
}

export interface EncryptResponse {
  ciphertext: string
  key_id: string
}

export interface DecryptResponse {
  plaintext: string
  key_id: string
}

export interface ProbeRouteResponse {
  result_type: string
  external_ref_no: string
  failure_code?: string
  failure_message?: string
}

export interface WebhookTestResponse {
  event_id: string
  event_type: string
  payment_intent_id: string
  charge_id?: string
  refund_id?: string
  external_ref_no?: string
  amount?: number
  timestamp?: number
}

export interface DashboardSummary {
  orders_total?: number
  recent_orders?: PaymentIntentSummary[]
  kms_active_key?: string
  kms_keys_total?: number
  // merchants snapshot（来自 user-merchant-core）
  merchants_total?: number
  merchants_approved?: number
  merchants_kyc_pending?: number
  recent_user_merchant_audits?: {
    id: number
    actor: string
    method: string
    target_id?: string
    status_code: string
    created_ms: number
  }[]
}
