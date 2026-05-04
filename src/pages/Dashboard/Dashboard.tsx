import { useEffect, useState } from 'react'
import { Card, Col, Row, Statistic, Table, Tag, Typography, message } from 'antd'
import { Link } from 'react-router-dom'
import dayjs from 'dayjs'
import { getDashboard } from '../../api'
import type { DashboardSummary, PaymentIntentSummary } from '../../api/types'
import { display } from '../../utils/money'

export default function Dashboard() {
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
      <Typography.Title level={3}>工作台</Typography.Title>
      <Row gutter={16}>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic title="订单总数" value={data?.orders_total ?? 0} />
          </Card>
        </Col>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic title="商户总数" value={data?.merchants_total ?? 0} />
          </Card>
        </Col>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic
              title="KYC 已通过 / 待提交"
              value={`${data?.merchants_approved ?? 0} / ${data?.merchants_kyc_pending ?? 0}`}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card loading={loading}>
            <Statistic title="KMS Key 数量" value={data?.kms_keys_total ?? 0} />
          </Card>
        </Col>
      </Row>

      <Row gutter={16} style={{ marginTop: 16 }}>
        <Col span={14}>
          <Card title="最近订单" loading={loading}>
            <Table<PaymentIntentSummary>
              rowKey="id"
              dataSource={data?.recent_orders ?? []}
              pagination={false}
              size="small"
              columns={[
                { title: '订单号', dataIndex: 'id' },
                {
                  title: '金额',
                  dataIndex: 'amount',
                  render: (v, r) => display(v, r.currency),
                },
                { title: '状态', dataIndex: 'status' },
                { title: '商户', dataIndex: 'mch_id' },
                {
                  title: '时间',
                  dataIndex: 'created',
                  render: (v) => (v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'),
                },
              ]}
            />
          </Card>
        </Col>
        <Col span={10}>
          <Card
            title="最近商户审计"
            loading={loading}
            extra={<Link to="/audit/user-merchant">全部</Link>}
          >
            <Table
              rowKey="id"
              dataSource={data?.recent_user_merchant_audits ?? []}
              pagination={false}
              size="small"
              columns={[
                {
                  title: '时间',
                  dataIndex: 'created_ms',
                  width: 130,
                  render: (v: number) => dayjs(v).format('MM-DD HH:mm:ss'),
                },
                {
                  title: '操作',
                  dataIndex: 'method',
                  render: (m: string) => <code>{m.split('/').pop()}</code>,
                },
                { title: 'Actor', dataIndex: 'actor', width: 100 },
                {
                  title: 'Status',
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
        <Statistic title="当前活跃 KMS Key" value={data?.kms_active_key ?? '—'} />
      </Card>
    </div>
  )
}
