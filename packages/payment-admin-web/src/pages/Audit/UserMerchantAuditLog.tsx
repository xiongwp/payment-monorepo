import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Space, Table, Tag, Tooltip, Typography, message,
} from 'antd'
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('audit')
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
      <Typography.Title level={3}>{t('userMerchantAudit.title')}</Typography.Title>
      <Typography.Paragraph type="secondary">
        {t('userMerchantAudit.subtitle')}
      </Typography.Paragraph>
      {!chainOk.ok && (
        <Alert
          type="error"
          message={t('userMerchantAudit.chainBroken', { idx: chainOk.idx })}
          style={{ marginBottom: 12 }}
        />
      )}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Form
          layout="inline"
          onFinish={(v) => setFilters(v)}
          initialValues={filters}
        >
          <Form.Item name="actor" label={t('userMerchantAudit.filters.actorLabel')}>
            <Input placeholder={t('userMerchantAudit.filters.actorPlaceholder')} allowClear />
          </Form.Item>
          <Form.Item name="target" label={t('userMerchantAudit.filters.targetLabel')}>
            <Input placeholder={t('userMerchantAudit.filters.targetPlaceholder')} allowClear />
          </Form.Item>
          <Space>
            <Button type="primary" htmlType="submit">{t('common:actions.search')}</Button>
            <Button onClick={() => setFilters({})}>{t('userMerchantAudit.filters.clear')}</Button>
            <Button onClick={() => load()}>{t('common:actions.refresh')}</Button>
          </Space>
        </Form>
      </Card>
      <Table<UserMerchantAuditEntry>
        size="small"
        rowKey="id"
        loading={loading}
        dataSource={rows}
        pagination={{ pageSize: limit, showTotal: (tot) => t('userMerchantAudit.totalSuffix', { count: tot }) }}
        onRow={(r) => ({ onClick: () => setDetail(r) })}
        columns={[
          { title: t('userMerchantAudit.columns.time'), dataIndex: 'created_ms', width: 170, render: (v: number) => dayjs(v).format('YYYY-MM-DD HH:mm:ss.SSS') },
          { title: t('userMerchantAudit.columns.actor'), dataIndex: 'actor', width: 140, ellipsis: true },
          { title: t('userMerchantAudit.columns.ip'), dataIndex: 'actor_ip', width: 140, ellipsis: true },
          {
            title: t('userMerchantAudit.columns.method'),
            dataIndex: 'method',
            ellipsis: true,
            render: (m: string) => <Tooltip title={m}><code>{m.split('/').pop()}</code></Tooltip>,
          },
          { title: t('userMerchantAudit.columns.target'), dataIndex: 'target_id', width: 140, ellipsis: true },
          {
            title: t('userMerchantAudit.columns.status'),
            dataIndex: 'status_code',
            width: 100,
            render: (s: string) => s === 'OK' ? <Tag color="green">OK</Tag> : <Tag color="red">{s}</Tag>,
          },
          { title: t('userMerchantAudit.columns.duration'), dataIndex: 'duration_ms', width: 70, render: (d) => d != null ? `${d}ms` : '-' },
        ]}
      />
      <Drawer
        open={!!detail}
        onClose={() => setDetail(null)}
        title={t('userMerchantAudit.drawer.title', { id: detail?.id ?? '' })}
        width={640}
      >
        {detail && (
          <>
            <Field label={t('userMerchantAudit.drawer.method')} value={detail.method} />
            <Field label={t('userMerchantAudit.drawer.actor')} value={`${detail.actor} (${detail.actor_ip || '-'})`} />
            <Field label={t('userMerchantAudit.drawer.target')} value={detail.target_id || '-'} />
            <Field label={t('userMerchantAudit.drawer.status')} value={`${detail.status_code}${detail.response_err ? ': ' + detail.response_err : ''}`} />
            <Field label={t('userMerchantAudit.drawer.duration')} value={detail.duration_ms != null ? `${detail.duration_ms}ms` : '-'} />
            <Field label={t('userMerchantAudit.drawer.trace')} value={detail.trace_id || '-'} mono />
            <Field label={t('userMerchantAudit.drawer.prevHash')} value={detail.prev_hash || t('userMerchantAudit.drawer.genesis')} mono />
            <Field label={t('userMerchantAudit.drawer.rowHash')} value={detail.row_hash} mono />
            <Typography.Title level={5} style={{ marginTop: 16 }}>{t('userMerchantAudit.drawer.requestBody')}</Typography.Title>
            <pre style={{ background: '#f5f5f5', padding: 12, maxHeight: 320, overflow: 'auto' }}>
              {detail.request_body || t('userMerchantAudit.drawer.empty')}
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
