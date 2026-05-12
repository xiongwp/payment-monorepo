// API client — biz-admin-web Go server 代理所有 /api/* 到对应 backend.
// 我们这里只需要 fetch + JSON.

const ADMIN_TOKEN = () => localStorage.getItem('admin_token') || '';

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set('Content-Type', 'application/json');
  if (ADMIN_TOKEN()) headers.set('X-Admin-Token', ADMIN_TOKEN());

  const r = await fetch(path, { ...init, headers });
  if (!r.ok) {
    const text = await r.text();
    throw new Error(`HTTP ${r.status}: ${text}`);
  }
  if (r.headers.get('content-type')?.includes('application/json')) {
    return (await r.json()) as T;
  }
  // SSE / 文本接口
  return r as unknown as T;
}

// AML
export const aml = {
  listPending: (limit = 50) =>
    request<{ hits: any[]; limit: number }>(`/api/aml/admin/hits/pending?limit=${limit}`),
  resolve: (hitId: string, decision: 'cleared' | 'frozen' | 'escalated', reason?: string) =>
    request(`/api/aml/admin/hits/${hitId}/resolve`, {
      method: 'POST',
      body: JSON.stringify({ reviewer: 'biz-admin-react', decision, reason }),
    }),
};

// Approval
export const approval = {
  list: (state = '', type = '') =>
    request<{ actions: any[] }>(`/api/approval/v1/actions?state=${state}&type=${type}&limit=100`),
  approve: (id: string, reviewer: string, note: string) =>
    request(`/api/approval/v1/actions/${id}/approve`, {
      method: 'POST',
      body: JSON.stringify({ reviewer, note }),
    }),
  reject: (id: string, reviewer: string, note: string) =>
    request(`/api/approval/v1/actions/${id}/reject`, {
      method: 'POST',
      body: JSON.stringify({ reviewer, note }),
    }),
  cancel: (id: string, requester: string, reason: string) =>
    request(`/api/approval/v1/actions/${id}/cancel`, {
      method: 'POST',
      body: JSON.stringify({ requester, reason }),
    }),
};

// Data Rights
export const dr = {
  list: (overdue = false) =>
    request<{ requests: any[] }>(`/api/dr/admin/requests?limit=50${overdue ? '&overdue=1' : ''}`),
  action: (id: string, action: 'verify' | 'approve' | 'reject' | 'fulfill', reviewer: string, reason = '') =>
    request(`/api/dr/admin/requests/${id}/${action}`, {
      method: 'POST',
      body: JSON.stringify({ reviewer, reason }),
    }),
};

// SSE stream — approval-service /v1/stream
export function subscribeApprovalUpdates(onUpdate: () => void) {
  const sse = new EventSource('/api/approval/v1/stream');
  sse.addEventListener('action_update', onUpdate);
  return () => sse.close();
}
