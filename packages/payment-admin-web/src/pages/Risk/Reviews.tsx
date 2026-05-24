import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Radio, Select,
  Space, Table, Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import { listReviews, decideReview, decideReviewBulk, getReview } from '../../api/risk'
import type { ReviewItem, ReviewStatus, ReviewAction } from '../../api/risk'
import { display } from '../../utils/money'

const STATUS_COLOR: Record<ReviewStatus, string> = {
  pending: 'processing',
  in_review: 'warning',
  escalated: 'magenta',
  approved: 'success',
  rejected: 'error',
}

const SCORE_COLOR = (n: number) =>
  n >= 80 ? 'red' : n >= 50 ? 'volcano' : n >= 20 ? 'gold' : 'default'

export default function Reviews() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<ReviewItem[]>([])
  const [status, setStatus] = useState<ReviewStatus>('pending')
  const [detail, setDetail] = useState<ReviewItem | null>(null)
  const [decideOpen, setDecideOpen] = useState(false)
  const [selectedIDs, setSelectedIDs] = useState<string[]>([])
  const [bulkSubmitting, setBulkSubmitting] = useState(false)
  const [form] = Form.useForm<{ action: ReviewAction; actor: string; reason: string }>()

  // 卡测试攻击 / 同模式 fraud 时选中 N 行 → 一次 reject。Modal 二次确认。
  const onBulkDecide = (action: ReviewAction) => {
    const ids = [...selectedIDs]
    if (ids.length === 0) {
      message.warning(t('reviews.bulkSelectWarning'))
      return
    }
    let actor = ''
    let reason = ''
    const verb = action === 'approve' ? t('reviews.bulkVerbApprove') : t('reviews.bulkVerbReject')
    Modal.confirm({
      title: t('reviews.bulkConfirmTitle', { verb, count: ids.length }),
      content: (
        <div>
          <Alert
            type={action === 'reject' ? 'warning' : 'info'} showIcon
            message={t('reviews.bulkAlertMessage', { count: ids.length, action })}
            style={{ marginBottom: 12 }}
          />
          <Input
            placeholder={t('reviews.bulkActorPlaceholder')}
            onChange={(e) => { actor = e.target.value.trim() }}
            style={{ marginBottom: 8 }}
          />
          <Input.TextArea
            placeholder={t('reviews.bulkReasonPlaceholder')} rows={3}
            onChange={(e) => { reason = e.target.value }}
          />
        </div>
      ),
      okText: t('reviews.bulkConfirmOkText'), okButtonProps: { danger: action === 'reject' },
      onOk: async () => {
        if (!actor) {
          message.error(t('reviews.bulkActorRequired'))
          return Promise.reject()
        }
        setBulkSubmitting(true)
        try {
          const r = await decideReviewBulk({ ids, action, actor, reason })
          message.success(
            t('reviews.bulkResult', {
              verb,
              success: r.success,
              total: r.total,
              notPending: r.not_pending,
              failed: r.failed,
            }),
          )
          setSelectedIDs([])
          load()
        } catch (e) { message.error(String(e)) }
        finally { setBulkSubmitting(false) }
      },
    })
  }

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listReviews({ status, limit: 200 })
      setRows(r.items || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [status])

  useEffect(() => { load() }, [load])

  const onDecide = async (v: { action: ReviewAction; actor: string; reason: string }) => {
    if (!detail) return
    try {
      await decideReview({ id: detail.id, ...v })
      message.success(v.action === 'approve' ? t('reviews.approvedSingleSuccess') : t('reviews.rejectedSingleSuccess'))
      setDecideOpen(false)
      form.resetFields()
      // 刷新当前详情 + 列表
      const fresh = await getReview(detail.id)
      setDetail(fresh)
      load()
    } catch (e) { message.error(String(e)) }
  }

  return (
    <div>
      <Typography.Title level={3}>{t('reviews.title')}</Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message={t('reviews.infoAlert')}
        />
        <Space style={{ marginBottom: 16 }} wrap>
          <Radio.Group value={status} onChange={(e) => { setStatus(e.target.value); setSelectedIDs([]) }}>
            <Radio.Button value="pending">{t('reviews.statusFilter.pending')}</Radio.Button>
            <Radio.Button value="approved">{t('reviews.statusFilter.approved')}</Radio.Button>
            <Radio.Button value="rejected">{t('reviews.statusFilter.rejected')}</Radio.Button>
          </Radio.Group>
          <Button onClick={load}>{t('common:actions.refresh')}</Button>
          <Typography.Text type="secondary">
            {t('reviews.totalRows', { count: rows.length })}
            {selectedIDs.length > 0 && (
              <span style={{ marginLeft: 8, color: '#1677ff' }}>
                {t('reviews.selectedSuffix', { count: selectedIDs.length })}
              </span>
            )}
          </Typography.Text>
          {status === 'pending' && selectedIDs.length > 0 && (
            <>
              <Button onClick={() => onBulkDecide('approve')} loading={bulkSubmitting}>
                {t('reviews.bulkApprove')}
              </Button>
              <Button danger onClick={() => onBulkDecide('reject')} loading={bulkSubmitting}>
                {t('reviews.bulkReject')}
              </Button>
              <Button size="small" onClick={() => setSelectedIDs([])}>{t('reviews.clearSelection')}</Button>
            </>
          )}
        </Space>
        <Table<ReviewItem>
          rowKey="id" size="small" loading={loading} dataSource={rows}
          pagination={{ pageSize: 25 }}
          rowSelection={status === 'pending' ? {
            selectedRowKeys: selectedIDs,
            onChange: (keys) => setSelectedIDs(keys as string[]),
          } : undefined}
          columns={[
            {
              title: t('reviews.columns.decisionId'), dataIndex: 'id', width: 220, ellipsis: true,
              render: (v) => <Typography.Text code copyable>{v}</Typography.Text>,
            },
            { title: t('reviews.columns.merchant'), dataIndex: 'merchant_id', width: 140, ellipsis: true },
            { title: t('reviews.columns.customer'), dataIndex: 'customer_id', width: 140, ellipsis: true },
            {
              title: t('reviews.columns.pi'), dataIndex: 'payment_intent_id', width: 180, ellipsis: true,
              render: (v: string) => <Typography.Text code>{v}</Typography.Text>,
            },
            {
              title: t('reviews.columns.amount'), dataIndex: 'amount', width: 110, align: 'right',
              // 注册 / 登录 / 改密这类事件 amount=0 currency="" — display 会抛 unsupported currency；
              // 用 — 占位避免崩溃。
              render: (v: number, r) => (v && r.currency) ? display(v, r.currency) : '—',
            },
            {
              title: t('reviews.columns.riskScore'), dataIndex: 'risk_score', width: 80, align: 'center',
              render: (v: number) => <Tag color={SCORE_COLOR(v)}>{v}</Tag>,
            },
            {
              title: t('reviews.columns.hitRules'), dataIndex: 'reasons', ellipsis: true,
              render: (vs: string[]) => (
                <Space size={4} wrap>
                  {(vs || []).map((s, i) => (
                    <Tag key={i}>{s}</Tag>
                  ))}
                </Space>
              ),
            },
            {
              title: t('reviews.columns.status'), dataIndex: 'status', width: 90,
              render: (v: ReviewStatus) => <Tag color={STATUS_COLOR[v]}>{v}</Tag>,
            },
            {
              title: t('reviews.columns.createdAt'), dataIndex: 'created_at', width: 150,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss'),
            },
            {
              title: '', width: 60,
              render: (_, r) => <a onClick={() => setDetail(r)}>{t('reviews.detailLink')}</a>,
            },
          ]}
        />
      </Card>

      <Drawer
        title={detail ? t('reviews.drawerTitle', { id: detail.id }) : ''}
        width={720} open={!!detail} onClose={() => setDetail(null)}
        extra={detail?.status === 'pending' && (
          <Button type="primary" onClick={() => setDecideOpen(true)}>{t('reviews.decideButton')}</Button>
        )}
      >
        {detail && (
          <Space direction="vertical" style={{ width: '100%' }} size="middle">
            <Card size="small" title={t('reviews.drawerSections.basic')}>
              <Row k={t('reviews.fields.decisionId')} v={<Typography.Text code copyable>{detail.id}</Typography.Text>} />
              <Row k={t('reviews.fields.merchant')} v={detail.merchant_id} />
              <Row k={t('reviews.fields.customer')} v={detail.customer_id} />
              <Row k={t('reviews.fields.pi')} v={<Typography.Text code copyable>{detail.payment_intent_id}</Typography.Text>} />
              <Row k={t('reviews.fields.amount')} v={(detail.amount && detail.currency) ? display(detail.amount, detail.currency) : '—'} />
              <Row k={t('reviews.fields.riskScore')} v={<Tag color={SCORE_COLOR(detail.risk_score)}>{detail.risk_score}</Tag>} />
              <Row k={t('reviews.fields.status')} v={<Tag color={STATUS_COLOR[detail.status]}>{detail.status}</Tag>} />
              <Row k={t('reviews.fields.createdAt')} v={dayjs(detail.created_at).format('YYYY-MM-DD HH:mm:ss')} />
              {detail.decided_at && <>
                <Row k={t('reviews.fields.decidedAt')} v={dayjs(detail.decided_at).format('YYYY-MM-DD HH:mm:ss')} />
                <Row k={t('reviews.fields.decidedBy')} v={detail.decided_by} />
                <Row k={t('reviews.fields.decideReason')} v={detail.decide_reason || '-'} />
              </>}
            </Card>
            <Card size="small" title={t('reviews.drawerSections.hitRules')}>
              {(detail.reasons || []).length === 0
                ? <Typography.Text type="secondary">{t('common.none')}</Typography.Text>
                : <ul style={{ margin: 0, paddingLeft: 20 }}>
                  {detail.reasons.map((r, i) => <li key={i}><Typography.Text>{r}</Typography.Text></li>)}
                </ul>
              }
            </Card>
          </Space>
        )}
      </Drawer>

      <Modal
        title={t('reviews.decideModal.title')} open={decideOpen}
        onCancel={() => setDecideOpen(false)} onOk={() => form.submit()}
      >
        <Alert
          type="warning" showIcon style={{ marginBottom: 12 }}
          message={t('reviews.decideModal.alert')}
        />
        <Form form={form} layout="vertical" onFinish={onDecide} initialValues={{ action: 'approve' }}>
          <Form.Item name="action" label={t('reviews.decideModal.actionLabel')} rules={[{ required: true }]}>
            <Select options={[
              { value: 'approve', label: t('reviews.decideModal.approveOption') },
              { value: 'reject',  label: t('reviews.decideModal.rejectOption') },
            ]} />
          </Form.Item>
          <Form.Item name="actor" label={t('reviews.decideModal.actorLabel')} rules={[{ required: true }]}>
            <Input placeholder={t('reviews.decideModal.actorPlaceholder')} />
          </Form.Item>
          <Form.Item name="reason" label={t('reviews.decideModal.reasonLabel')}>
            <Input.TextArea rows={3} placeholder={t('reviews.decideModal.reasonPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div style={{ marginBottom: 6 }}>
      <Typography.Text type="secondary" style={{ display: 'inline-block', width: 90 }}>{k}：</Typography.Text>
      {v}
    </div>
  )
}
