import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import {
  listWebhookDeliveries, retryWebhookDelivery, testWebhookSend,
} from '../../api'
import type { WebhookDelivery } from '../../api'

const STATUS_COLORS: Record<string, string> = {
  pending: 'processing',
  succeeded: 'success',
  failed: 'warning',
  exhausted: 'error',
}

export default function WebhookDeliveriesPage() {
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<WebhookDelivery[]>([])
  const [total, setTotal] = useState(0)
  const [filters, setFilters] = useState<{ merchant_id?: string; status?: string }>({})
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)
  const [detail, setDetail] = useState<WebhookDelivery | null>(null)
  const [testOpen, setTestOpen] = useState(false)
  const [testForm] = Form.useForm()

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listWebhookDeliveries({
        ...filters, limit: pageSize, offset: (page - 1) * pageSize,
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

  const onRetry = async (id: number) => {
    try {
      await retryWebhookDelivery(id)
      message.success('已入队重试')
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  const onTestSend = async (v: { merchant_id: string; event_type?: string; payload?: string }) => {
    try {
      // Validate payload is JSON.
      if (v.payload) {
        try { JSON.parse(v.payload) } catch { message.error('payload 必须是合法 JSON'); return }
      }
      const r = await testWebhookSend(v)
      setTestOpen(false)
      testForm.resetFields()
      message.success(`已发送测试 webhook，delivery id=${r.delivery.id}`)
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  return (
    <div>
      <Typography.Title level={3}>
        出站 Webhook 投递
        <Typography.Text type="secondary" style={{ fontSize: 14, marginLeft: 12 }}>
          HMAC-SHA256 签名 · 指数退避重试 · 0s/30s/2min/15min/2h
        </Typography.Text>
      </Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message="商户侧用 header X-Signature: t=<unix_ts>,v1=<HMAC-SHA256 hex> 验证；签名内容 = t + '.' + body。"
        />
        <Space wrap style={{ marginBottom: 16 }}>
          <Input
            placeholder="merchant_id" allowClear style={{ width: 220 }}
            onPressEnter={(e) => setFilters((f) => ({ ...f, merchant_id: (e.target as HTMLInputElement).value }))}
          />
          <Select
            placeholder="状态" allowClear style={{ width: 160 }}
            onChange={(v) => { setPage(1); setFilters((f) => ({ ...f, status: v })) }}
            options={['pending', 'succeeded', 'failed', 'exhausted'].map((s) => ({ value: s, label: s }))}
          />
          <Button onClick={load}>刷新</Button>
          <Button type="primary" onClick={() => setTestOpen(true)}>发送测试 Webhook</Button>
        </Space>
        <Table<WebhookDelivery>
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
            { title: 'ID', dataIndex: 'id', width: 80 },
            { title: '时间', dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
            { title: '商户', dataIndex: 'merchant_id', width: 140, ellipsis: true },
            { title: '事件', dataIndex: 'event_type', width: 220, ellipsis: true, render: (v: string) => <Tag>{v}</Tag> },
            {
              title: '状态', dataIndex: 'status', width: 110,
              render: (v: string, r) => (
                <Space size={4}>
                  <Tag color={STATUS_COLORS[v] || 'default'}>{v}</Tag>
                  <Typography.Text type="secondary">{r.attempts}/{r.max_attempts}</Typography.Text>
                </Space>
              ),
            },
            { title: 'HTTP', dataIndex: 'http_status', width: 80 },
            {
              title: '下次重试', dataIndex: 'next_retry_ms', width: 160,
              render: (v?: number) => v ? dayjs(v).format('MM-DD HH:mm:ss') : '-',
            },
            {
              title: '', width: 180,
              render: (_, r) => (
                <Space size={4}>
                  <a onClick={() => setDetail(r)}>详情</a>
                  {r.status !== 'succeeded' && (
                    <a onClick={() => onRetry(r.id)}>立即重试</a>
                  )}
                </Space>
              ),
            },
          ]}
        />
      </Card>

      <Drawer
        title={detail ? `Delivery #${detail.id}` : ''}
        open={!!detail} onClose={() => setDetail(null)} width={820}
      >
        {detail && (
          <div>
            <Typography.Paragraph>
              <Typography.Text strong>URL: </Typography.Text>
              <Typography.Text code copyable>{detail.url}</Typography.Text>
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Typography.Text strong>Event: </Typography.Text>
              {detail.event_type} · <Typography.Text code>{detail.event_id}</Typography.Text>
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Typography.Text strong>Status: </Typography.Text>
              <Tag color={STATUS_COLORS[detail.status]}>{detail.status}</Tag>
              · {detail.attempts}/{detail.max_attempts} attempts · http={detail.http_status}
            </Typography.Paragraph>
            {detail.last_error && (
              <Alert type="error" message="上一次失败原因" description={detail.last_error} showIcon style={{ marginBottom: 12 }} />
            )}
            <Typography.Title level={5}>Payload</Typography.Title>
            <Input.TextArea
              value={formatJSON(detail.payload)} readOnly autoSize={{ minRows: 4, maxRows: 20 }}
              style={{ fontFamily: 'monospace', fontSize: 12 }}
            />
          </div>
        )}
      </Drawer>

      <Modal
        title="发送测试 Webhook"
        open={testOpen}
        onCancel={() => setTestOpen(false)}
        onOk={() => testForm.submit()}
        width={640}
      >
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message="会调用目标商户配置的 webhook_url，用其 webhook_secret HMAC-SHA256 签名。"
        />
        <Form form={testForm} layout="vertical" onFinish={onTestSend}>
          <Form.Item name="merchant_id" label="商户 ID" rules={[{ required: true }]}>
            <Input placeholder="mch_xxx" />
          </Form.Item>
          <Form.Item name="event_type" label="事件类型">
            <Input placeholder="webhook.test（默认）" />
          </Form.Item>
          <Form.Item name="payload" label="Payload (JSON)">
            <Input.TextArea rows={4} placeholder='{"message":"hello"}' />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

function formatJSON(raw: string): string {
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}
