import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Select, Space, Table, Tabs, Tag, Typography, message,
} from 'antd'
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

const STATUS_LABEL: Record<DisputeStatus, string> = {
  DISPUTE_STATUS_NEEDS_RESPONSE: '待响应',
  DISPUTE_STATUS_UNDER_REVIEW: '审核中',
  DISPUTE_STATUS_WON: '商户赢',
  DISPUTE_STATUS_LOST: '商户输',
  DISPUTE_STATUS_WARNING_CLOSED: '仅预警',
  DISPUTE_STATUS_CHARGE_REFUNDED: '已退款关闭',
  DISPUTE_STATUS_CANCELED: '已撤回',
}

export default function DisputeList() {
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
      <Typography.Title level={3}>Disputes / Chargebacks</Typography.Title>
      <Card>
        <Alert
          type="warning" showIcon style={{ marginBottom: 16 }}
          message="Dispute 是渠道发起的争议；needs_response 必须在 deadline 前提交证据，否则默认商户输。lost / concede 会自动触发退款。"
        />
        <Space wrap style={{ marginBottom: 16 }}>
          <Input
            placeholder="商户 ID (必填)" allowClear style={{ width: 240 }}
            onPressEnter={(e) => setFilters((f) => ({ ...f, merchant_id: (e.target as HTMLInputElement).value }))}
          />
          <Select
            placeholder="状态" allowClear style={{ width: 200 }}
            onChange={(v) => setFilters((f) => ({ ...f, status: v }))}
            options={(Object.keys(STATUS_LABEL) as DisputeStatus[]).map((k) => ({
              value: k, label: STATUS_LABEL[k],
            }))}
          />
          <Button onClick={load} disabled={!filters.merchant_id}>加载</Button>
        </Space>
        {!filters.merchant_id && (
          <Typography.Text type="secondary">请先输入商户 ID。</Typography.Text>
        )}
        <Table<Dispute>
          rowKey="id"
          size="small"
          loading={loading}
          dataSource={rows}
          pagination={{ pageSize: 20 }}
          columns={[
            { title: 'ID', dataIndex: 'id', width: 160, ellipsis: true, render: (v) => <Typography.Text code>{v}</Typography.Text> },
            { title: '时间', dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm') },
            { title: 'Charge', dataIndex: 'charge_id', width: 160, ellipsis: true },
            { title: '渠道', dataIndex: 'channel', width: 90, render: (v) => <Tag>{v}</Tag> },
            { title: '金额', dataIndex: 'amount', width: 110, align: 'right', render: (v: number, r) => display(v, r.currency) },
            { title: '原因', dataIndex: 'reason', width: 140, render: (v) => <Tag>{v}</Tag> },
            {
              title: '状态', dataIndex: 'status', width: 140,
              render: (v: DisputeStatus) => <Tag color={STATUS_COLORS[v]}>{STATUS_LABEL[v]}</Tag>,
            },
            {
              title: '截止', dataIndex: 'evidence_due_at_ms', width: 140,
              render: (v?: number) => v ? dayjs(v).format('MM-DD HH:mm') : '-',
            },
            {
              title: '', width: 80,
              render: (_, r) => <a onClick={async () => {
                try {
                  const full = await getDispute(r.payment_intent_id, r.id)
                  setDetail(full)
                } catch (e) { message.error(String(e)) }
              }}>详情</a>,
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
      message.success('已提交证据，状态 → 审核中')
      setEvidenceOpen(false)
      evidenceForm.resetFields()
      onRefresh()
    } catch (e) { message.error(String(e)) }
  }

  const onConcede = async () => {
    Modal.confirm({
      title: '确认对商户让步（全额退款）?',
      content: '此操作会把 dispute → charge_refunded，并对原 charge 触发自动退款。',
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await concedeDispute(dispute.payment_intent_id, dispute.id, { actor: 'admin', note: 'conceded via admin' })
          message.success('已让步')
          onRefresh()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onCancel = async () => {
    try {
      await cancelDispute(dispute.payment_intent_id, dispute.id, { actor: 'admin', note: 'canceled by admin' })
      message.success('已撤回')
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
      message.success(`已模拟渠道事件：${v.action}`)
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
      title={`Dispute ${dispute.id}`}
      width={820}
      open={!!dispute}
      onClose={onClose}
    >
      <Tabs
        items={[
          {
            key: 'info', label: '基本',
            children: (
              <>
                <Space direction="vertical" style={{ width: '100%' }}>
                  <div><Typography.Text strong>状态: </Typography.Text>
                    <Tag color={STATUS_COLORS[dispute.status]}>{STATUS_LABEL[dispute.status]}</Tag>
                  </div>
                  <div><Typography.Text strong>金额: </Typography.Text>
                    <Typography.Text strong>{display(dispute.amount, dispute.currency)}</Typography.Text>
                    {dispute.outcome_amount != null && dispute.outcome_amount > 0 && (
                      <> · outcome: <Typography.Text strong>{display(dispute.outcome_amount, dispute.currency)}</Typography.Text></>
                    )}
                  </div>
                  <div><Typography.Text strong>渠道: </Typography.Text>{dispute.channel} / <Typography.Text code>{dispute.channel_dispute_id}</Typography.Text></div>
                  <div><Typography.Text strong>Charge: </Typography.Text><Typography.Text code copyable>{dispute.charge_id}</Typography.Text></div>
                  <div><Typography.Text strong>PI: </Typography.Text><Typography.Text code copyable>{dispute.payment_intent_id}</Typography.Text></div>
                  <div><Typography.Text strong>原因: </Typography.Text><Tag>{dispute.reason}</Tag> {dispute.reason_detail}</div>
                  {dispute.evidence_due_at_ms && (
                    <div><Typography.Text strong>截止: </Typography.Text>
                      {dayjs(dispute.evidence_due_at_ms).format('YYYY-MM-DD HH:mm:ss')}
                    </div>
                  )}
                  <div><Typography.Text strong>Auto-refund: </Typography.Text>
                    <Tag color={dispute.auto_refund_charge ? 'orange' : 'default'}>
                      {dispute.auto_refund_charge ? '是（lost 时联动退款）' : '否'}
                    </Tag>
                  </div>
                </Space>

                {!isTerminal && (
                  <Card title="操作" style={{ marginTop: 16 }}>
                    <Space wrap>
                      <Button type="primary" onClick={() => setEvidenceOpen(true)}>
                        提交证据
                      </Button>
                      <Button danger onClick={onConcede}>让步（退款关闭）</Button>
                      <Button onClick={onCancel}>撤回</Button>
                      <Button onClick={() => setSimulateOpen(true)}>
                        模拟渠道事件
                      </Button>
                    </Space>
                  </Card>
                )}
              </>
            ),
          },
          {
            key: 'evidence', label: 'Evidence',
            children: dispute.evidence && Object.keys(dispute.evidence).length
              ? Object.entries(dispute.evidence).map(([k, v]) => (
                <div key={k} style={{ marginBottom: 12 }}>
                  <Typography.Text strong>{k}: </Typography.Text>
                  <Typography.Paragraph copyable style={{ margin: 0 }}>{v}</Typography.Paragraph>
                </div>
              ))
              : <Typography.Text type="secondary">尚未提交证据</Typography.Text>,
          },
          {
            key: 'events', label: `事件 (${events.length})`,
            children: (
              <Table<DisputeEvent>
                rowKey="id" size="small" pagination={false} dataSource={events}
                columns={[
                  { title: '时间', dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
                  { title: '从', dataIndex: 'from_status', width: 140, render: (v) => <Tag>{v}</Tag> },
                  { title: '到', dataIndex: 'to_status', width: 140, render: (v) => <Tag>{v}</Tag> },
                  { title: '来源', dataIndex: 'source', width: 110 },
                  { title: 'Actor', dataIndex: 'actor', width: 120 },
                  { title: 'Note', dataIndex: 'note' },
                ]}
              />
            ),
          },
        ]}
      />

      <Modal
        title="提交证据" open={evidenceOpen}
        onCancel={() => setEvidenceOpen(false)} onOk={() => evidenceForm.submit()}
      >
        <Alert type="info" showIcon style={{ marginBottom: 12 }}
          message="证据正文请提前上传到对象存储，这里只记录链接 + 摘要文本。提交后状态 → 审核中。" />
        <Form form={evidenceForm} layout="vertical" onFinish={onSubmitEvidence}>
          <Form.Item name="actor" label="操作人" rules={[{ required: true }]}>
            <Input placeholder="admin user id" />
          </Form.Item>
          <Form.Item name="customer_communication" label="客户沟通记录（URL/文字）">
            <Input.TextArea rows={2} />
          </Form.Item>
          <Form.Item name="receipt" label="收据 URL"><Input /></Form.Item>
          <Form.Item name="shipping_documentation" label="物流证明 URL"><Input /></Form.Item>
          <Form.Item name="service_documentation" label="服务交付证明 URL"><Input /></Form.Item>
          <Form.Item name="refund_policy" label="退款政策 URL/文字"><Input /></Form.Item>
          <Form.Item name="uncategorized_text" label="其他说明"><Input.TextArea rows={2} /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title="模拟渠道 webhook 事件" open={simulateOpen}
        onCancel={() => setSimulateOpen(false)} onOk={() => simulateForm.submit()}
      >
        <Alert type="warning" showIcon style={{ marginBottom: 12 }}
          message="此操作直接让 order-core 执行对应的状态机跃迁，用于 QA / 演示。实际生产中这些都来自渠道 webhook。" />
        <Form form={simulateForm} layout="vertical" onFinish={onSimulate} initialValues={{ action: 'under_review' }}>
          <Form.Item name="action" label="模拟事件" rules={[{ required: true }]}>
            <Select
              options={[
                { value: 'under_review', label: '→ under_review（渠道开始审核）' },
                { value: 'won', label: '→ won（商户赢）' },
                { value: 'lost', label: '→ lost（商户输，会自动退款）' },
                { value: 'warning_closed', label: '→ warning_closed（仅预警）' },
              ]}
            />
          </Form.Item>
          <Form.Item
            name="outcome_amount"
            label="Outcome amount（仅 lost，留空用 dispute 原金额）"
            tooltip={`原金额 ${display(dispute.amount, dispute.currency)}（storage 整数，= minor × 100）`}
          >
            <Input type="number" placeholder="storage 整数（backend convention: minor × 100）" />
          </Form.Item>
        </Form>
      </Modal>
    </Drawer>
  )
}
