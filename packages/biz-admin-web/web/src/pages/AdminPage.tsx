// AdminPage — admin.html 完整 React 版.
// 7 服务健康监控 + 各服务关键操作入口.

import { useQuery } from '@tanstack/react-query';

const SERVICES = [
  { id: 'billing',  name: 'billing-system',     port: 8081, path: '/api/billing' },
  { id: 'gateway',  name: 'payment-gateway',    port: 8080, path: '/api/gateway' },
  { id: 'dispute',  name: 'dispute-service',    port: 8080, path: '/api/dispute' },
  { id: 'webhook',  name: 'merchant-webhook',   port: 8080, path: '/api/webhook' },
  { id: 'refund',   name: 'refund-engine',      port: 8080, path: '/api/refund' },
  { id: 'kyc',      name: 'kyc-service',        port: 8084, path: '/api/kyc' },
  { id: 'audit',    name: 'audit-log',          port: 8087, path: '/api/audit' },
];

export default function AdminPage() {
  const { data } = useQuery({
    queryKey: ['health-all'],
    queryFn: () => fetch('/health/all').then((r) => r.json()),
    refetchInterval: 10_000,
  });

  return (
    <div className="max-w-7xl mx-auto p-4">
      <header className="mb-4">
        <h1 className="text-xl font-semibold">💳 biz-admin · 业务服务监控</h1>
      </header>

      <div className="grid grid-cols-3 gap-3 mb-6">
        {SERVICES.map((s) => {
          const status = data?.[s.id] || 'unknown';
          const ok = status === 'up' || status === 'ok';
          return (
            <div key={s.id} className="bg-white border rounded p-3 flex items-center justify-between">
              <div>
                <div className="font-medium text-sm">{s.name}</div>
                <div className="text-xs text-gray-500 font-mono">{s.path}</div>
              </div>
              <span
                className={`w-3 h-3 rounded-full ${ok ? 'bg-green-500' : 'bg-red-500'}`}
                title={status}
              />
            </div>
          );
        })}
      </div>

      <h2 className="text-lg font-semibold mb-2">快捷入口</h2>
      <div className="grid grid-cols-3 gap-3">
        <QuickLink title="退款列表"  desc="refund-engine"     path="/api/refund/api/v1/refunds" />
        <QuickLink title="争议工单"  desc="dispute-service"   path="/api/dispute/api/v1/disputes" />
        <QuickLink title="商户列表"  desc="kyc-service"       path="/api/kyc/api/v1/merchants" />
        <QuickLink title="webhook DLQ" desc="merchant-webhook" path="/api/webhook/api/v1/dlq" />
        <QuickLink title="audit logs" desc="audit-log"        path="/api/audit/api/v1/audit/logs" />
      </div>
    </div>
  );
}

function QuickLink({ title, desc, path }: { title: string; desc: string; path: string }) {
  return (
    <a
      href={path}
      target="_blank"
      rel="noopener"
      className="bg-white border rounded p-3 hover:bg-blue-50"
    >
      <div className="font-medium text-sm">{title}</div>
      <div className="text-xs text-gray-500">{desc}</div>
      <div className="text-xs text-blue-600 mt-1">{path}</div>
    </a>
  );
}
