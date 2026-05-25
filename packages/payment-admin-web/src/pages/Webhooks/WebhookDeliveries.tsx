import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('channel')
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
      message.success(t('deliveries.messages.retryQueued'))
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  const onTestSend = async (v: { merchant_id: string; event_type?: string; payload?: string }) => {
    try {
      // Validate payload is JSON.
      if (v.payload) {
        try { JSON.parse(v.payload) } catch { message.error(t('deliveries.messages.invalidPayloadJson')); return }
      }
      const r = await testWebhookSend(v)
      setTestOpen(false)
      testForm.resetFields()
      message.success(t('deliveries.messages.testSent', { id: r.delivery.id }))
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  return (
    <div>
      <Typography.Title level={3}>
        {t('deliveries.title')}
        <Typography.Text type="secondary" style={{ fontSize: 14, marginLeft: 12 }}>
          {t('deliveries.subtitle')}
        </Typography.Text>
      </Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message={t('deliveries.signatureHint')}
        />
        <Space wrap style={{ marginBottom: 16 }}>
          <Input
            placeholder={t('deliveries.filters.merchantIdPlaceholder')} allowClear style={{ width: 220 }}
            onPressEnter={(e) => setFilters((f) => ({ ...f, merchant_id: (e.target as HTMLInputElement).value }))}
          />
          <Select
            placeholder={t('deliveries.filters.statusPlaceholder')} allowClear style={{ width: 160 }}
            onChange={(v) => { setPage(1); setFilters((f) => ({ ...f, status: v })) }}
            options={['pending', 'succeeded', 'failed', 'exhausted'].map((s) => ({ value: s, label: s }))}
          />
          <Button onClick={load}>{t('common:actions.refresh')}</Button>
          <Button type="primary" onClick={() => setTestOpen(true)}>{t('deliveries.actions.sendTest')}</Button>
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
            { title: t('deliveries.columns.id'), dataIndex: 'id', width: 80 },
            { title: t('deliveries.columns.time'), dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
            { title: t('deliveries.columns.merchant'), dataIndex: 'merchant_id', width: 140, ellipsis: true },
            { title: t('deliveries.columns.event'), dataIndex: 'event_type', width: 220, ellipsis: true, render: (v: string) => <Tag>{v}</Tag> },
            {
              title: t('deliveries.columns.status'), dataIndex: 'status', width: 110,
              render: (v: string, r) => (
                <Space size={4}>
                  <Tag color={STATUS_COLORS[v] || 'default'}>{v}</Tag>
                  <Typography.Text type="secondary">{r.attempts}/{r.max_attempts}</Typography.Text>
                </Space>
              ),
            },
            { title: t('deliveries.columns.http'), dataIndex: 'http_status', width: 80 },
            {
              title: t('deliveries.columns.nextRetry'), dataIndex: 'next_retry_ms', width: 160,
              render: (v?: number) => v ? dayjs(v).format('MM-DD HH:mm:ss') : '-',
            },
            {
              title: '', width: 180,
              render: (_, r) => (
                <Space size={4}>
                  <a onClick={() => setDetail(r)}>{t('deliveries.actions.detail')}</a>
                  {r.status !== 'succeeded' && (
                    <a onClick={() => onRetry(r.id)}>{t('deliveries.actions.retryNow')}</a>
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
              <Typography.Text strong>{t('deliveries.drawer.url')}</Typography.Text>
              <Typography.Text code copyable>{detail.url}</Typography.Text>
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Typography.Text strong>{t('deliveries.drawer.event')}</Typography.Text>
              {detail.event_type} · <Typography.Text code>{detail.event_id}</Typography.Text>
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Typography.Text strong>{t('deliveries.drawer.status')}</Typography.Text>
              <Tag color={STATUS_COLORS[detail.status]}>{detail.status}</Tag>
              {t('deliveries.drawer.attemptsHttp', { attempts: detail.attempts, max: detail.max_attempts, http: detail.http_status })}
            </Typography.Paragraph>
            {detail.last_error && (
              <Alert type="error" message={t('deliveries.drawer.lastErrorTitle')} description={detail.last_error} showIcon style={{ marginBottom: 12 }} />
            )}
            <Typography.Title level={5}>{t('deliveries.drawer.payload')}</Typography.Title>
            <Input.TextArea
              value={formatJSON(detail.payload)} readOnly autoSize={{ minRows: 4, maxRows: 20 }}
              style={{ fontFamily: 'monospace', fontSize: 12 }}
            />
          </div>
        )}
      </Drawer>

      <Modal
        title={t('deliveries.testModal.title')}
        open={testOpen}
        onCancel={() => setTestOpen(false)}
        onOk={() => testForm.submit()}
        width={640}
      >
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message={t('deliveries.testModal.intro')}
        />
        <Form form={testForm} layout="vertical" onFinish={onTestSend}>
          <Form.Item name="merchant_id" label={t('deliveries.testModal.merchantId')} rules={[{ required: true }]}>
            <Input placeholder={t('deliveries.testModal.merchantIdPlaceholder')} />
          </Form.Item>
          <Form.Item name="event_type" label={t('deliveries.testModal.eventType')}>
            <Input placeholder={t('deliveries.testModal.eventTypePlaceholder')} />
          </Form.Item>
          <Form.Item name="payload" label={t('deliveries.testModal.payload')}>
            <Input.TextArea rows={4} placeholder={t('deliveries.testModal.payloadPlaceholder')} />
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
