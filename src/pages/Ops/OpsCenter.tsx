import { useEffect, useState } from 'react'
import {
  Alert,
  Badge,
  Button,
  Card,
  Col,
  Descriptions,
  Divider,
  Row,
  Space,
  Table,
  Tag,
  Typography,
  message,
} from 'antd'
import { request } from '../../api/client'

interface ServiceHealth {
  name: string
  url: string
  healthy: boolean
  latency: string
  error?: string
}

interface HealthOverview {
  overall: boolean
  services: ServiceHealth[]
}

interface RiskRule {
  id: string
  name: string
  type: string
  enabled: boolean
  description?: string
  config?: string
}

interface ConfigInfo {
  services: string[]
  ports: Record<string, string>
  features: Record<string, boolean>
}

export default function OpsCenter() {
  const [health, setHealth] = useState<HealthOverview | null>(null)
  const [rules, setRules] = useState<RiskRule[]>([])
  const [config, setConfig] = useState<ConfigInfo | null>(null)
  const [loading, setLoading] = useState(false)

  const reload = () => {
    setLoading(true)
    Promise.all([
      request<HealthOverview>({ url: '/ops/health' }).catch(() => null),
      request<{ rules: RiskRule[] }>({ url: '/ops/risk/rules' }).catch(() => ({ rules: [] })),
      request<ConfigInfo>({ url: '/ops/config' }).catch(() => null),
    ])
      .then(([h, r, c]) => {
        setHealth(h)
        setRules(r?.rules ?? [])
        setConfig(c)
      })
      .finally(() => setLoading(false))
  }

  useEffect(reload, [])

  const onReloadRules = async () => {
    try {
      const r = await request<{ loaded: number }>({ url: '/ops/risk/reload', method: 'POST' })
      message.success(`规则已重载：${r.loaded} 条`)
      reload()
    } catch (e) {
      message.error(String(e))
    }
  }

  return (
    <div>
      <Typography.Title level={3}>运营中心</Typography.Title>

      {/* ── 服务健康 ── */}
      <Card
        title="服务健康"
        loading={loading}
        extra={<Button onClick={reload} size="small">刷新</Button>}
      >
        {health && (
          <>
            <Alert
              type={health.overall ? 'success' : 'error'}
              showIcon
              message={health.overall ? '所有服务正常' : '部分服务异常'}
              style={{ marginBottom: 16 }}
            />
            <Table<ServiceHealth>
              rowKey="name"
              dataSource={health.services}
              pagination={false}
              size="small"
              columns={[
                { title: '服务', dataIndex: 'name' },
                {
                  title: '状态',
                  dataIndex: 'healthy',
                  render: (v) =>
                    v ? <Badge status="success" text="健康" /> : <Badge status="error" text="异常" />,
                },
                { title: '延迟', dataIndex: 'latency' },
                { title: '错误', dataIndex: 'error', render: (v) => v || '—' },
              ]}
            />
          </>
        )}
      </Card>

      <Divider />

      {/* ── 风控规则 ── */}
      <Card
        title="风控规则"
        extra={
          <Button type="primary" onClick={onReloadRules} size="small">
            热重载规则
          </Button>
        }
      >
        <Table<RiskRule>
          rowKey="id"
          dataSource={rules}
          pagination={false}
          size="small"
          columns={[
            { title: 'ID', dataIndex: 'id', width: 200 },
            { title: '规则名', dataIndex: 'name' },
            { title: '类型', dataIndex: 'type', render: (v) => <Tag>{v}</Tag> },
            {
              title: '状态',
              dataIndex: 'enabled',
              render: (v) =>
                v ? <Tag color="green">启用</Tag> : <Tag color="default">停用</Tag>,
            },
          ]}
        />
      </Card>

      <Divider />

      {/* ── 系统配置 ── */}
      {config && (
        <Card title="系统配置">
          <Row gutter={16}>
            <Col span={12}>
              <Descriptions column={1} size="small" bordered title="服务端口">
                {Object.entries(config.ports || {}).map(([k, v]) => (
                  <Descriptions.Item key={k} label={k}>
                    {v}
                  </Descriptions.Item>
                ))}
              </Descriptions>
            </Col>
            <Col span={12}>
              <Descriptions column={1} size="small" bordered title="功能开关">
                {Object.entries(config.features || {}).map(([k, v]) => (
                  <Descriptions.Item key={k} label={k}>
                    {v ? <Tag color="green">ON</Tag> : <Tag color="default">OFF</Tag>}
                  </Descriptions.Item>
                ))}
              </Descriptions>
            </Col>
          </Row>
        </Card>
      )}
    </div>
  )
}
