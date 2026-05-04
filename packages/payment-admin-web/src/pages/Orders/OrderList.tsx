import { useState } from 'react'
import { Alert, Button, Card, Input, Space, Table, Tag, Typography, message } from 'antd'
import { useNavigate } from 'react-router-dom'
import dayjs from 'dayjs'
import { listOrders } from '../../api'
import type { PaymentIntentSummary } from '../../api/types'
import { display } from '../../utils/money'

const statusColor: Record<string, string> = {
  PAYMENT_INTENT_STATUS_CREATED: 'default',
  PAYMENT_INTENT_STATUS_REQUIRES_ACTION: 'blue',
  PAYMENT_INTENT_STATUS_PROCESSING: 'gold',
  PAYMENT_INTENT_STATUS_SUCCEEDED: 'green',
  PAYMENT_INTENT_STATUS_FAILED: 'red',
  PAYMENT_INTENT_STATUS_CANCELED: 'default',
}

export default function OrderList() {
  const nav = useNavigate()
  const [items, setItems] = useState<PaymentIntentSummary[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [mchID, setMchID] = useState('')
  const [loading, setLoading] = useState(false)
  const [queried, setQueried] = useState(false)

  // 注意：order-core 的 PaymentIntent 表按 business_id(=mch_id) 分库分表，
  // 没法做"跨商户全局列表"。所以必须用户明确填 mch_id 才发请求。
  const load = (p: number, ps: number) => {
    if (!mchID.trim()) {
      message.warning('请先填商户 ID')
      return
    }
    setLoading(true)
    setQueried(true)
    listOrders({ mch_id: mchID.trim(), page: p, page_size: ps })
      .then((res) => {
        setItems(res.items ?? [])
        setTotal(res.total ?? 0)
      })
      .catch((e) => {
        message.error(String(e))
        setItems([])
        setTotal(0)
      })
      .finally(() => setLoading(false))
  }

  const onSearch = () => {
    setPage(1)
    load(1, pageSize)
  }

  return (
    <div>
      <Typography.Title level={3}>订单管理</Typography.Title>

      <Alert
        type="info"
        showIcon
        message="PaymentIntent 按 business_id（商户 ID）分库分表"
        description="必须指定商户 ID 才能查询；否则会命中全库扫描，order-core 会直接拒绝。"
        style={{ marginBottom: 16 }}
      />

      <Card>
        <Space style={{ marginBottom: 16 }}>
          <Input
            placeholder="商户 ID（必填）"
            value={mchID}
            onChange={(e) => setMchID(e.target.value)}
            onPressEnter={onSearch}
            style={{ width: 240 }}
            allowClear
          />
          <Button type="primary" onClick={onSearch} disabled={!mchID.trim()}>
            查询
          </Button>
        </Space>
        <Table<PaymentIntentSummary>
          rowKey="id"
          loading={loading}
          dataSource={items}
          locale={{ emptyText: queried ? '无匹配订单' : '请输入商户 ID 后查询' }}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            onChange: (p, ps) => {
              setPage(p)
              setPageSize(ps)
              load(p, ps)
            },
          }}
          columns={[
            {
              title: '订单号',
              dataIndex: 'id',
              render: (v) => <a onClick={() => nav(`/orders/${v}`)}>{v}</a>,
            },
            {
              title: '金额',
              dataIndex: 'amount',
              render: (v, r) => display(v, r.currency),
            },
            {
              title: '状态',
              dataIndex: 'status',
              render: (v) => (
                <Tag color={statusColor[v] || 'default'}>
                  {v.replace('PAYMENT_INTENT_STATUS_', '')}
                </Tag>
              ),
            },
            { title: '商户', dataIndex: 'mch_id' },
            { title: '商户订单号', dataIndex: 'mch_order_no' },
            {
              title: '创建时间',
              dataIndex: 'created',
              render: (v) => (v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'),
            },
          ]}
        />
      </Card>
    </div>
  )
}
