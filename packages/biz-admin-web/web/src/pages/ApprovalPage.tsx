// ApprovalPage — React reference implementation 替代 approval.html (Alpine.js).
//
// 演示:
//   - React Query: 自动 retry / cache / refetch
//   - SSE 实时刷新 → invalidate query
//   - TypeScript 类型安全
//   - 复用 api/client.ts 不写 fetch boilerplate

import { useEffect, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { approval, subscribeApprovalUpdates } from '../api/client';

type State =
  | 'pending'
  | 'reviewing'
  | 'approved'
  | 'rejected'
  | 'executed'
  | 'cancelled'
  | 'expired';

interface Action {
  id: string;
  type: string;
  resource: string;
  requester: string;
  requester_at: string;
  request_note?: string;
  required_approvals: number;
  state: State;
  approvals: { reviewer: string; decision: 'approve' | 'reject'; note?: string; reviewed_at: string }[];
  payload?: Record<string, any>;
}

const STATE_BADGE: Record<State, string> = {
  pending:   'bg-blue-100 text-blue-800',
  reviewing: 'bg-yellow-100 text-yellow-800',
  approved:  'bg-green-100 text-green-800',
  rejected:  'bg-red-100 text-red-800',
  executed:  'bg-purple-100 text-purple-800',
  cancelled: 'bg-gray-100 text-gray-600',
  expired:   'bg-gray-200 text-gray-700',
};

export default function ApprovalPage() {
  const qc = useQueryClient();
  const [reviewer, setReviewer] = useState(localStorage.getItem('reviewer') || 'ops_alice');
  const [filterState, setFilterState] = useState<State | ''>('');

  // 持久化 reviewer 选择
  useEffect(() => {
    localStorage.setItem('reviewer', reviewer);
  }, [reviewer]);

  // 拉列表
  const { data, isLoading, error } = useQuery({
    queryKey: ['approval', filterState],
    queryFn: () => approval.list(filterState),
  });

  // SSE 实时刷新
  useEffect(() => {
    return subscribeApprovalUpdates(() => {
      qc.invalidateQueries({ queryKey: ['approval'] });
    });
  }, [qc]);

  // approve / reject mutations
  const decide = useMutation({
    mutationFn: ({ id, action, note }: { id: string; action: 'approve' | 'reject'; note: string }) =>
      action === 'approve'
        ? approval.approve(id, reviewer, note)
        : approval.reject(id, reviewer, note),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['approval'] }),
  });

  if (isLoading) return <div className="p-6 text-gray-500">加载中...</div>;
  if (error) return <div className="p-6 text-red-600">出错: {(error as Error).message}</div>;

  const actions: Action[] = data?.actions || [];
  const counts = actions.reduce<Record<string, number>>((a, x) => {
    a[x.state] = (a[x.state] || 0) + 1;
    return a;
  }, {});

  return (
    <div className="max-w-6xl mx-auto p-4">
      <header className="mb-4 flex items-center justify-between">
        <h1 className="text-xl font-semibold">👥 双人复核</h1>
        <div className="flex items-center gap-2 text-sm">
          <label>reviewer:</label>
          <input
            className="border px-2 py-1 rounded"
            value={reviewer}
            onChange={(e) => setReviewer(e.target.value)}
          />
        </div>
      </header>

      {/* stats */}
      <div className="grid grid-cols-5 gap-2 mb-4">
        {(['pending', 'reviewing', 'approved', 'rejected', 'executed'] as State[]).map((s) => (
          <div key={s} className="bg-white rounded shadow-sm p-3 border">
            <div className="text-xs text-gray-500">{s}</div>
            <div className="text-2xl font-semibold">{counts[s] || 0}</div>
          </div>
        ))}
      </div>

      {/* filter */}
      <div className="mb-3 flex gap-2 items-center text-sm">
        <select
          value={filterState}
          onChange={(e) => setFilterState(e.target.value as State | '')}
          className="border px-2 py-1 rounded"
        >
          <option value="">所有</option>
          <option value="pending">pending</option>
          <option value="reviewing">reviewing</option>
          <option value="approved">approved</option>
        </select>
      </div>

      {/* list */}
      <div className="space-y-3">
        {actions.map((a) => (
          <ActionCard
            key={a.id}
            action={a}
            currentReviewer={reviewer}
            onDecide={(decision, note) => decide.mutate({ id: a.id, action: decision, note })}
          />
        ))}
        {!actions.length && <div className="text-gray-400 text-center py-12">无待复核动作</div>}
      </div>
    </div>
  );
}

function ActionCard({
  action,
  currentReviewer,
  onDecide,
}: {
  action: Action;
  currentReviewer: string;
  onDecide: (decision: 'approve' | 'reject', note: string) => void;
}) {
  const [note, setNote] = useState('');
  const isRequester = action.requester === currentReviewer;
  const alreadyReviewed = (action.approvals || []).some((p) => p.reviewer === currentReviewer);
  const canAct =
    (action.state === 'pending' || action.state === 'reviewing') &&
    !isRequester &&
    !alreadyReviewed;
  const approveCount = (action.approvals || []).filter((p) => p.decision === 'approve').length;

  return (
    <div className="bg-white rounded shadow-sm border p-4">
      <div className="flex items-start justify-between mb-2">
        <div className="flex-1">
          <div className="flex items-center gap-2">
            <span className={`px-2 py-0.5 rounded text-xs ${STATE_BADGE[action.state]}`}>
              {action.state}
            </span>
            <span className="font-medium">{action.type}</span>
            <span className="text-xs text-gray-400 font-mono">{action.id}</span>
          </div>
          <div className="text-xs text-gray-600 mt-1">
            resource: <span className="font-mono">{action.resource}</span> · requester:{' '}
            {action.requester} · {timeAgo(action.requester_at)}
          </div>
          {action.request_note && (
            <div className="text-xs text-gray-500 italic mt-1">note: {action.request_note}</div>
          )}
        </div>
        <div className="text-right">
          <div className="text-xs text-gray-500">approvals</div>
          <div className="font-semibold">
            {approveCount} / {action.required_approvals}
          </div>
        </div>
      </div>

      {action.approvals?.length > 0 && (
        <div className="border-t border-gray-100 pt-2 mb-2 text-xs space-y-0.5">
          {action.approvals.map((p, i) => (
            <div key={i} className="flex items-center gap-2">
              <span
                className={`px-2 rounded ${
                  p.decision === 'approve' ? 'bg-green-100 text-green-700' : 'bg-red-100 text-red-700'
                }`}
              >
                {p.decision}
              </span>
              <span className="font-medium">{p.reviewer}</span>
              {p.note && <span className="text-gray-500">{p.note}</span>}
              <span className="ml-auto text-gray-400">{timeAgo(p.reviewed_at)}</span>
            </div>
          ))}
        </div>
      )}

      {canAct && (
        <div className="flex gap-2">
          <input
            placeholder="复核说明 (reject 必填)"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            className="border px-2 py-1.5 rounded text-sm flex-1"
          />
          <button
            onClick={() => onDecide('approve', note)}
            className="px-4 py-1.5 bg-green-600 text-white text-sm rounded"
          >
            ✓ Approve
          </button>
          <button
            onClick={() => {
              if (!note) {
                alert('reject 必填理由');
                return;
              }
              onDecide('reject', note);
            }}
            className="px-4 py-1.5 bg-red-600 text-white text-sm rounded"
          >
            ✗ Reject
          </button>
        </div>
      )}
      {!canAct && (
        <div className="text-xs text-gray-400">
          {isRequester && '你是 requester, 不能 self-approve'}
          {alreadyReviewed && !isRequester && '你已审过'}
          {action.state === 'approved' && '待业务侧执行'}
          {action.state === 'executed' && '已执行'}
          {action.state === 'rejected' && '已驳回'}
        </div>
      )}
    </div>
  );
}

function timeAgo(ts: string): string {
  const diff = (Date.now() - new Date(ts).getTime()) / 1000;
  if (diff < 60) return `${Math.floor(diff)}s ago`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}
