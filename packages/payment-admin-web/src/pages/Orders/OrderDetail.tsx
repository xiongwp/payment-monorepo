import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import { Card, Descriptions, Space, Table, Tag, Typography, message } from 'antd'
import { useTranslation } from 'react-i18next'
import dayjs from 'dayjs'
import { getOrder, listOrderCharges, listOrderRefunds } from '../../api'
import type { ChargeSummary, PaymentIntentDetail, RefundSummary } from '../../api/types'
import { display } from '../../utils/money'

export default function OrderDetail() {
  const { t } = useTranslation('order')
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
      <Typography.Title level={3}>{t('detail.title', { id })}</Typography.Title>
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Card loading={loading} title={t('detail.paymentIntent.cardTitle')}>
          {pi && (
            <Descriptions column={2} size="small">
              <Descriptions.Item label={t('detail.paymentIntent.id')}>{pi.id}</Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.status')}>
                <Tag>{pi.status.replace('PAYMENT_INTENT_STATUS_', '')}</Tag>
              </Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.amount')}>
                {display(pi.amount, pi.currency)}
              </Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.merchant')}>
                {pi.mch_id} / {pi.mch_order_no}
              </Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.shardingKey')}>{pi.business_id}</Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.idempotencyKey')}>{pi.idempotency_key}</Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.description')} span={2}>
                {pi.description}
              </Descriptions.Item>
              <Descriptions.Item label={t('detail.paymentIntent.createdAt')} span={2}>
                {pi.created ? dayjs(pi.created * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
              </Descriptions.Item>
            </Descriptions>
          )}
        </Card>

        <Card loading={loading} title={t('detail.charges.cardTitle')}>
          <Table<ChargeSummary>
            rowKey="id"
            dataSource={charges}
            pagination={false}
            size="small"
            columns={[
              { title: t('detail.charges.columns.chargeId'), dataIndex: 'id' },
              {
                title: t('detail.charges.columns.amount'),
                dataIndex: 'amount',
                render: (v, r) => display(v, r.currency),
              },
              {
                title: t('detail.charges.columns.amountCaptured'),
                dataIndex: 'amount_captured',
                render: (v, r) => display(v, r.currency),
              },
              {
                title: t('detail.charges.columns.amountRefunded'),
                dataIndex: 'amount_refunded',
                render: (v, r) => display(v, r.currency),
              },
              { title: t('detail.charges.columns.paymentMethod'), dataIndex: 'payment_method' },
              {
                title: t('detail.charges.columns.status'),
                dataIndex: 'status',
                render: (v) => v.replace('CHARGE_STATUS_', ''),
              },
              {
                title: t('detail.charges.columns.createdAt'),
                dataIndex: 'created',
                render: (v) =>
                  v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—',
              },
            ]}
          />
        </Card>

        <Card loading={loading} title={t('detail.refunds.cardTitle')}>
          <Table<RefundSummary>
            rowKey="id"
            dataSource={refunds}
            pagination={false}
            size="small"
            columns={[
              { title: t('detail.refunds.columns.refundId'), dataIndex: 'id' },
              { title: t('detail.refunds.columns.charge'), dataIndex: 'charge_id' },
              {
                title: t('detail.refunds.columns.amount'),
                dataIndex: 'amount',
                render: (v, r) => display(v, r.currency),
              },
              {
                title: t('detail.refunds.columns.status'),
                dataIndex: 'status',
                render: (v) => v.replace('REFUND_STATUS_', ''),
              },
              {
                title: t('detail.refunds.columns.reason'),
                dataIndex: 'reason',
                render: (v) => v.replace('REFUND_REASON_', ''),
              },
              {
                title: t('detail.refunds.columns.createdAt'),
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
