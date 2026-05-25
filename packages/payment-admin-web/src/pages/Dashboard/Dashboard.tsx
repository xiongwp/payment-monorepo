import { useEffect, useState } from 'react'
import { Card, Col, Row, Statistic, Table, Tag, Typography, message } from 'antd'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import dayjs from 'dayjs'
import { getDashboard } from '../../api'
import type { DashboardSummary, PaymentIntentSummary } from '../../api/types'
import { display } from '../../utils/money'

export default function Dashboard() {
  const { t } = useTranslation('dashboard')
  const [data, setData] = useState<DashboardSummary | null>(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    setLoading(true)
    getDashboard()
      .then(setData)
      .catch((e) => message.error(String(e)))
      .finally(() => setLoading(false))
  }, [])

  return (
    <div>
      <Typography.Title level={3}>{t('title')}</Typography.Title>
      <Row gutter={16}>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic title={t('stats.ordersTotal')} value={data?.orders_total ?? 0} />
          </Card>
        </Col>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic title={t('stats.merchantsTotal')} value={data?.merchants_total ?? 0} />
          </Card>
        </Col>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic
              title={t('stats.kycApprovedPending')}
              value={`${data?.merchants_approved ?? 0} / ${data?.merchants_kyc_pending ?? 0}`}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic title={t('stats.kmsKeysTotal')} value={data?.kms_keys_total ?? 0} />
          </Card>
        </Col>
      </Row>

      <Row gutter={16} style={{ marginTop: 16 }}>
        <Col span={14}>
          <Card title={t('recentOrders.title')} loading={loading}>
            <Table<PaymentIntentSummary>
              rowKey="id"
              dataSource={data?.recent_orders ?? []}
              pagination={false}
              size="small"
              columns={[
                { title: t('recentOrders.columns.orderId'), dataIndex: 'id' },
                {
                  title: t('recentOrders.columns.amount'),
                  dataIndex: 'amount',
                  render: (v, r) => display(v, r.currency),
                },
                { title: t('recentOrders.columns.status'), dataIndex: 'status' },
                { title: t('recentOrders.columns.merchant'), dataIndex: 'mch_id' },
                {
                  title: t('recentOrders.columns.time'),
                  dataIndex: 'created',
                  render: (v) => (v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'),
                },
              ]}
            />
          </Card>
        </Col>
        <Col span={10}>
          <Card
            title={t('recentMerchantAudit.title')}
            loading={loading}
            extra={<Link to="/audit/user-merchant">{t('recentMerchantAudit.viewAll')}</Link>}
          >
            <Table
              rowKey="id"
              dataSource={data?.recent_user_merchant_audits ?? []}
              pagination={false}
              size="small"
              columns={[
                {
                  title: t('recentMerchantAudit.columns.time'),
                  dataIndex: 'created_ms',
                  width: 130,
                  render: (v: number) => dayjs(v).format('MM-DD HH:mm:ss'),
                },
                {
                  title: t('recentMerchantAudit.columns.action'),
                  dataIndex: 'method',
                  render: (m: string) => <code>{m.split('/').pop()}</code>,
                },
                { title: t('recentMerchantAudit.columns.actor'), dataIndex: 'actor', width: 100 },
                {
                  title: t('recentMerchantAudit.columns.status'),
                  dataIndex: 'status_code',
                  width: 90,
                  render: (s: string) =>
                    s === 'OK' ? <Tag color="green">OK</Tag> : <Tag color="red">{s}</Tag>,
                },
              ]}
            />
          </Card>
        </Col>
      </Row>

      <Card style={{ marginTop: 16 }} loading={loading}>
        <Statistic title={t('stats.activeKmsKey')} value={data?.kms_active_key ?? '—'} />
      </Card>
    </div>
  )
}
