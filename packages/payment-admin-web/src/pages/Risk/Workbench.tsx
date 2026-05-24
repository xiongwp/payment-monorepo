// Workbench.tsx —— 风控 case 工作台。
//
// 跟 Reviews.tsx 的区别：
//   Reviews   → 列出所有 pending / approved / rejected，按 status 过滤
//   Workbench → 分析师视角，三个 tab：
//                 1) 我的工作台 (claim 过、in_review 状态)
//                 2) 待领取 (pending)
//                 3) 超期未决 (overdue SLA，监控告警来源)
//               每条 case 可 Claim / Release / Escalate / 加备注 / 决议。
//
// 流程：
//   pending → Claim(actor) → in_review → Decide / Escalate / Release
//   in_review → Escalate(actor, reason) → escalated
//   任何阶段 → AddNote 留协作记录
//
// 简化：actor 当前用 prompt 输入；生产应接 admin SSO，从 token 拉 user_id。
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Radio, Space, Table,
  Tabs, Tag, Tooltip, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import {
  listReviews, claimReview, releaseReview, escalateReview, addReviewNote,
  decideReview, getReview, myAssignedReviews, overdueReviews,
} from '../../api/risk'
import type { ReviewItem, ReviewStatus, ReviewAction } from '../../api/risk'
import { display } from '../../utils/money'

const STATUS_COLOR: Record<ReviewStatus, string> = {
  pending: 'processing',
  in_review: 'warning',
  escalated: 'magenta',
  approved: 'success',
  rejected: 'error',
}

// 当前 analyst id；持久化到 localStorage 让分析师不用每次输。
function useActor(): [string, (a: string) => void] {
  const [actor, setActor] = useState<string>(() => localStorage.getItem('risk.actor') || '')
  const set = (a: string) => {
    setActor(a)
    localStorage.setItem('risk.actor', a)
  }
  return [actor, set]
}

export default function Workbench() {
  const { t } = useTranslation('risk')
  const [actor, setActor] = useActor()
  const [tab, setTab] = useState<'mine' | 'queue' | 'overdue'>('mine')

  const [rows, setRows] = useState<ReviewItem[]>([])
  const [loading, setLoading] = useState(false)
  const [detail, setDetail] = useState<ReviewItem | null>(null)
  const [decideOpen, setDecideOpen] = useState(false)
  const [decideForm] = Form.useForm<{ action: ReviewAction; reason: string }>()

  const load = useCallback(async () => {
    if (!actor && tab === 'mine') return
    setLoading(true)
    try {
      let items: ReviewItem[] = []
      if (tab === 'mine') {
        items = await myAssignedReviews({ actor, status: 'in_review', limit: 200 })
      } else if (tab === 'queue') {
        const r = await listReviews({ status: 'pending', limit: 200 })
        items = r.items || []
      } else {
        items = await overdueReviews({ limit: 200 })
      }
      setRows(Array.isArray(items) ? items : [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [tab, actor])

  useEffect(() => { load() }, [load])

  const refreshDetail = async (id: string) => {
    try {
      const fresh = await getReview(id)
      setDetail(fresh)
    } catch (e) { message.error(String(e)) }
  }

  const requireActor = () => {
    if (!actor) {
      message.warning(t('workbench.requireActorWarning'))
      return false
    }
    return true
  }

  const onClaim = async (id: string) => {
    if (!requireActor()) return
    try {
      await claimReview({ id, actor })
      message.success(t('workbench.claimed'))
      await refreshDetail(id)
      load()
    } catch (e) { message.error(String(e)) }
  }

  const onRelease = async (id: string) => {
    if (!requireActor()) return
    try {
      await releaseReview({ id, actor })
      message.success(t('workbench.released'))
      await refreshDetail(id)
      load()
    } catch (e) { message.error(String(e)) }
  }

  const onEscalate = async (id: string) => {
    if (!requireActor()) return
    Modal.confirm({
      title: t('workbench.escalateModal.title'),
      content: (
        <Form layout="vertical" id="escalate-form">
          <Form.Item label={t('workbench.escalateModal.reasonLabel')} name="reason">
            <Input.TextArea rows={2} placeholder={t('workbench.escalateModal.reasonPlaceholder')} />
          </Form.Item>
        </Form>
      ),
      onOk: async () => {
        const reason =
          (document.querySelector('#escalate-form textarea') as HTMLTextAreaElement | null)?.value || ''
        try {
          await escalateReview({ id, actor, reason })
          message.success(t('workbench.escalated'))
          await refreshDetail(id)
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onAddNote = async (id: string) => {
    if (!requireActor()) return
    Modal.confirm({
      title: t('workbench.noteModal.title'),
      content: (
        <Form layout="vertical" id="note-form">
          <Form.Item label={t('workbench.noteModal.bodyLabel')} name="body" rules={[{ required: true }]}>
            <Input.TextArea rows={3} />
          </Form.Item>
        </Form>
      ),
      onOk: async () => {
        const body =
          (document.querySelector('#note-form textarea') as HTMLTextAreaElement | null)?.value || ''
        if (!body) return
        try {
          await addReviewNote({ id, actor, body })
          message.success(t('workbench.noteAdded'))
          await refreshDetail(id)
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onDecideSubmit = async (v: { action: ReviewAction; reason: string }) => {
    if (!detail || !requireActor()) return
    try {
      await decideReview({ id: detail.id, action: v.action, actor, reason: v.reason })
      message.success(v.action === 'approve' ? t('workbench.approved') : t('workbench.rejected'))
      setDecideOpen(false)
      decideForm.resetFields()
      await refreshDetail(detail.id)
      load()
    } catch (e) { message.error(String(e)) }
  }

  const columns = useMemo(
    () => [
      {
        title: t('workbench.columns.decisionId'), dataIndex: 'id', width: 200, ellipsis: true,
        render: (v: string) => <Typography.Text code>{v.slice(0, 12)}…</Typography.Text>,
      },
      { title: t('workbench.columns.merchant'), dataIndex: 'merchant_id', width: 120, ellipsis: true },
      { title: t('workbench.columns.customer'), dataIndex: 'customer_id', width: 120, ellipsis: true },
      {
        title: t('workbench.columns.amount'), width: 120,
        render: (_: unknown, r: ReviewItem) =>
          r.amount ? display(r.amount, r.currency) : '—',
      },
      {
        title: t('workbench.columns.riskScore'), dataIndex: 'risk_score', width: 80,
        render: (v: number) => (
          <Tag color={v >= 80 ? 'red' : v >= 50 ? 'volcano' : v >= 20 ? 'gold' : 'default'}>
            {v}
          </Tag>
        ),
      },
      {
        title: t('workbench.columns.status'), dataIndex: 'status', width: 100,
        render: (s: ReviewStatus) => <Tag color={STATUS_COLOR[s] || 'default'}>{s}</Tag>,
      },
      {
        title: t('workbench.columns.assignee'), dataIndex: 'assigned_to', width: 120,
        render: (v?: string) => v || <Typography.Text type="secondary">—</Typography.Text>,
      },
      {
        title: t('workbench.columns.sla'), dataIndex: 'sla_deadline', width: 140,
        render: (v?: string) => {
          if (!v) return '—'
          const d = dayjs(v)
          const diffMin = d.diff(dayjs(), 'minute')
          const overdue = diffMin < 0
          const label = overdue
            ? t('workbench.slaOverdue', { minutes: Math.abs(diffMin) })
            : diffMin > 60
              ? t('workbench.slaInHours', { hours: Math.round(diffMin / 60) })
              : t('workbench.slaInMinutes', { minutes: diffMin })
          return (
            <Tooltip title={d.format('YYYY-MM-DD HH:mm:ss')}>
              <Tag color={overdue ? 'red' : diffMin < 60 ? 'orange' : 'default'}>{label}</Tag>
            </Tooltip>
          )
        },
      },
      {
        title: t('workbench.columns.actions'), width: 80,
        render: (_: unknown, r: ReviewItem) => (
          <Button size="small" type="link" onClick={() => setDetail(r)}>
            {t('workbench.openButton')}
          </Button>
        ),
      },
    ],
    [t],
  )

  return (
    <div>
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 12 }}>
        <Typography.Title level={3} style={{ margin: 0 }}>{t('workbench.title')}</Typography.Title>
        <Space>
          <Typography.Text type="secondary">{t('workbench.currentAnalystLabel')}</Typography.Text>
          <Input
            placeholder={t('workbench.analystPlaceholder')}
            value={actor}
            onChange={(e) => setActor(e.target.value)}
            style={{ width: 160 }}
          />
        </Space>
      </Space>

      <Card>
        {!actor && (
          <Alert
            type="warning" showIcon style={{ marginBottom: 12 }}
            message={t('workbench.noActorWarning')}
          />
        )}
        <Tabs
          activeKey={tab}
          onChange={(k) => setTab(k as 'mine' | 'queue' | 'overdue')}
          items={[
            { key: 'mine', label: t('workbench.tabs.mine') },
            { key: 'queue', label: t('workbench.tabs.queue') },
            { key: 'overdue', label: t('workbench.tabs.overdue') },
          ]}
        />
        <Space style={{ marginBottom: 12 }}>
          <Button onClick={load}>{t('common:actions.refresh')}</Button>
          <Typography.Text type="secondary">{t('workbench.totalRows', { count: rows.length })}</Typography.Text>
        </Space>
        <Table<ReviewItem>
          rowKey="id" size="small" loading={loading} dataSource={rows}
          pagination={{ pageSize: 25 }}
          columns={columns}
        />
      </Card>

      <Drawer
        title={t('workbench.drawer.title')} width={680} open={!!detail} onClose={() => setDetail(null)}
        extra={
          detail && (
            <Space>
              {detail.status === 'pending' && (
                <Button type="primary" onClick={() => onClaim(detail.id)}>{t('workbench.drawer.claim')}</Button>
              )}
              {detail.status === 'in_review' && detail.assigned_to === actor && (
                <>
                  <Button onClick={() => onRelease(detail.id)}>{t('workbench.drawer.release')}</Button>
                  <Button onClick={() => onEscalate(detail.id)}>{t('workbench.drawer.escalate')}</Button>
                  <Button type="primary" onClick={() => setDecideOpen(true)}>{t('workbench.drawer.decide')}</Button>
                </>
              )}
              {(detail.status === 'in_review' || detail.status === 'escalated' || detail.status === 'pending') && (
                <Button onClick={() => onAddNote(detail.id)}>{t('workbench.drawer.addNote')}</Button>
              )}
            </Space>
          )
        }
      >
        {detail && (
          <>
            <Card size="small" title={t('workbench.drawer.basicInfo')} style={{ marginBottom: 12 }}>
              <p><b>{t('workbench.drawer.id')}：</b><Typography.Text code copyable>{detail.id}</Typography.Text></p>
              <p><b>{t('workbench.drawer.status')}：</b><Tag color={STATUS_COLOR[detail.status] || 'default'}>{detail.status}</Tag></p>
              <p><b>{t('workbench.drawer.assignedTo')}：</b>{detail.assigned_to || '—'}</p>
              <p><b>{t('workbench.drawer.riskScore')}：</b>{detail.risk_score}</p>
              <p><b>{t('workbench.drawer.amount')}：</b>{detail.amount ? display(detail.amount, detail.currency) : '—'}</p>
              <p><b>{t('workbench.drawer.merchant')}：</b>{detail.merchant_id} <b style={{ marginLeft: 16 }}>{t('workbench.drawer.customer')}：</b>{detail.customer_id}</p>
              <p><b>{t('workbench.drawer.created')}：</b>{dayjs(detail.created_at).format('YYYY-MM-DD HH:mm:ss')}</p>
              <p><b>{t('workbench.drawer.sla')}：</b>{detail.sla_deadline ? dayjs(detail.sla_deadline).format('YYYY-MM-DD HH:mm:ss') : '—'}</p>
              {detail.escalate_level ? <p><b>{t('workbench.drawer.escalateLevel')}：</b>{detail.escalate_level}</p> : null}
              {detail.decided_at && (
                <>
                  <p><b>{t('workbench.drawer.decidedAt')}：</b>{dayjs(detail.decided_at).format('YYYY-MM-DD HH:mm:ss')}</p>
                  <p><b>{t('workbench.drawer.decidedBy')}：</b>{detail.decided_by}</p>
                  <p><b>{t('workbench.drawer.decideReason')}：</b>{detail.decide_reason || '—'}</p>
                </>
              )}
            </Card>

            <Card size="small" title={t('workbench.drawer.hitRules')} style={{ marginBottom: 12 }}>
              {(detail.reasons?.length || 0) === 0
                ? <Typography.Text type="secondary">{t('common.none')}</Typography.Text>
                : <ul>{detail.reasons.map((r, i) => <li key={i}>{r}</li>)}</ul>}
            </Card>

            <Card size="small" title={t('workbench.drawer.noteCount', { count: detail.notes?.length || 0 })}>
              {(detail.notes?.length || 0) === 0 ? (
                <Typography.Text type="secondary">{t('workbench.drawer.noNotes')}</Typography.Text>
              ) : (
                <div>
                  {detail.notes!.map((n, i) => (
                    <div key={i} style={{ borderBottom: '1px solid #f0f0f0', padding: '8px 0' }}>
                      <Space>
                        <Typography.Text strong>{n.actor}</Typography.Text>
                        <Typography.Text type="secondary">
                          {dayjs(n.created_at).format('YYYY-MM-DD HH:mm:ss')}
                        </Typography.Text>
                      </Space>
                      <div style={{ marginTop: 4, whiteSpace: 'pre-wrap' }}>{n.body}</div>
                    </div>
                  ))}
                </div>
              )}
            </Card>
          </>
        )}
      </Drawer>

      <Modal
        title={t('workbench.decideModal.title')} open={decideOpen} onCancel={() => setDecideOpen(false)}
        onOk={() => decideForm.submit()}
      >
        <Form form={decideForm} layout="vertical" onFinish={onDecideSubmit}>
          <Form.Item label={t('workbench.decideModal.actionLabel')} name="action" rules={[{ required: true }]}>
            <Radio.Group>
              <Radio.Button value="approve">{t('workbench.decideModal.approve')}</Radio.Button>
              <Radio.Button value="reject">{t('workbench.decideModal.reject')}</Radio.Button>
            </Radio.Group>
          </Form.Item>
          <Form.Item label={t('workbench.decideModal.reasonLabel')} name="reason">
            <Input.TextArea rows={3} placeholder={t('workbench.decideModal.reasonPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
