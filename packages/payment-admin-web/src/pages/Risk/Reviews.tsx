import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Form, Input, Modal, Radio, Select,
  Space, Table, Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
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
      message.warning('请先勾选要批量处理的条目')
      return
    }
    let actor = ''
    let reason = ''
    Modal.confirm({
      title: `批量${action === 'approve' ? '批准' : '拒绝'} ${ids.length} 笔审核？`,
      content: (
        <div>
          <Alert
            type={action === 'reject' ? 'warning' : 'info'} showIcon
            message={`将对选中的 ${ids.length} 笔同时执行 ${action}`}
            style={{ marginBottom: 12 }}
          />
          <Input
            placeholder="actor (操作人 ID，必填)"
            onChange={(e) => { actor = e.target.value.trim() }}
            style={{ marginBottom: 8 }}
          />
          <Input.TextArea
            placeholder="reason (原因，落审计 + ML 训练 label)" rows={3}
            onChange={(e) => { reason = e.target.value }}
          />
        </div>
      ),
      okText: '确认', okButtonProps: { danger: action === 'reject' },
      onOk: async () => {
        if (!actor) {
          message.error('actor 必填')
          return Promise.reject()
        }
        setBulkSubmitting(true)
        try {
          const r = await decideReviewBulk({ ids, action, actor, reason })
          message.success(
            `批量${action === 'approve' ? '批准' : '拒绝'}：${r.success}/${r.total} 成功，${r.not_pending} 已变更状态，${r.failed} 失败`,
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
      message.success(v.action === 'approve' ? '已批准（放行）' : '已拒绝（拦截）')
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
      <Typography.Title level={3}>风控人工审核队列</Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message="风控引擎判 verdict=Review 的交易会进入此队列。决议结果（approve/reject）会自动写到 feedback recorder，作为 ML 模型的训练 label。"
        />
        <Space style={{ marginBottom: 16 }} wrap>
          <Radio.Group value={status} onChange={(e) => { setStatus(e.target.value); setSelectedIDs([]) }}>
            <Radio.Button value="pending">待审核</Radio.Button>
            <Radio.Button value="approved">已批准</Radio.Button>
            <Radio.Button value="rejected">已拒绝</Radio.Button>
          </Radio.Group>
          <Button onClick={load}>刷新</Button>
          <Typography.Text type="secondary">
            共 {rows.length} 条
            {selectedIDs.length > 0 && (
              <span style={{ marginLeft: 8, color: '#1677ff' }}>
                · 已选 {selectedIDs.length}
              </span>
            )}
          </Typography.Text>
          {status === 'pending' && selectedIDs.length > 0 && (
            <>
              <Button onClick={() => onBulkDecide('approve')} loading={bulkSubmitting}>
                批量批准
              </Button>
              <Button danger onClick={() => onBulkDecide('reject')} loading={bulkSubmitting}>
                批量拒绝
              </Button>
              <Button size="small" onClick={() => setSelectedIDs([])}>取消选中</Button>
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
              title: '决策 ID', dataIndex: 'id', width: 220, ellipsis: true,
              render: (v) => <Typography.Text code copyable>{v}</Typography.Text>,
            },
            { title: '商户', dataIndex: 'merchant_id', width: 140, ellipsis: true },
            { title: '客户', dataIndex: 'customer_id', width: 140, ellipsis: true },
            {
              title: 'PI', dataIndex: 'payment_intent_id', width: 180, ellipsis: true,
              render: (v: string) => <Typography.Text code>{v}</Typography.Text>,
            },
            {
              title: '金额', dataIndex: 'amount', width: 110, align: 'right',
              // 注册 / 登录 / 改密这类事件 amount=0 currency="" — display 会抛 unsupported currency；
              // 用 — 占位避免崩溃。
              render: (v: number, r) => (v && r.currency) ? display(v, r.currency) : '—',
            },
            {
              title: '风险分', dataIndex: 'risk_score', width: 80, align: 'center',
              render: (v: number) => <Tag color={SCORE_COLOR(v)}>{v}</Tag>,
            },
            {
              title: '命中规则', dataIndex: 'reasons', ellipsis: true,
              render: (vs: string[]) => (
                <Space size={4} wrap>
                  {(vs || []).map((s, i) => (
                    <Tag key={i}>{s}</Tag>
                  ))}
                </Space>
              ),
            },
            {
              title: '状态', dataIndex: 'status', width: 90,
              render: (v: ReviewStatus) => <Tag color={STATUS_COLOR[v]}>{v}</Tag>,
            },
            {
              title: '创建时间', dataIndex: 'created_at', width: 150,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss'),
            },
            {
              title: '', width: 60,
              render: (_, r) => <a onClick={() => setDetail(r)}>详情</a>,
            },
          ]}
        />
      </Card>

      <Drawer
        title={detail ? `Review ${detail.id}` : ''}
        width={720} open={!!detail} onClose={() => setDetail(null)}
        extra={detail?.status === 'pending' && (
          <Button type="primary" onClick={() => setDecideOpen(true)}>决议</Button>
        )}
      >
        {detail && (
          <Space direction="vertical" style={{ width: '100%' }} size="middle">
            <Card size="small" title="基本">
              <Row k="决策 ID" v={<Typography.Text code copyable>{detail.id}</Typography.Text>} />
              <Row k="商户" v={detail.merchant_id} />
              <Row k="客户" v={detail.customer_id} />
              <Row k="PI" v={<Typography.Text code copyable>{detail.payment_intent_id}</Typography.Text>} />
              <Row k="金额" v={(detail.amount && detail.currency) ? display(detail.amount, detail.currency) : '—'} />
              <Row k="风险分" v={<Tag color={SCORE_COLOR(detail.risk_score)}>{detail.risk_score}</Tag>} />
              <Row k="状态" v={<Tag color={STATUS_COLOR[detail.status]}>{detail.status}</Tag>} />
              <Row k="创建时间" v={dayjs(detail.created_at).format('YYYY-MM-DD HH:mm:ss')} />
              {detail.decided_at && <>
                <Row k="决议时间" v={dayjs(detail.decided_at).format('YYYY-MM-DD HH:mm:ss')} />
                <Row k="决议人" v={detail.decided_by} />
                <Row k="决议原因" v={detail.decide_reason || '-'} />
              </>}
            </Card>
            <Card size="small" title="命中规则">
              {(detail.reasons || []).length === 0
                ? <Typography.Text type="secondary">无</Typography.Text>
                : <ul style={{ margin: 0, paddingLeft: 20 }}>
                  {detail.reasons.map((r, i) => <li key={i}><Typography.Text>{r}</Typography.Text></li>)}
                </ul>
              }
            </Card>
          </Space>
        )}
      </Drawer>

      <Modal
        title="决议" open={decideOpen}
        onCancel={() => setDecideOpen(false)} onOk={() => form.submit()}
      >
        <Alert
          type="warning" showIcon style={{ marginBottom: 12 }}
          message="approve = 放行该笔交易；reject = 标记为欺诈并阻止放款。决议后会自动写一条 SourceReviewHuman 的 outcome 给 ML 训练。"
        />
        <Form form={form} layout="vertical" onFinish={onDecide} initialValues={{ action: 'approve' }}>
          <Form.Item name="action" label="动作" rules={[{ required: true }]}>
            <Select options={[
              { value: 'approve', label: 'approve（放行 / 非欺诈）' },
              { value: 'reject',  label: 'reject（拒绝 / 是欺诈）' },
            ]} />
          </Form.Item>
          <Form.Item name="actor" label="操作人" rules={[{ required: true }]}>
            <Input placeholder="如 ops:zhang.san" />
          </Form.Item>
          <Form.Item name="reason" label="决议原因">
            <Input.TextArea rows={3} placeholder="如：客户致电确认本人交易；或：与历史 chargeback 设备指纹一致" />
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
