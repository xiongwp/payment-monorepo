import { Card, Row, Col, Statistic, Tag, Alert, Spin, Table, Badge } from 'antd'
import {
  CheckCircleOutlined,
  ExclamationCircleOutlined,
  ClockCircleOutlined,
  DatabaseOutlined,
  ApartmentOutlined,
  CalculatorOutlined,
  FileTextOutlined,
  SearchOutlined,
} from '@ant-design/icons'
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { healthCheck, listServiceInstances } from '../../api/accounting'
import type { ServiceInstance } from '../../types/accounting'

// ServiceInstance does not expose status — all instances returned by the API are alive

export default function Dashboard() {
  const { t } = useTranslation('instances')
  const navigate = useNavigate()
  const [healthy, setHealthy] = useState<boolean | null>(null)
  const [instances, setInstances] = useState<ServiceInstance[]>([])
  const [loadingInstances, setLoadingInstances] = useState(true)

  useEffect(() => {
    healthCheck()
      .then(() => setHealthy(true))
      .catch(() => setHealthy(false))

    listServiceInstances()
      .then((list) => setInstances(list))
      .catch(() => setInstances([]))
      .finally(() => setLoadingInstances(false))
  }, [])

  const shortcuts = [
    { icon: <SearchOutlined />, title: t('dashboard.shortcuts.transactions.title'), desc: t('dashboard.shortcuts.transactions.desc'), path: '/transactions', color: '#1890ff' },
    { icon: <DatabaseOutlined />, title: t('dashboard.shortcuts.accounts.title'), desc: t('dashboard.shortcuts.accounts.desc'), path: '/accounts', color: '#52c41a' },
    { icon: <CalculatorOutlined />, title: t('dashboard.shortcuts.booking.title'), desc: t('dashboard.shortcuts.booking.desc'), path: '/booking', color: '#faad14' },
    { icon: <FileTextOutlined />, title: t('dashboard.shortcuts.snapshots.title'), desc: t('dashboard.shortcuts.snapshots.desc'), path: '/snapshots', color: '#722ed1' },
    { icon: <ApartmentOutlined />, title: t('dashboard.shortcuts.trialBalance.title'), desc: t('dashboard.shortcuts.trialBalance.desc'), path: '/trial-balance', color: '#eb2f96' },
    { icon: <ClockCircleOutlined />, title: t('dashboard.shortcuts.tcc.title'), desc: t('dashboard.shortcuts.tcc.desc'), path: '/tcc', color: '#fa8c16' },
  ]

  const instanceColumns = [
    { title: t('columns.instance'), dataIndex: 'instance_id', key: 'instance_id' },
    { title: t('columns.host'), dataIndex: 'host', key: 'host' },
    { title: t('columns.grpcPort'), dataIndex: 'grpc_port', key: 'grpc_port' },
    {
      title: t('columns.status'),
      key: 'status',
      render: () => <Badge status="processing" text={t('statusTags.running')} />,
    },
    {
      title: t('columns.recentHeartbeat'),
      dataIndex: 'last_heartbeat',
      key: 'last_heartbeat',
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
  ]

  return (
    <div>
      <h2 style={{ marginBottom: 20 }}>{t('dashboard.title')}</h2>

      {healthy === false && (
        <Alert
          type="warning"
          showIcon
          message={t('dashboard.backendUnhealthy')}
          style={{ marginBottom: 16 }}
        />
      )}

      {/* 状态卡片 */}
      <Row gutter={16} style={{ marginBottom: 24 }}>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('dashboard.stats.serviceStatus')}
              value={healthy === null ? t('dashboard.stats.checking') : healthy ? t('dashboard.stats.normal') : t('dashboard.stats.abnormal')}
              prefix={
                healthy === null ? undefined :
                healthy ? <CheckCircleOutlined /> : <ExclamationCircleOutlined />
              }
              valueStyle={{ color: healthy === null ? '#888' : healthy ? '#3f8600' : '#cf1322' }}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('dashboard.stats.onlineInstances')}
              value={loadingInstances ? '-' : instances.length}
              valueStyle={{ color: '#1890ff' }}
            />
          </Card>
        </Col>
        <Col span={12}>
          <Card style={{ height: '100%' }}>
            <div style={{ fontSize: 14 }}>
              <p style={{ margin: 0, fontWeight: 500 }}>{t('dashboard.stats.systemName')}</p>
              <p style={{ margin: '4px 0 0', fontSize: 12, color: '#888' }}>
                {t('dashboard.stats.systemTagline')}
              </p>
            </div>
          </Card>
        </Col>
      </Row>

      {/* 快捷入口 */}
      <Card title={t('dashboard.shortcutsCard')} style={{ marginBottom: 24 }}>
        <Row gutter={[16, 16]}>
          {shortcuts.map((s) => (
            <Col span={8} key={s.path}>
              <Card
                hoverable
                size="small"
                onClick={() => navigate(s.path)}
                style={{ cursor: 'pointer', borderLeft: `3px solid ${s.color}` }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                  <span style={{ fontSize: 22, color: s.color }}>{s.icon}</span>
                  <div>
                    <div style={{ fontWeight: 500 }}>{s.title}</div>
                    <div style={{ fontSize: 12, color: '#888' }}>{s.desc}</div>
                  </div>
                </div>
              </Card>
            </Col>
          ))}
        </Row>
      </Card>

      {/* 服务实例列表 */}
      <Card title={t('dashboard.instancesCard')}>
        {loadingInstances ? (
          <Spin tip={t('dashboard.loadingText')} style={{ display: 'block', padding: 24 }} />
        ) : (
          <Table
            columns={instanceColumns}
            dataSource={instances}
            rowKey="instance_id"
            pagination={false}
            size="small"
            locale={{ emptyText: t('dashboard.emptyText') }}
          />
        )}
      </Card>

      <div style={{ marginTop: 16 }}>
        <Tag color="blue">{t('dashboard.tipTag')}</Tag>
        <span style={{ color: '#888', fontSize: 13 }}>
          &nbsp;{t('dashboard.tipText')}
        </span>
      </div>
    </div>
  )
}
