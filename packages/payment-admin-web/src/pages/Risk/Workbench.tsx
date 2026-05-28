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
  Alert, Button, Card, Drawer, Form, Input, Modal, Radio, Select, Space, Table,
  Tabs, Tag, Timeline, Tooltip, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import {
  listReviews, claimReview, releaseReview, escalateReview, addReviewNote,
  decideReview, getReview, myAssignedReviews, overdueReviews,
} from '../../api/risk'
import type { ReviewItem, ReviewStatus, ReviewAction } from '../../api/risk'
import { display } from '../../utils/money'
import { request } from '../../api/client'

// ── 结构化 ReasonCode（后端 enum，前端通过 GET /risk/reviews/reason-codes 拉）─
//
// 字典初次加载时缓存到 module-level；用户切语言不需要重拉（zh / en 都在 payload）。
// 后端新增 code → 前端无需改代码，下拉自动出现。
type ReasonCodeLabel = {
  code: string
  zh_CN: string
  en_US: string
  hint_free_text?: boolean
}
let reasonCodeCache: ReasonCodeLabel[] | null = null
async function fetchReasonCodes(): Promise<ReasonCodeLabel[]> {
  if (reasonCodeCache) return reasonCodeCache
  const r = await request<ReasonCodeLabel[]>({
    url: '/risk/reviews/reason-codes', method: 'GET',
  })
  reasonCodeCache = Array.isArray(r) ? r : []
  return reasonCodeCache
}

// ── case Level 渲染 ────────────────────────────────────────────────────────
// 后端 Item 加了 level 字段（1=L1, 2=L2）+ escalate_hist 时间轴 + sla_escalated 标记。
// TS 类型这里临时 extend；正式应去 api/risk.ts 加（TODO）。
type CaseLevel = 1 | 2
type ExtendedReviewItem = ReviewItem & {
  level?: CaseLevel
  reason_code?: string
  sla_escalated?: boolean
  escalate_hist?: Array<{
    from_level: number
    to_level: number
    trigger: 'manual' | 'sla_timeout'
    actor?: string
    reason?: string
    created_at: string
  }>
  transfer_hist?: Array<{
    from: string
    to: string
    reason?: string
    created_at: string
  }>
}
function LevelTag({ level, slaEscalated }: { level?: number; slaEscalated?: boolean }) {
  const lv = level && level >= 2 ? 2 : 1
  return (
    <Tag color={lv === 2 ? 'magenta' : 'blue'}>
      L{lv}
      {slaEscalated ? ' (auto)' : ''}
    </Tag>
  )
}

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
  const { t, i18n } = useTranslation('risk')
  const [actor, setActor] = useActor()
  const [tab, setTab] = useState<'mine' | 'queue' | 'overdue'>('mine')

  const [rows, setRows] = useState<ReviewItem[]>([])
  const [loading, setLoading] = useState(false)
  const [detail, setDetail] = useState<ExtendedReviewItem | null>(null)
  const [decideOpen, setDecideOpen] = useState(false)
  const [decideForm] = Form.useForm<{ action: ReviewAction; reason_code: string; reason: string }>()
  const [reasonCodes, setReasonCodes] = useState<ReasonCodeLabel[]>([])
  const [reasonCodeWatched, setReasonCodeWatched] = useState<string>('')

  // 初次挂载拉 reason code 字典；失败不阻塞页面（degrade 成无下拉，user 改回 free-text）
  useEffect(() => {
    fetchReasonCodes()
      .then((list) => setReasonCodes(list))
      .catch((e) => {
        console.warn('reason codes fetch failed:', e)
        setReasonCodes([])
      })
  }, [])

  // 当前语言（zh / en）。
  const lang = (i18n.language || 'zh').toLowerCase().startsWith('en') ? 'en_US' : 'zh_CN'

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

  const onDecideSubmit = async (v: { action: ReviewAction; reason_code: string; reason: string }) => {
    if (!detail || !requireActor()) return
    if (!v.reason_code) {
      message.warning(t('workbench.decideModal.reasonCodeRequired', 'Reason code required'))
      return
    }
    try {
      // 透传 reason_code 给后端。decideReview 当前签名不含 reason_code；
      // TODO(api/risk.ts): 把 reasonCode 加入 decideReview 入参类型。临时 cast 走。
      await decideReview({
        id: detail.id,
        action: v.action,
        actor,
        reason: v.reason,
        // @ts-expect-error reason_code 未在 type 里；后端 v2 必须；待 api/risk.ts 升级
        reason_code: v.reason_code,
      })
      message.success(v.action === 'approve' ? t('workbench.approved') : t('workbench.rejected'))
      setDecideOpen(false)
      decideForm.resetFields()
      setReasonCodeWatched('')
      await refreshDetail(detail.id)
      load()
    } catch (e) { message.error(String(e)) }
  }

  // ── Transfer 弹窗（v1 极简：目标 analyst id + reason） ────────────────
  const onTransfer = async (id: string) => {
    if (!requireActor()) return
    // TODO(ui): 用 Modal + Form 控件而非 prompt；v1 先打通端到端流程。
    const target = window.prompt(t('workbench.transferPrompt', 'Transfer to which analyst? (id)') || '')
    if (!target) return
    const reason = window.prompt(t('workbench.transferReasonPrompt', 'Reason?') || '') || ''
    try {
      await request({
        url: '/risk/reviews/transfer', method: 'POST',
        data: { case_id: id, to_actor: target, reason, from_actor: actor },
      })
      message.success(t('workbench.transferred', 'Transferred'))
      await refreshDetail(id)
      load()
    } catch (e) { message.error(String(e)) }
  }

  // ── 显式 EscalateTo(L2) ──────────────────────────────────────────────
  // 区别于旧 onEscalate（只 bump 计数）：这里走 /escalate-level，真升 L2。
  const onEscalateLevel = async (id: string) => {
    if (!requireActor()) return
    Modal.confirm({
      title: t('workbench.escalateLevelModal.title', 'Escalate to L2?'),
      content: t('workbench.escalateLevelModal.content', 'Case will be routed to senior analyst queue.'),
      onOk: async () => {
        try {
          await request({
            url: '/risk/reviews/escalate-level', method: 'POST',
            data: { case_id: id, target_level: 2, reason: 'manual', actor },
          })
          message.success(t('workbench.escalatedL2', 'Escalated to L2'))
          await refreshDetail(id)
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
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
        title: t('workbench.columns.level', 'Level'), width: 90,
        render: (_: unknown, r: ReviewItem) => {
          const e = r as ExtendedReviewItem
          return <LevelTag level={e.level} slaEscalated={e.sla_escalated} />
        },
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
                  <Button onClick={() => onTransfer(detail.id)}>{t('workbench.drawer.transfer', 'Transfer')}</Button>
                  {(detail.level || 1) < 2 && (
                    <Button onClick={() => onEscalateLevel(detail.id)}>
                      {t('workbench.drawer.escalateL2', 'Escalate L2')}
                    </Button>
                  )}
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
              <p>
                <b>{t('workbench.drawer.level', 'Level')}：</b>
                <LevelTag level={detail.level} slaEscalated={detail.sla_escalated} />
              </p>
              {detail.reason_code && (
                <p>
                  <b>{t('workbench.drawer.reasonCode', 'Reason code')}：</b>
                  <Tag>{detail.reason_code}</Tag>
                </p>
              )}
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

            {(detail.escalate_hist?.length || 0) > 0 && (
              <Card
                size="small"
                title={t('workbench.drawer.escalateHist', 'Escalate history')}
                style={{ marginBottom: 12 }}
              >
                <Timeline
                  items={(detail.escalate_hist || []).map((h) => ({
                    color: h.trigger === 'sla_timeout' ? 'red' : 'blue',
                    children: (
                      <div>
                        <Space>
                          <Tag color={h.trigger === 'sla_timeout' ? 'red' : 'blue'}>
                            {h.trigger}
                          </Tag>
                          <span>L{h.from_level} → L{h.to_level}</span>
                          <Typography.Text type="secondary">
                            {dayjs(h.created_at).format('YYYY-MM-DD HH:mm:ss')}
                          </Typography.Text>
                        </Space>
                        <div style={{ marginTop: 4 }}>
                          <Typography.Text type="secondary">{h.actor || 'system'}</Typography.Text>
                          {h.reason ? <span>：{h.reason}</span> : null}
                        </div>
                      </div>
                    ),
                  }))}
                />
              </Card>
            )}

            {(detail.transfer_hist?.length || 0) > 0 && (
              <Card
                size="small"
                title={t('workbench.drawer.transferHist', 'Transfer history')}
                style={{ marginBottom: 12 }}
              >
                {(detail.transfer_hist || []).map((tr, i) => (
                  <div key={i} style={{ padding: '4px 0' }}>
                    <Typography.Text>{tr.from}</Typography.Text>
                    <span> → </span>
                    <Typography.Text strong>{tr.to}</Typography.Text>
                    <Typography.Text type="secondary" style={{ marginLeft: 12 }}>
                      {dayjs(tr.created_at).format('YYYY-MM-DD HH:mm:ss')}
                    </Typography.Text>
                    {tr.reason ? <span>：{tr.reason}</span> : null}
                  </div>
                ))}
              </Card>
            )}

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
          {/* 结构化 Reason Code 下拉。后端 v2 强制必传；空提交后端 400。
              选 "other" 时强制要求 free-text 详述（前端只提示，后端不再二次校验）。*/}
          <Form.Item
            label={t('workbench.decideModal.reasonCodeLabel', 'Reason code')}
            name="reason_code"
            rules={[{ required: true, message: t('workbench.decideModal.reasonCodeRequired', 'Required') }]}
          >
            <Select
              showSearch
              placeholder={t('workbench.decideModal.reasonCodePlaceholder', 'Select a reason')}
              onChange={(v: string) => setReasonCodeWatched(v)}
              options={reasonCodes.map((rc) => ({
                value: rc.code,
                label: `${lang === 'en_US' ? rc.en_US : rc.zh_CN}${rc.hint_free_text ? ' *' : ''}`,
              }))}
              filterOption={(input, option) =>
                String(option?.label || '').toLowerCase().includes(input.toLowerCase())
              }
            />
          </Form.Item>
          <Form.Item
            label={t('workbench.decideModal.reasonLabel')}
            name="reason"
            rules={
              reasonCodeWatched === 'other'
                ? [{ required: true, message: t('workbench.decideModal.reasonRequiredForOther', 'Free text required when code=other') }]
                : []
            }
          >
            <Input.TextArea rows={3} placeholder={t('workbench.decideModal.reasonPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
