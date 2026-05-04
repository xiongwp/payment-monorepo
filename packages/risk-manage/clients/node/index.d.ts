// TypeScript types for @xiongwp/risk-sdk

export type Verdict = 'ALLOW' | 'REVIEW' | 'DENY';
export type RiskLevel = 'low' | 'medium' | 'high' | 'critical';
export type ReviewStatus = 'pending' | 'approved' | 'rejected';
export type ReviewAction = 'approve' | 'reject';
export type OutcomeSource = 'review_human' | 'dispute' | 'merchant_confirm';
export type ListKind = 'allow' | 'block';

export interface DecisionAudit {
  decision_id: string;
  occurred_at: string;
  verdict: Verdict;
  risk_score: number;
  risk_level: RiskLevel;
  recommended_action?: '' | 'block' | 'step_up_3ds';
  hit_rules?: string[];
  eval_duration_ms?: number;
}

export interface ReviewItem {
  id: string;
  merchant_id: string;
  customer_id: string;
  payment_intent_id: string;
  amount: number;
  currency: string;
  risk_score: number;
  reasons: string[];
  status: ReviewStatus;
  created_at: string;
  decided_at?: string;
  decided_by?: string;
  decide_reason?: string;
}

export interface Outcome {
  decision_id: string;
  source: OutcomeSource;
  is_fraud: boolean;
  at?: string;
  actor?: string;
  notes?: string;
}

export interface MerchantListEntry {
  merchant_id: string;
  kind: ListKind;
  dimension: 'customer' | 'device' | 'ip' | 'card_fingerprint' | 'email';
  value: string;
  reason?: string;
  actor?: string;
  expires_at?: string;
}

export interface SessionSnapshot {
  fingerprint_hash: string;
  canvas_fingerprint?: string;
  webgl_renderer?: string;
  audio_context_hash?: string;
  screen_wxh?: string;
  timezone?: string;
  language?: string;
  hardware_concurrency?: number;
  platform?: 'web' | 'ios' | 'android' | 'weChat-miniprogram';
  user_agent?: string;
}

export interface RiskClientOptions {
  baseURL: string;
  apiKey?: string;
  timeoutMs?: number;
}

export class RiskClient {
  constructor(opts: RiskClientOptions);
  createSession(snapshot: SessionSnapshot): Promise<{ session_id: string }>;
  finalizeSession(behavior: Record<string, unknown> & { session_id: string }): Promise<void>;
  listDecisions(opts?: { limit?: number }): Promise<DecisionAudit[]>;
  listReviews(opts?: { status?: ReviewStatus; limit?: number; offset?: number }): Promise<ReviewItem[]>;
  getReview(id: string): Promise<ReviewItem>;
  decideReview(p: { id: string; action: ReviewAction; actor: string; reason?: string }): Promise<ReviewItem>;
  recordOutcome(o: Outcome): Promise<unknown>;
  getOutcomes(decisionId: string): Promise<Outcome[]>;
  recentOutcomes(opts?: { limit?: number }): Promise<Outcome[]>;
  listMerchantList(p: { merchant_id: string; kind?: ListKind }): Promise<MerchantListEntry[]>;
  addMerchantListEntry(e: MerchantListEntry, opts?: { ttl?: string }): Promise<{ status: string }>;
  removeMerchantListEntry(p: { merchant_id: string; kind: ListKind; dimension: string; value: string }): Promise<{ removed: boolean }>;
  verifyWebhook(signature: string, rawBody: Buffer | string, secret: string): boolean;
}
