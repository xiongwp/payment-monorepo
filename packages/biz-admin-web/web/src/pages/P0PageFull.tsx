// P0PageFull — p0-services.html 完整 React 版.

import { useEffect, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { aml, dr } from '../api/client';

type Tab = 'aml' | 'vault' | 'tax' | 'dr';

export default function P0PageFull() {
  const [tab, setTab] = useState<Tab>('aml');

  return (
    <div className="max-w-7xl mx-auto p-4">
      <header className="mb-4">
        <h1 className="text-xl font-semibold">🛡 P0 服务复核控制台</h1>
      </header>

      <nav className="flex gap-2 mb-4">
        {([
          ['aml', '🔍 AML 命中'],
          ['vault', '🔐 Vault'],
          ['tax', '📑 Tax'],
          ['dr', '⚖️ Data Rights'],
        ] as [Tab, string][]).map(([id, label]) => (
          <button
            key={id}
            onClick={() => setTab(id)}
            className={`px-4 py-2 text-sm border rounded ${
              tab === id ? 'bg-blue-600 text-white' : 'bg-white hover:bg-blue-50'
            }`}
          >
            {label}
          </button>
        ))}
      </nav>

      {tab === 'aml' && <AMLTab />}
      {tab === 'vault' && <VaultTab />}
      {tab === 'tax' && <TaxTab />}
      {tab === 'dr' && <DRTab />}
    </div>
  );
}

// ─── AML tab ───

function AMLTab() {
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({
    queryKey: ['aml-hits'],
    queryFn: () => aml.listPending(50),
    refetchInterval: 10_000,
  });
  const resolve = useMutation({
    mutationFn: ({ id, decision, reason }: { id: string; decision: 'cleared' | 'frozen' | 'escalated'; reason?: string }) =>
      aml.resolve(id, decision, reason),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['aml-hits'] }),
  });

  if (isLoading) return <Loading />;
  const hits = data?.hits || [];

  return (
    <section className="bg-white rounded-lg shadow-sm p-5">
      <h2 className="text-base font-semibold mb-3">AML 待复核命中 ({hits.length})</h2>
      <table className="min-w-full text-sm">
        <thead className="text-xs text-gray-500 border-b">
          <tr>
            <th className="text-left py-2 px-2">Hit ID</th>
            <th className="text-left py-2 px-2">Source</th>
            <th className="text-left py-2 px-2">Matched Name</th>
            <th className="text-right py-2 px-2">Confidence</th>
            <th className="text-left py-2 px-2">Matched On</th>
            <th className="text-left py-2 px-2">Program</th>
            <th className="text-right py-2 px-2">动作</th>
          </tr>
        </thead>
        <tbody>
          {hits.map((h: any) => (
            <tr key={h.hit_id} className="border-b hover:bg-gray-50">
              <td className="py-2 px-2 text-xs font-mono">{h.hit_id}</td>
              <td className="py-2 px-2">
                <Badge color="blue">{h.list_entry.source}</Badge>
              </td>
              <td className="py-2 px-2">{h.list_entry.primary_name}</td>
              <td className="py-2 px-2 text-right font-semibold">{h.confidence}</td>
              <td className="py-2 px-2 text-xs">{(h.matched_on || []).join(',')}</td>
              <td className="py-2 px-2 text-xs text-gray-600">{h.list_entry.program}</td>
              <td className="py-2 px-2 text-right space-x-1">
                <button
                  onClick={() => resolve.mutate({ id: h.hit_id, decision: 'cleared' })}
                  className="text-xs px-2 py-1 bg-green-100 text-green-800 rounded"
                >
                  误报
                </button>
                <button
                  onClick={() => resolve.mutate({ id: h.hit_id, decision: 'frozen' })}
                  className="text-xs px-2 py-1 bg-red-100 text-red-800 rounded"
                >
                  冻结
                </button>
                <button
                  onClick={() => resolve.mutate({ id: h.hit_id, decision: 'escalated' })}
                  className="text-xs px-2 py-1 bg-orange-100 text-orange-800 rounded"
                >
                  SAR
                </button>
              </td>
            </tr>
          ))}
          {hits.length === 0 && (
            <tr>
              <td colSpan={7} className="text-center text-gray-400 py-6">
                无待复核命中
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </section>
  );
}

// ─── Vault tab ───

function VaultTab() {
  const [merchantId, setMerchantId] = useState('');
  const [count, setCount] = useState<number | null>(null);

  const queryCount = async () => {
    if (!merchantId) return;
    const r = await fetch(`/api/vault/admin/metrics/merchants/${merchantId}/count`, {
      headers: { 'X-Admin-Token': localStorage.getItem('admin_token') || '' },
    });
    const data = await r.json();
    setCount(data.active_token_count);
  };

  return (
    <section className="bg-white rounded-lg shadow-sm p-5">
      <h2 className="text-base font-semibold mb-3">Tokenization Vault</h2>
      <p className="text-xs text-gray-500 mb-4">PCI 边界内的网络代币 (VTS/MDES) 健康概览.</p>
      <div className="grid grid-cols-3 gap-3 mb-4">
        {['VTS (Visa)', 'MDES (Mastercard)', 'Inhouse'].map((p) => (
          <div key={p} className="bg-gray-50 p-3 rounded border">
            <div className="text-xs text-gray-500">{p}</div>
            <div className="text-xl font-semibold mt-1 text-green-600">configured</div>
          </div>
        ))}
      </div>
      <div className="text-sm">
        <input
          value={merchantId}
          onChange={(e) => setMerchantId(e.target.value)}
          placeholder="merchant_id"
          className="border px-3 py-1.5 rounded text-sm w-72"
        />
        <button onClick={queryCount} className="ml-2 text-sm px-3 py-1.5 bg-blue-600 text-white rounded">
          查询活跃 token 数
        </button>
        {count !== null && (
          <span className="ml-3 text-sm">
            → active tokens: <strong>{count}</strong>
          </span>
        )}
      </div>
    </section>
  );
}

// ─── Tax tab ───

function TaxTab() {
  const [year, setYear] = useState(new Date().getFullYear());
  const [eligible, setEligible] = useState<any[]>([]);
  const [loading, setLoading] = useState(false);

  const loadEligible = async () => {
    setLoading(true);
    const r = await fetch(`/api/tax/admin/eligible/${year}`, {
      headers: { 'X-Admin-Token': localStorage.getItem('admin_token') || '' },
    });
    const data = await r.json();
    setEligible(data.eligible || []);
    setLoading(false);
  };

  const bulkGenerate = async () => {
    if (!confirm(`批量生成 ${year} 年所有符合阈值的 1099-K?`)) return;
    const r = await fetch(`/api/tax/admin/bulk-generate/${year}`, {
      method: 'POST',
      headers: { 'X-Admin-Token': localStorage.getItem('admin_token') || '' },
    });
    const data = await r.json();
    alert(`生成 ${data.generated} 份 1099-K`);
  };

  return (
    <section className="bg-white rounded-lg shadow-sm p-5">
      <h2 className="text-base font-semibold mb-3">Tax Reporting</h2>
      <div className="mb-3 flex items-center gap-3">
        <label className="text-sm">
          年:{' '}
          <input
            value={year}
            onChange={(e) => setYear(Number(e.target.value))}
            type="number"
            className="border px-2 py-1 rounded w-24"
          />
        </label>
        <button onClick={loadEligible} className="text-sm px-3 py-1 bg-blue-600 text-white rounded">
          {loading ? '加载中...' : '看符合 1099-K 阈值的商户'}
        </button>
        <button onClick={bulkGenerate} className="text-sm px-3 py-1 bg-orange-600 text-white rounded">
          批量生成 1099-K
        </button>
      </div>
      <table className="min-w-full text-sm">
        <thead className="text-xs text-gray-500 border-b">
          <tr>
            <th className="text-left py-2">Merchant</th>
            <th className="text-left py-2">Jurisdiction</th>
            <th className="text-right py-2">Gross</th>
            <th className="text-right py-2">Txn 笔数</th>
          </tr>
        </thead>
        <tbody>
          {eligible.map((m: any) => (
            <tr key={m.merchant_id} className="border-b hover:bg-gray-50">
              <td className="py-2 font-mono text-xs">{m.merchant_id}</td>
              <td className="py-2">{m.jurisdiction}</td>
              <td className="py-2 text-right">${(m.total_gross / 100).toLocaleString()}</td>
              <td className="py-2 text-right">{m.total_count}</td>
            </tr>
          ))}
          {eligible.length === 0 && (
            <tr>
              <td colSpan={4} className="text-center text-gray-400 py-6">
                点上面按钮加载
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </section>
  );
}

// ─── Data Rights tab ───

function DRTab() {
  const qc = useQueryClient();
  const [overdue, setOverdue] = useState(false);
  const { data, isLoading } = useQuery({
    queryKey: ['dr-requests', overdue],
    queryFn: () => dr.list(overdue),
    refetchInterval: 15_000,
  });
  const act = useMutation({
    mutationFn: ({ id, action, reason }: any) => dr.action(id, action, 'biz-admin-react', reason || ''),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['dr-requests'] }),
  });

  if (isLoading) return <Loading />;
  const requests = data?.requests || [];

  return (
    <section className="bg-white rounded-lg shadow-sm p-5">
      <h2 className="text-base font-semibold mb-3">Data Rights (DSAR / RTBF)</h2>
      <p className="text-xs text-gray-500 mb-4">
        30 天法定 SLA. <strong className="text-red-600">超期未处理 = 合规违规.</strong>
      </p>
      <div className="mb-3 text-sm">
        <label>
          <input type="checkbox" checked={overdue} onChange={(e) => setOverdue(e.target.checked)} className="mr-1" />
          仅看 overdue
        </label>
      </div>
      <table className="min-w-full text-sm">
        <thead className="text-xs text-gray-500 border-b">
          <tr>
            <th className="text-left py-2 px-2">Request ID</th>
            <th className="text-left py-2 px-2">Type</th>
            <th className="text-left py-2 px-2">Subject</th>
            <th className="text-left py-2 px-2">State</th>
            <th className="text-left py-2 px-2">Deadline</th>
            <th className="text-right py-2 px-2">动作</th>
          </tr>
        </thead>
        <tbody>
          {requests.map((r: any) => {
            const isOverdue =
              r.state !== 'fulfilled' && r.state !== 'rejected' && new Date(r.deadline_at) < new Date();
            return (
              <tr key={r.request_id} className="border-b hover:bg-gray-50">
                <td className="py-2 px-2 font-mono text-xs">{r.request_id}</td>
                <td className="py-2 px-2">
                  <Badge color="blue">{r.type}</Badge>
                </td>
                <td className="py-2 px-2 text-xs">
                  {r.subject.type}:{r.subject.id}
                </td>
                <td className="py-2 px-2">
                  <Badge color={isOverdue ? 'red' : r.state === 'fulfilled' ? 'green' : 'blue'}>{r.state}</Badge>
                </td>
                <td
                  className={`py-2 px-2 text-xs ${
                    isOverdue ? 'text-red-600 font-semibold' : ''
                  }`}
                >
                  {new Date(r.deadline_at).toLocaleDateString()}
                </td>
                <td className="py-2 px-2 text-right space-x-1">
                  <button
                    onClick={() => act.mutate({ id: r.request_id, action: 'verify' })}
                    className="text-xs px-2 py-1 bg-blue-100 text-blue-800 rounded"
                  >
                    verify
                  </button>
                  <button
                    onClick={() => act.mutate({ id: r.request_id, action: 'approve' })}
                    className="text-xs px-2 py-1 bg-green-100 text-green-800 rounded"
                  >
                    approve
                  </button>
                  <button
                    onClick={() => {
                      const reason = prompt('拒绝理由?');
                      if (reason) act.mutate({ id: r.request_id, action: 'reject', reason });
                    }}
                    className="text-xs px-2 py-1 bg-red-100 text-red-800 rounded"
                  >
                    reject
                  </button>
                  <button
                    onClick={() => act.mutate({ id: r.request_id, action: 'fulfill' })}
                    className="text-xs px-2 py-1 bg-purple-100 text-purple-800 rounded"
                  >
                    fulfill
                  </button>
                </td>
              </tr>
            );
          })}
          {requests.length === 0 && (
            <tr>
              <td colSpan={6} className="text-center text-gray-400 py-6">
                无工单
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </section>
  );
}

// ─── shared ───

function Badge({ children, color = 'blue' }: { children: any; color?: 'blue' | 'green' | 'red' | 'gray' }) {
  const cls = {
    blue: 'bg-blue-100 text-blue-800',
    green: 'bg-green-100 text-green-800',
    red: 'bg-red-100 text-red-800',
    gray: 'bg-gray-100 text-gray-700',
  }[color];
  return <span className={`${cls} text-xs px-2 py-0.5 rounded`}>{children}</span>;
}

function Loading() {
  return <div className="p-6 text-gray-500 text-sm">加载中...</div>;
}
