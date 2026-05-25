import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Select, Space, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import dayjs from 'dayjs'
import {
  listDisputes, getDispute, listDisputeEvents,
  submitDisputeEvidence, concedeDispute, cancelDispute, simulateDisputeEvent,
} from '../../api'
import type { Dispute, DisputeStatus, DisputeEvent } from '../../api'
import { display } from '../../utils/money'

const STATUS_COLORS: Record<DisputeStatus, string> = {
  DISPUTE_STATUS_NEEDS_RESPONSE: 'red',
  DISPUTE_STATUS_UNDER_REVIEW: 'processing',
  DISPUTE_STATUS_WON: 'success',
  DISPUTE_STATUS_LOST: 'error',
  DISPUTE_STATUS_WARNING_CLOSED: 'default',
  DISPUTE_STATUS_CHARGE_REFUNDED: 'warning',
  DISPUTE_STATUS_CANCELED: 'default',
}

const STATUS_I18N_KEY: Record<DisputeStatus, string> = {
  DISPUTE_STATUS_NEEDS_RESPONSE: 'status.needsResponse',
  DISPUTE_STATUS_UNDER_REVIEW: 'status.underReview',
  DISPUTE_STATUS_WON: 'status.won',
  DISPUTE_STATUS_LOST: 'status.lost',
  DISPUTE_STATUS_WARNING_CLOSED: 'status.warningClosed',
  DISPUTE_STATUS_CHARGE_REFUNDED: 'status.chargeRefunded',
  DISPUTE_STATUS_CANCELED: 'status.canceled',
}

function statusLabel(t: TFunction, s: DisputeStatus): string {
  return t(STATUS_I18N_KEY[s])
}

export default function DisputeList() {
  const { t } = useTranslation('dispute')
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<Dispute[]>([])
  const [filters, setFilters] = useState<{ merchant_id?: string; status?: DisputeStatus }>({})
  const [detail, setDetail] = useState<Dispute | null>(null)

  const load = useCallback(async () => {
    if (!filters.merchant_id) return
    setLoading(true)
    try {
      const r = await listDisputes({ ...filters, limit: 100 })
      setRows(r.disputes || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [filters])

  useEffect(() => { load() }, [load])

  return (
    <div>
      <Typography.Title level={3}>{t('list.title')}</Typography.Title>
      <Card>
        <Alert
          type="warning" showIcon style={{ marginBottom: 16 }}
          message={t('list.warning')}
        />
        <Space wrap style={{ marginBottom: 16 }}>
          <Input
            placeholder={t('list.merchantIdPlaceholder')} allowClear style={{ width: 240 }}
            onPressEnter={(e) => setFilters((f) => ({ ...f, merchant_id: (e.target as HTMLInputElement).value }))}
          />
          <Select
            placeholder={t('list.statusPlaceholder')} allowClear style={{ width: 200 }}
            onChange={(v) => setFilters((f) => ({ ...f, status: v }))}
            options={(Object.keys(STATUS_I18N_KEY) as DisputeStatus[]).map((k) => ({
              value: k, label: statusLabel(t, k),
            }))}
          />
          <Button onClick={load} disabled={!filters.merchant_id}>{t('list.load')}</Button>
        </Space>
        {!filters.merchant_id && (
          <Typography.Text type="secondary">{t('list.merchantRequired')}</Typography.Text>
        )}
        <Table<Dispute>
          rowKey="id"
          size="small"
          loading={loading}
          dataSource={rows}
          pagination={{ pageSize: 20 }}
          columns={[
            { title: t('list.columns.id'), dataIndex: 'id', width: 160, ellipsis: true, render: (v) => <Typography.Text code>{v}</Typography.Text> },
            { title: t('list.columns.time'), dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm') },
            { title: t('list.columns.charge'), dataIndex: 'charge_id', width: 160, ellipsis: true },
            { title: t('list.columns.channel'), dataIndex: 'channel', width: 90, render: (v) => <Tag>{v}</Tag> },
            { title: t('list.columns.amount'), dataIndex: 'amount', width: 110, align: 'right', render: (v: number, r) => display(v, r.currency) },
            { title: t('list.columns.reason'), dataIndex: 'reason', width: 140, render: (v) => <Tag>{v}</Tag> },
            {
              title: t('list.columns.status'), dataIndex: 'status', width: 140,
              render: (v: DisputeStatus) => <Tag color={STATUS_COLORS[v]}>{statusLabel(t, v)}</Tag>,
            },
            {
              title: t('list.columns.due'), dataIndex: 'evidence_due_at_ms', width: 140,
              render: (v?: number) => v ? dayjs(v).format('MM-DD HH:mm') : '-',
            },
            {
              title: '', width: 80,
              render: (_, r) => <a onClick={async () => {
                try {
                  const full = await getDispute(r.payment_intent_id, r.id)
                  setDetail(full)
                } catch (e) { message.error(String(e)) }
              }}>{t('list.columns.detail')}</a>,
            },
          ]}
        />
      </Card>

      <DisputeDetailDrawer
        dispute={detail}
        onClose={() => setDetail(null)}
        onRefresh={async () => {
          if (!detail) return
          try {
            const full = await getDispute(detail.payment_intent_id, detail.id)
            setDetail(full)
          } catch (e) { message.error(String(e)) }
          load()
        }}
      />
    </div>
  )
}

function DisputeDetailDrawer({
  dispute, onClose, onRefresh,
}: { dispute: Dispute | null; onClose: () => void; onRefresh: () => void }) {
  const { t } = useTranslation('dispute')
  const [events, setEvents] = useState<DisputeEvent[]>([])
  const [evidenceOpen, setEvidenceOpen] = useState(false)
  const [evidenceForm] = Form.useForm()
  const [simulateOpen, setSimulateOpen] = useState(false)
  const [simulateForm] = Form.useForm()

  useEffect(() => {
    if (!dispute) { setEvents([]); return }
    listDisputeEvents(dispute.payment_intent_id, dispute.id)
      .then(setEvents)
      .catch((e) => message.error(String(e)))
  }, [dispute])

  if (!dispute) return null

  const onSubmitEvidence = async (v: Record<string, string>) => {
    const evidence: Record<string, string> = {}
    for (const k of Object.keys(v)) {
      if (k !== 'actor' && v[k]) evidence[k] = v[k]
    }
    try {
      await submitDisputeEvidence(dispute.payment_intent_id, dispute.id, { actor: v.actor || 'admin', evidence })
      message.success(t('evidenceModal.submitSuccess'))
      setEvidenceOpen(false)
      evidenceForm.resetFields()
      onRefresh()
    } catch (e) { message.error(String(e)) }
  }

  const onConcede = async () => {
    Modal.confirm({
      title: t('concedeModal.title'),
      content: t('concedeModal.content'),
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await concedeDispute(dispute.payment_intent_id, dispute.id, { actor: 'admin', note: 'conceded via admin' })
          message.success(t('concedeModal.success'))
          onRefresh()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onCancel = async () => {
    try {
      await cancelDispute(dispute.payment_intent_id, dispute.id, { actor: 'admin', note: 'canceled by admin' })
      message.success(t('cancelDispute.success'))
      onRefresh()
    } catch (e) { message.error(String(e)) }
  }

  const onSimulate = async (v: { action: string; outcome_amount?: number }) => {
    try {
      const amt = v.action === 'lost' ? (v.outcome_amount ?? dispute.amount) : undefined
      await simulateDisputeEvent(dispute.payment_intent_id, dispute.id, {
        action: v.action as any,
        outcome_amount: amt,
      })
      message.success(t('simulateModal.success', { action: v.action }))
      setSimulateOpen(false)
      simulateForm.resetFields()
      onRefresh()
    } catch (e) { message.error(String(e)) }
  }

  const isTerminal = [
    'DISPUTE_STATUS_WON', 'DISPUTE_STATUS_LOST', 'DISPUTE_STATUS_WARNING_CLOSED',
    'DISPUTE_STATUS_CHARGE_REFUNDED', 'DISPUTE_STATUS_CANCELED',
  ].includes(dispute.status)

  return (
    <Drawer
      title={t('detail.title', { id: dispute.id })}
      width={820}
      open={!!dispute}
      onClose={onClose}
    >
      <Tabs
        items={[
          {
            key: 'info', label: t('detail.tabs.info'),
            children: (
              <>
                <Space direction="vertical" style={{ width: '100%' }}>
                  <div><Typography.Text strong>{t('detail.fields.status')}: </Typography.Text>
                    <Tag color={STATUS_COLORS[dispute.status]}>{statusLabel(t, dispute.status)}</Tag>
                  </div>
                  <div><Typography.Text strong>{t('detail.fields.amount')}: </Typography.Text>
                    <Typography.Text strong>{display(dispute.amount, dispute.currency)}</Typography.Text>
                    {dispute.outcome_amount != null && dispute.outcome_amount > 0 && (
                      <> · {t('detail.fields.outcome')}: <Typography.Text strong>{display(dispute.outcome_amount, dispute.currency)}</Typography.Text></>
                    )}
                  </div>
                  <div><Typography.Text strong>{t('detail.fields.channel')}: </Typography.Text>{dispute.channel} / <Typography.Text code>{dispute.channel_dispute_id}</Typography.Text></div>
                  <div><Typography.Text strong>{t('detail.fields.charge')}: </Typography.Text><Typography.Text code copyable>{dispute.charge_id}</Typography.Text></div>
                  <div><Typography.Text strong>{t('detail.fields.pi')}: </Typography.Text><Typography.Text code copyable>{dispute.payment_intent_id}</Typography.Text></div>
                  <div><Typography.Text strong>{t('detail.fields.reason')}: </Typography.Text><Tag>{dispute.reason}</Tag> {dispute.reason_detail}</div>
                  {dispute.evidence_due_at_ms && (
                    <div><Typography.Text strong>{t('detail.fields.due')}: </Typography.Text>
                      {dayjs(dispute.evidence_due_at_ms).format('YYYY-MM-DD HH:mm:ss')}
                    </div>
                  )}
                  <div><Typography.Text strong>{t('detail.fields.autoRefund')}: </Typography.Text>
                    <Tag color={dispute.auto_refund_charge ? 'orange' : 'default'}>
                      {dispute.auto_refund_charge ? t('detail.fields.autoRefundYes') : t('detail.fields.autoRefundNo')}
                    </Tag>
                  </div>
                </Space>

                {!isTerminal && (
                  <Card title={t('detail.actions.title')} style={{ marginTop: 16 }}>
                    <Space wrap>
                      <Button type="primary" onClick={() => setEvidenceOpen(true)}>
                        {t('detail.actions.submitEvidence')}
                      </Button>
                      <Button danger onClick={onConcede}>{t('detail.actions.concede')}</Button>
                      <Button onClick={onCancel}>{t('detail.actions.cancel')}</Button>
                      <Button onClick={() => setSimulateOpen(true)}>
                        {t('detail.actions.simulate')}
                      </Button>
                    </Space>
                  </Card>
                )}
              </>
            ),
          },
          {
            key: 'evidence', label: t('detail.tabs.evidence'),
            children: dispute.evidence && Object.keys(dispute.evidence).length
              ? Object.entries(dispute.evidence).map(([k, v]) => (
                <div key={k} style={{ marginBottom: 12 }}>
                  <Typography.Text strong>{k}: </Typography.Text>
                  <Typography.Paragraph copyable style={{ margin: 0 }}>{v}</Typography.Paragraph>
                </div>
              ))
              : <Typography.Text type="secondary">{t('detail.evidenceEmpty')}</Typography.Text>,
          },
          {
            key: 'events', label: t('detail.tabs.events', { count: events.length }),
            children: (
              <Table<DisputeEvent>
                rowKey="id" size="small" pagination={false} dataSource={events}
                columns={[
                  { title: t('detail.eventColumns.time'), dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
                  { title: t('detail.eventColumns.from'), dataIndex: 'from_status', width: 140, render: (v) => <Tag>{v}</Tag> },
                  { title: t('detail.eventColumns.to'), dataIndex: 'to_status', width: 140, render: (v) => <Tag>{v}</Tag> },
                  { title: t('detail.eventColumns.source'), dataIndex: 'source', width: 110 },
                  { title: t('detail.eventColumns.actor'), dataIndex: 'actor', width: 120 },
                  { title: t('detail.eventColumns.note'), dataIndex: 'note' },
                ]}
              />
            ),
          },
        ]}
      />

      <Modal
        title={t('evidenceModal.title')} open={evidenceOpen}
        onCancel={() => setEvidenceOpen(false)} onOk={() => evidenceForm.submit()}
      >
        <Alert type="info" showIcon style={{ marginBottom: 12 }}
          message={t('evidenceModal.alert')} />
        <Form form={evidenceForm} layout="vertical" onFinish={onSubmitEvidence}>
          <Form.Item name="actor" label={t('evidenceModal.fields.actor')} rules={[{ required: true }]}>
            <Input placeholder={t('evidenceModal.fields.actorPlaceholder')} />
          </Form.Item>
          <Form.Item name="customer_communication" label={t('evidenceModal.fields.customerCommunication')}>
            <Input.TextArea rows={2} />
          </Form.Item>
          <Form.Item name="receipt" label={t('evidenceModal.fields.receipt')}><Input /></Form.Item>
          <Form.Item name="shipping_documentation" label={t('evidenceModal.fields.shipping')}><Input /></Form.Item>
          <Form.Item name="service_documentation" label={t('evidenceModal.fields.service')}><Input /></Form.Item>
          <Form.Item name="refund_policy" label={t('evidenceModal.fields.refundPolicy')}><Input /></Form.Item>
          <Form.Item name="uncategorized_text" label={t('evidenceModal.fields.uncategorized')}><Input.TextArea rows={2} /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('simulateModal.title')} open={simulateOpen}
        onCancel={() => setSimulateOpen(false)} onOk={() => simulateForm.submit()}
      >
        <Alert type="warning" showIcon style={{ marginBottom: 12 }}
          message={t('simulateModal.alert')} />
        <Form form={simulateForm} layout="vertical" onFinish={onSimulate} initialValues={{ action: 'under_review' }}>
          <Form.Item name="action" label={t('simulateModal.action')} rules={[{ required: true }]}>
            <Select
              options={[
                { value: 'under_review', label: t('simulateModal.actionOptions.underReview') },
                { value: 'won', label: t('simulateModal.actionOptions.won') },
                { value: 'lost', label: t('simulateModal.actionOptions.lost') },
                { value: 'warning_closed', label: t('simulateModal.actionOptions.warningClosed') },
              ]}
            />
          </Form.Item>
          <Form.Item
            name="outcome_amount"
            label={t('simulateModal.outcomeAmount')}
            tooltip={t('simulateModal.outcomeTooltip', { amount: display(dispute.amount, dispute.currency) })}
          >
            <Input type="number" placeholder={t('simulateModal.outcomePlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </Drawer>
  )
}
