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
import { Trans, useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('ops')
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
      message.success(t('risk.reloadSuccess', { count: r.loaded }))
      reload()
    } catch (e) {
      message.error(String(e))
    }
  }

  // config-center URL：来自 backend ConfigOverview 返回；fallback 到 9691。
  const ccURL = (config as any)?.config_center_url || 'http://localhost:9691/admin/'

  return (
    <div>
      <Typography.Title level={3}>{t('title')}</Typography.Title>

      {/* ── 全平台动态配置入口（config-center） ── */}
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={t('configCenter.message')}
        description={
          <div>
            <div style={{ marginBottom: 8 }}>
              {t('configCenter.description')}
            </div>
            <Button type="primary" size="small" href={ccURL} target="_blank">
              {t('configCenter.open')}
            </Button>
            <Button
              size="small"
              style={{ marginLeft: 8 }}
              href={ccURL + 'audit'}
              target="_blank"
            >
              {t('configCenter.audit')}
            </Button>
            <Button
              size="small"
              style={{ marginLeft: 8 }}
              href={ccURL + 'items'}
              target="_blank"
            >
              {t('configCenter.keySearch')}
            </Button>
          </div>
        }
      />

      {/* ── 服务健康 ── */}
      <Card
        title={t('health.title')}
        loading={loading}
        extra={<Button onClick={reload} size="small">{t('common:actions.refresh')}</Button>}
      >
        {health && (
          <>
            <Alert
              type={health.overall ? 'success' : 'error'}
              showIcon
              message={health.overall ? t('health.allHealthy') : t('health.someUnhealthy')}
              style={{ marginBottom: 16 }}
            />
            <Table<ServiceHealth>
              rowKey="name"
              dataSource={health.services}
              pagination={false}
              size="small"
              columns={[
                { title: t('health.columns.service'), dataIndex: 'name' },
                {
                  title: t('health.columns.status'),
                  dataIndex: 'healthy',
                  render: (v) =>
                    v ? <Badge status="success" text={t('health.healthy')} /> : <Badge status="error" text={t('health.unhealthy')} />,
                },
                { title: t('health.columns.latency'), dataIndex: 'latency' },
                { title: t('health.columns.error'), dataIndex: 'error', render: (v) => v || '—' },
              ]}
            />
          </>
        )}
      </Card>

      <Divider />

      {/* ── 风控规则 ── */}
      <Card
        title={t('risk.title')}
        extra={
          <Space>
            <Button size="small" href={ccURL + 'ns/risk-manage'} target="_blank">
              {t('risk.editInConfigCenter')}
            </Button>
            <Button onClick={onReloadRules} size="small">
              {t('risk.forceReload')}
            </Button>
          </Space>
        }
      >
        {rules.length === 0 ? (
          <Alert
            type="info"
            showIcon
            message={t('risk.migrated.title')}
            description={
              <span>
                <Trans
                  i18nKey="risk.migrated.prefix"
                  ns="ops"
                />
                {' '}
                <a href={ccURL + 'ns/risk-manage'} target="_blank" rel="noreferrer">
                  {' '}{t('risk.migrated.linkText')}{' '}
                </a>
                {t('risk.migrated.suffix')}
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
              { title: t('risk.columns.id'), dataIndex: 'id', width: 200 },
              { title: t('risk.columns.name'), dataIndex: 'name' },
              { title: t('risk.columns.type'), dataIndex: 'type', render: (v) => <Tag>{v}</Tag> },
              {
                title: t('risk.columns.status'),
                dataIndex: 'enabled',
                render: (v) =>
                  v ? <Tag color="green">{t('risk.columns.enabled')}</Tag> : <Tag color="default">{t('risk.columns.disabled')}</Tag>,
              },
            ]}
          />
        )}
      </Card>

      <Divider />

      {/* ── 系统配置 ── */}
      {config && (
        <Card title={t('config.title')}>
          <Row gutter={16}>
            <Col span={12}>
              <Descriptions column={1} size="small" bordered title={t('config.servicePorts')}>
                {Object.entries(config.ports || {}).map(([k, v]) => (
                  <Descriptions.Item key={k} label={k}>
                    {v}
                  </Descriptions.Item>
                ))}
              </Descriptions>
            </Col>
            <Col span={12}>
              <Descriptions column={1} size="small" bordered title={t('config.featureFlags')}>
                {Object.entries(config.features || {}).map(([k, v]) => (
                  <Descriptions.Item key={k} label={k}>
                    {v ? <Tag color="green">{t('config.on')}</Tag> : <Tag color="default">{t('config.off')}</Tag>}
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
