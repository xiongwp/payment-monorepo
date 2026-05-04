import { useCallback, useEffect, useState } from 'react'
import {
  Button, Card, DatePicker, Form, Input, Space, Table, Tag, Typography, Drawer, message,
} from 'antd'
import dayjs, { Dayjs } from 'dayjs'
import { listAudit } from '../../api'
import type { AuditEntry } from '../../api'

// AuditLog is read-only. It reflects the append-only admin_audit_log table
// order-core writes every mutating request to via AuditMiddleware.
// No "delete" or "edit" affordance: audit trails must be tamper-evident.
export default function AuditLogPage() {
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<AuditEntry[]>([])
  const [total, setTotal] = useState(0)
  const [filters, setFilters] = useState<{
    actor?: string; action?: string; target_type?: string; target_id?: string;
    range?: [Dayjs, Dayjs] | null;
  }>({})
  const [detail, setDetail] = useState<AuditEntry | null>(null)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listAudit({
        actor: filters.actor,
        action: filters.action,
        target_type: filters.target_type,
        target_id: filters.target_id,
        since_ms: filters.range?.[0]?.valueOf(),
        until_ms: filters.range?.[1]?.valueOf(),
        limit: pageSize,
        offset: (page - 1) * pageSize,
      })
      setRows(r.items || [])
      setTotal(r.total || 0)
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [filters, page, pageSize])

  useEffect(() => { load() }, [load])

  return (
    <div>
      <Typography.Title level={3}>审计日志</Typography.Title>
      <Card>
        <Form layout="inline" style={{ marginBottom: 16 }} onFinish={(v) => {
          setPage(1)
          setFilters({
            actor: v.actor, action: v.action,
            target_type: v.target_type, target_id: v.target_id,
            range: v.range,
          })
        }}>
          <Form.Item name="actor"><Input placeholder="actor" allowClear /></Form.Item>
          <Form.Item name="action">
            <Input placeholder="action（支持 merchant.* 前缀）" allowClear style={{ width: 220 }} />
          </Form.Item>
          <Form.Item name="target_type"><Input placeholder="target type" allowClear /></Form.Item>
          <Form.Item name="target_id"><Input placeholder="target id" allowClear /></Form.Item>
          <Form.Item name="range"><DatePicker.RangePicker showTime /></Form.Item>
          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit">查询</Button>
              <Button onClick={load}>刷新</Button>
            </Space>
          </Form.Item>
        </Form>
        <Table<AuditEntry>
          rowKey="id"
          size="small"
          loading={loading}
          dataSource={rows}
          pagination={{
            current: page, pageSize, total,
            onChange: (p, ps) => { setPage(p); setPageSize(ps) },
            showSizeChanger: true, pageSizeOptions: ['20', '50', '100', '200'],
          }}
          columns={[
            { title: '时间', dataIndex: 'created_ms', width: 170, render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss') },
            { title: 'Actor', dataIndex: 'actor', width: 160, ellipsis: true },
            {
              title: 'Action', dataIndex: 'action', width: 220,
              render: (v: string) => <Tag>{v}</Tag>,
            },
            {
              title: 'Target', width: 220,
              render: (_, r) => r.target_type ? `${r.target_type} / ${r.target_id || '-'}` : '-',
            },
            {
              title: 'HTTP', width: 180,
              render: (_, r) => r.http_method
                ? `${r.http_method} ${r.http_path} · ${r.http_status ?? ''}`
                : '-',
            },
            { title: 'Dur', dataIndex: 'duration_ms', width: 70, render: (v: number) => v ? `${v}ms` : '-' },
            {
              title: '', width: 70,
              render: (_, r) => <a onClick={() => setDetail(r)}>详情</a>,
            },
          ]}
        />
      </Card>

      <Drawer
        title={detail ? `#${detail.id} ${detail.action}` : ''}
        width={720}
        open={!!detail}
        onClose={() => setDetail(null)}
      >
        {detail && (
          <div>
            <Typography.Paragraph>
              <Typography.Text strong>Actor: </Typography.Text>{detail.actor}
              {detail.actor_ip && <> · <Typography.Text code>{detail.actor_ip}</Typography.Text></>}
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Typography.Text strong>Target: </Typography.Text>
              {detail.target_type || '-'} / {detail.target_id || '-'}
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Typography.Text strong>HTTP: </Typography.Text>
              {detail.http_method} {detail.http_path} → {detail.http_status}
              {detail.duration_ms != null && ` (${detail.duration_ms}ms)`}
            </Typography.Paragraph>
            <Typography.Title level={5}>Request body</Typography.Title>
            <Input.TextArea value={detail.request_body || '-'} readOnly autoSize={{ minRows: 3, maxRows: 12 }}
              style={{ fontFamily: 'monospace', fontSize: 12 }} />
            {detail.response_msg && (
              <>
                <Typography.Title level={5} style={{ marginTop: 12 }}>Response</Typography.Title>
                <Input.TextArea value={detail.response_msg} readOnly autoSize={{ minRows: 2, maxRows: 10 }}
                  style={{ fontFamily: 'monospace', fontSize: 12 }} />
              </>
            )}
            <Typography.Text type="secondary" style={{ display: 'block', marginTop: 16 }}>
              审计日志不可修改/删除（合规要求保留 ≥7 年）。
            </Typography.Text>
          </div>
        )}
      </Drawer>
    </div>
  )
}
