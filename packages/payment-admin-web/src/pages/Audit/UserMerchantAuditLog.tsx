import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Space, Table, Tag, Tooltip, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { listUserMerchantAudit } from '../../api'
import type { UserMerchantAuditEntry } from '../../api'

/**
 * User-merchant-core append-only audit log viewer.
 *
 * Every merchant mutation (Create / Update / RotateApiKey / KYC transitions /
 * PutSecret / DeleteSecret ...) writes a row via the AuditInterceptor in
 * user-merchant-core. Each row has prev_hash + row_hash such that tampering
 * any past row breaks the chain. audit-verify CLI validates the chain offline;
 * this UI surfaces recent events for live triage.
 */
export default function UserMerchantAuditLogPage() {
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<UserMerchantAuditEntry[]>([])
  const [filters, setFilters] = useState<{ actor?: string; target?: string }>({})
  const [detail, setDetail] = useState<UserMerchantAuditEntry | null>(null)
  const [limit] = useState(100)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listUserMerchantAudit({ ...filters, limit })
      setRows(r.entries || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [filters, limit])

  useEffect(() => { void load() }, [load])

  // Forward chain validation 客户端再查一遍 — 只要展示的 N 行里任意一行的
  // prev_hash != 上一行的 row_hash 就提示。完整验证交给 cmd/audit-verify CLI。
  const chainOk = useMemo(() => {
    for (let i = 1; i < rows.length; i++) {
      if (rows[i - 1].prev_hash !== rows[i].row_hash) return { ok: false, idx: i }
    }
    return { ok: true as const }
  }, [rows])

  return (
    <div>
      <Typography.Title level={3}>商户审计日志（user-merchant-core）</Typography.Title>
      <Typography.Paragraph type="secondary">
        append-only · 链式 row_hash · mutation 自动写入（Create/Rotate/SubmitKyc/Put…）
      </Typography.Paragraph>
      {!chainOk.ok && (
        <Alert
          type="error"
          message={`链式签名在第 ${chainOk.idx} 行断裂 —— 立即跑 audit-verify CLI 核对`}
          style={{ marginBottom: 12 }}
        />
      )}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Form
          layout="inline"
          onFinish={(v) => setFilters(v)}
          initialValues={filters}
        >
          <Form.Item name="actor" label="Actor">
            <Input placeholder="admin user / bearer prefix" allowClear />
          </Form.Item>
          <Form.Item name="target" label="Target merchant_id">
            <Input placeholder="mch_xxx" allowClear />
          </Form.Item>
          <Space>
            <Button type="primary" htmlType="submit">查询</Button>
            <Button onClick={() => setFilters({})}>清空</Button>
            <Button onClick={() => load()}>刷新</Button>
          </Space>
        </Form>
      </Card>
      <Table<UserMerchantAuditEntry>
        size="small"
        rowKey="id"
        loading={loading}
        dataSource={rows}
        pagination={{ pageSize: limit, showTotal: (t) => `${t} 条` }}
        onRow={(r) => ({ onClick: () => setDetail(r) })}
        columns={[
          { title: '时间', dataIndex: 'created_ms', width: 170, render: (v: number) => dayjs(v).format('YYYY-MM-DD HH:mm:ss.SSS') },
          { title: 'Actor', dataIndex: 'actor', width: 140, ellipsis: true },
          { title: 'IP', dataIndex: 'actor_ip', width: 140, ellipsis: true },
          {
            title: 'Method',
            dataIndex: 'method',
            ellipsis: true,
            render: (m: string) => <Tooltip title={m}><code>{m.split('/').pop()}</code></Tooltip>,
          },
          { title: 'Target', dataIndex: 'target_id', width: 140, ellipsis: true },
          {
            title: 'Status',
            dataIndex: 'status_code',
            width: 100,
            render: (s: string) => s === 'OK' ? <Tag color="green">OK</Tag> : <Tag color="red">{s}</Tag>,
          },
          { title: '耗时', dataIndex: 'duration_ms', width: 70, render: (d) => d != null ? `${d}ms` : '-' },
        ]}
      />
      <Drawer
        open={!!detail}
        onClose={() => setDetail(null)}
        title={`审计条目 #${detail?.id ?? ''}`}
        width={640}
      >
        {detail && (
          <>
            <Field label="Method" value={detail.method} />
            <Field label="Actor" value={`${detail.actor} (${detail.actor_ip || '-'})`} />
            <Field label="Target" value={detail.target_id || '-'} />
            <Field label="Status" value={`${detail.status_code}${detail.response_err ? ': ' + detail.response_err : ''}`} />
            <Field label="Duration" value={detail.duration_ms != null ? `${detail.duration_ms}ms` : '-'} />
            <Field label="Trace" value={detail.trace_id || '-'} mono />
            <Field label="Prev hash" value={detail.prev_hash || '(genesis)'} mono />
            <Field label="Row hash" value={detail.row_hash} mono />
            <Typography.Title level={5} style={{ marginTop: 16 }}>Request body</Typography.Title>
            <pre style={{ background: '#f5f5f5', padding: 12, maxHeight: 320, overflow: 'auto' }}>
              {detail.request_body || '(empty)'}
            </pre>
          </>
        )}
      </Drawer>
    </div>
  )
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div style={{ marginBottom: 8 }}>
      <Typography.Text type="secondary">{label}: </Typography.Text>
      {mono
        ? <Typography.Text code copyable>{value}</Typography.Text>
        : <Typography.Text>{value}</Typography.Text>}
    </div>
  )
}
