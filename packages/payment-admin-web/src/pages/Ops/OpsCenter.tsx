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

  // config-center URL：来自 backend ConfigOverview 返回；fallback 到 9691。
  const ccURL = (config as any)?.config_center_url || 'http://localhost:9691/admin/'

  return (
    <div>
      <Typography.Title level={3}>运营中心</Typography.Title>

      {/* ── 全平台动态配置入口（config-center） ── */}
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="全平台动态配置已收口到 config-center"
        description={
          <div>
            <div style={{ marginBottom: 8 }}>
              所有动态配置（rate_limit / 风控阈值 / 熔断 / TTL / 路由权重 / 队列参数 等）改一次即可
              秒级热更新到集群所有副本。下方各服务状态仅作只读监控。
            </div>
            <Button type="primary" size="small" href={ccURL} target="_blank">
              打开 Config Center 管理页
            </Button>
            <Button
              size="small"
              style={{ marginLeft: 8 }}
              href={ccURL + 'audit'}
              target="_blank"
            >
              审计日志
            </Button>
            <Button
              size="small"
              style={{ marginLeft: 8 }}
              href={ccURL + 'items'}
              target="_blank"
            >
              全平台 key 检索
            </Button>
          </div>
        }
      />

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
        title="风控规则（只读监控）"
        extra={
          <Space>
            <Button size="small" href={ccURL + 'ns/risk-manage'} target="_blank">
              在 Config Center 编辑
            </Button>
            <Button onClick={onReloadRules} size="small">
              强制重载（兼容老路径）
            </Button>
          </Space>
        }
      >
        {rules.length === 0 ? (
          <Alert
            type="info"
            showIcon
            message="规则配置已迁到 Config Center"
            description={
              <span>
                修改 / 新增 / 灰度 / 回滚规则请在 Config Center
                <a href={ccURL + 'ns/risk-manage'} target="_blank" rel="noreferrer">
                  {' '}namespace=risk-manage{' '}
                </a>
                操作；改完秒级 OnChange 热更新到所有 risk-manage 副本。
              </span>
            }
          />
        ) : (
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
        )}
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
