import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { Card, Descriptions, Space, Table, Tag, Typography, message } from 'antd'
import dayjs from 'dayjs'
import { getOrder, listOrderCharges, listOrderRefunds } from '../../api'
import type { ChargeSummary, PaymentIntentDetail, RefundSummary } from '../../api/types'
import { display } from '../../utils/money'

export default function OrderDetail() {
  const { id = '' } = useParams()
  const [pi, setPI] = useState<PaymentIntentDetail | null>(null)
  const [charges, setCharges] = useState<ChargeSummary[]>([])
  const [refunds, setRefunds] = useState<RefundSummary[]>([])
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (!id) return
    setLoading(true)
    Promise.all([getOrder(id), listOrderCharges(id), listOrderRefunds(id)])
      .then(([piv, cv, rv]) => {
        setPI(piv)
        setCharges(cv.items ?? [])
        setRefunds(rv.items ?? [])
      })
      .catch((e) => message.error(String(e)))
      .finally(() => setLoading(false))
  }, [id])

  return (
    <div>
      <Typography.Title level={3}>订单详情 {id}</Typography.Title>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Card loading={loading} title="PaymentIntent">
          {pi && (
            <Descriptions column={2} size="small">
              <Descriptions.Item label="ID">{pi.id}</Descriptions.Item>
              <Descriptions.Item label="状态">
                <Tag>{pi.status.replace('PAYMENT_INTENT_STATUS_', '')}</Tag>
              </Descriptions.Item>
              <Descriptions.Item label="金额">
                {display(pi.amount, pi.currency)}
              </Descriptions.Item>
              <Descriptions.Item label="商户">
                {pi.mch_id} / {pi.mch_order_no}
              </Descriptions.Item>
              <Descriptions.Item label="分片键">{pi.business_id}</Descriptions.Item>
              <Descriptions.Item label="幂等键">{pi.idempotency_key}</Descriptions.Item>
              <Descriptions.Item label="描述" span={2}>
                {pi.description}
              </Descriptions.Item>
              <Descriptions.Item label="创建时间" span={2}>
                {pi.created ? dayjs(pi.created * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
              </Descriptions.Item>
            </Descriptions>
          )}
        </Card>

        <Card loading={loading} title="Charges">
          <Table<ChargeSummary>
            rowKey="id"
            dataSource={charges}
            pagination={false}
            size="small"
            columns={[
              { title: 'Charge ID', dataIndex: 'id' },
              {
                title: '金额',
                dataIndex: 'amount',
                render: (v, r) => display(v, r.currency),
              },
              {
                title: '已扣款',
                dataIndex: 'amount_captured',
                render: (v, r) => display(v, r.currency),
              },
              {
                title: '已退款',
                dataIndex: 'amount_refunded',
                render: (v, r) => display(v, r.currency),
              },
              { title: '支付方式', dataIndex: 'payment_method' },
              {
                title: '状态',
                dataIndex: 'status',
                render: (v) => v.replace('CHARGE_STATUS_', ''),
              },
              {
                title: '创建时间',
                dataIndex: 'created',
                render: (v) =>
                  v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—',
              },
            ]}
          />
        </Card>

        <Card loading={loading} title="Refunds">
          <Table<RefundSummary>
            rowKey="id"
            dataSource={refunds}
            pagination={false}
            size="small"
            columns={[
              { title: 'Refund ID', dataIndex: 'id' },
              { title: 'Charge', dataIndex: 'charge_id' },
              {
                title: '金额',
                dataIndex: 'amount',
                render: (v, r) => display(v, r.currency),
              },
              {
                title: '状态',
                dataIndex: 'status',
                render: (v) => v.replace('REFUND_STATUS_', ''),
              },
              {
                title: '原因',
                dataIndex: 'reason',
                render: (v) => v.replace('REFUND_REASON_', ''),
              },
              {
                title: '创建时间',
                dataIndex: 'created',
                render: (v) =>
                  v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—',
              },
            ]}
          />
        </Card>
      </Space>
    </div>
  )
}
