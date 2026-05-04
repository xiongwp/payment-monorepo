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
import { healthCheck, listServiceInstances } from '../../api/accounting'
import type { ServiceInstance } from '../../types/accounting'

// ServiceInstance does not expose status — all instances returned by the API are alive

export default function Dashboard() {
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
    { icon: <SearchOutlined />, title: '交易流水查询', desc: '按账户号 / 业务单号 / 交易ID 查询流水', path: '/transactions', color: '#1890ff' },
    { icon: <DatabaseOutlined />, title: '账户管理', desc: '创建、查询账户信息及余额', path: '/accounts', color: '#52c41a' },
    { icon: <CalculatorOutlined />, title: '手动记账', desc: '复式记账、调账操作', path: '/booking', color: '#faad14' },
    { icon: <FileTextOutlined />, title: '余额快照', desc: '查询账户日终余额快照', path: '/snapshots', color: '#722ed1' },
    { icon: <ApartmentOutlined />, title: '试算平衡', desc: '验证借贷平衡与会计恒等式', path: '/trial-balance', color: '#eb2f96' },
    { icon: <ClockCircleOutlined />, title: 'TCC 监控', desc: '查看并修复悬挂的 TCC 事务', path: '/tcc', color: '#fa8c16' },
  ]

  const instanceColumns = [
    { title: '实例', dataIndex: 'instance_id', key: 'instance_id' },
    { title: 'Host', dataIndex: 'host', key: 'host' },
    { title: 'gRPC 端口', dataIndex: 'grpc_port', key: 'grpc_port' },
    {
      title: '状态',
      key: 'status',
      render: () => <Badge status="processing" text="运行中" />,
    },
    {
      title: '最近心跳',
      dataIndex: 'last_heartbeat',
      key: 'last_heartbeat',
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
  ]

  return (
    <div>
      <h2 style={{ marginBottom: 20 }}>工作台</h2>

      {healthy === false && (
        <Alert
          type="warning"
          showIcon
          message="后端服务连接异常，请检查 accounting-system 是否运行"
          style={{ marginBottom: 16 }}
        />
      )}

      {/* 状态卡片 */}
      <Row gutter={16} style={{ marginBottom: 24 }}>
        <Col span={6}>
          <Card>
            <Statistic
              title="服务状态"
              value={healthy === null ? '检测中' : healthy ? '正常' : '异常'}
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
              title="在线实例数"
              value={loadingInstances ? '-' : instances.length}
              valueStyle={{ color: '#1890ff' }}
            />
          </Card>
        </Col>
        <Col span={12}>
          <Card style={{ height: '100%' }}>
            <div style={{ fontSize: 14 }}>
              <p style={{ margin: 0, fontWeight: 500 }}>复式记账管理系统</p>
              <p style={{ margin: '4px 0 0', fontSize: 12, color: '#888' }}>
                TCC 分布式事务 · 热路径(Redis+Outbox) · 分库分表(10库×100表) · 日切对账
              </p>
            </div>
          </Card>
        </Col>
      </Row>

      {/* 快捷入口 */}
      <Card title="快捷入口" style={{ marginBottom: 24 }}>
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
      <Card title="服务实例">
        {loadingInstances ? (
          <Spin tip="加载中..." style={{ display: 'block', padding: 24 }} />
        ) : (
          <Table
            columns={instanceColumns}
            dataSource={instances}
            rowKey="instance_id"
            pagination={false}
            size="small"
            locale={{ emptyText: '暂无实例注册' }}
          />
        )}
      </Card>

      <div style={{ marginTop: 16 }}>
        <Tag color="blue">提示</Tag>
        <span style={{ color: '#888', fontSize: 13 }}>
          &nbsp;「交易流水」查询需指定账户号、业务单号或交易ID，防止全库扫描
        </span>
      </div>
    </div>
  )
}
