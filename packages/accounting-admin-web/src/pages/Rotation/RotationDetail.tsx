import { useEffect, useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import {
  Typography,
  Card,
  Descriptions,
  Tag,
  Table,
  Button,
  Space,
  Alert,
  Row,
  Col,
  Statistic,
  message,
} from 'antd'
import {
  ArrowLeftOutlined,
  ReloadOutlined,
  CheckCircleOutlined,
  WarningOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useTranslation, Trans } from 'react-i18next'
import { getRotationInstanceHistory } from '../../api/accounting'
import type {
  RotationInstanceHistoryView,
  RotationInstanceHistoryRow,
} from '../../types/accounting'
import { LIFECYCLE_PHASE_COLOR, LIFECYCLE_PHASE_LABEL } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const { Title, Paragraph, Text } = Typography

function fmt(rfc?: string): string {
  if (!rfc || rfc.startsWith('0001-')) return '—'
  try {
    return new Date(rfc).toLocaleString()
  } catch {
    return rfc
  }
}

export default function RotationDetail() {
  const { t } = useTranslation('rotation')
  const { key } = useParams<{ key: string }>()
  const navigate = useNavigate()
  const decodedKey = decodeURIComponent(key || '')

  const [data, setData] = useState<RotationInstanceHistoryView | null>(null)
  const [loading, setLoading] = useState(false)

  const load = async () => {
    if (!decodedKey) return
    setLoading(true)
    try {
      const view = await getRotationInstanceHistory(decodedKey)
      setData(view)
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('detail.loadFailed'))
      setData(null)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [decodedKey])

  const columns: ColumnsType<RotationInstanceHistoryRow> = [
    { title: t('detail.columns.accountNo'), dataIndex: 'account_no', key: 'account_no', width: 240, ellipsis: true },
    {
      title: t('detail.columns.phase'),
      dataIndex: 'lifecycle_phase',
      key: 'lifecycle_phase',
      width: 130,
      render: (v: number, row) => (
        <Tag color={LIFECYCLE_PHASE_COLOR[v] || 'default'}>
          {row.lifecycle_phase_name || LIFECYCLE_PHASE_LABEL[v] || `phase=${v}`}
        </Tag>
      ),
    },
    {
      title: t('detail.columns.balance'),
      dataIndex: 'balance',
      key: 'balance',
      width: 140,
      align: 'right',
      render: (v: number, row) => (
        <Space>
          {displayMoney(String(v), row.currency)}
          {row.is_zero ? (
            <Tag color="green" icon={<CheckCircleOutlined />}>{t('detail.isZeroTag')}</Tag>
          ) : (
            <Tag color="orange" icon={<WarningOutlined />}>{t('detail.nonZeroTag')}</Tag>
          )}
        </Space>
      ),
    },
    {
      title: t('detail.columns.available'),
      dataIndex: 'available_balance',
      key: 'available_balance',
      width: 140,
      align: 'right',
      render: (v: number, row) => displayMoney(String(v), row.currency),
    },
    {
      title: t('detail.columns.frozen'),
      dataIndex: 'frozen_balance',
      key: 'frozen_balance',
      width: 120,
      align: 'right',
      render: (v: number, row) => displayMoney(String(v), row.currency),
    },
    { title: t('detail.columns.periodStart'), dataIndex: 'period_start', key: 'period_start', width: 180, render: fmt },
    { title: t('detail.columns.periodEnd'), dataIndex: 'period_end', key: 'period_end', width: 180, render: fmt },
    {
      title: t('detail.columns.drainingStartedAt'),
      dataIndex: 'draining_started_at',
      key: 'draining_started_at',
      width: 180,
      render: fmt,
    },
    { title: t('detail.columns.frozenAt'), dataIndex: 'frozen_at', key: 'frozen_at', width: 180, render: fmt },
    { title: t('detail.columns.archivedAt'), dataIndex: 'archived_at', key: 'archived_at', width: 180, render: fmt },
    {
      title: t('detail.columns.policyOverride'),
      key: 'policy',
      width: 220,
      render: (_, row) => (
        <Space direction="vertical" size={0}>
          {row.policy_version_at_birth !== undefined && (
            <Text type="secondary">policy v{row.policy_version_at_birth}</Text>
          )}
          {row.effective_hard_timeout_secs !== undefined && (
            <Text type="secondary">hard_to={row.effective_hard_timeout_secs}s</Text>
          )}
          {row.override_reason && (
            <Text type="warning">
              override: {row.override_by} / {row.override_reason}
            </Text>
          )}
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Space style={{ marginBottom: 16 }}>
        <Button icon={<ArrowLeftOutlined />} onClick={() => navigate('/rotation')}>
          {t('detail.backToList')}
        </Button>
        <Button icon={<ReloadOutlined />} onClick={load} loading={loading}>
          {t('common:actions.refresh')}
        </Button>
      </Space>

      <Title level={3}>{decodedKey}</Title>
      <Paragraph type="secondary">
        <Trans i18nKey="detail.description" t={t}>
          {'该 logical_account 下所有 instance（含已 archived）的状态 + 余额。归档 instance 的 '}
          <Text code>balance</Text>
          {' 必须为 0（不变量 I4）；本页用绿色"0"标识。'}
        </Trans>
      </Paragraph>

      {data && !data.rotation_enabled && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message={t('detail.rotationDisabledTitle')}
          description={t('detail.rotationDisabledDesc')}
        />
      )}
      {data && !data.all_instances_zero && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message={t('detail.nonZeroTitle')}
          description={t('detail.nonZeroDesc')}
        />
      )}

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={6}>
          <Card>
            <Statistic title={t('detail.stats.currency')} value={data?.currency || '—'} />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('detail.stats.instanceCount')}
              value={data?.instances.length || 0}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('detail.stats.totalBalance')}
              value={data ? displayMoney(String(data.total_balance), data.currency) : '—'}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title={t('detail.stats.allZero')}
              value={data?.all_instances_zero ? t('detail.stats.allZeroYes') : t('detail.stats.allZeroNo')}
              valueStyle={{
                color: data?.all_instances_zero ? '#52c41a' : '#fa8c16',
              }}
            />
          </Card>
        </Col>
      </Row>

      {data && (
        <Card title={t('detail.phaseDistribution')} style={{ marginBottom: 16 }} size="small">
          <Space wrap>
            {Object.entries(data.phase_counts).map(([phase, count]) => (
              <Tag key={phase} color="blue">
                {phase}: {count}
              </Tag>
            ))}
          </Space>
        </Card>
      )}

      <Descriptions bordered size="small" column={2} style={{ marginBottom: 16 }}>
        <Descriptions.Item label={t('detail.info.logicalAccountId')}>
          {data?.logical_account_id || '—'}
        </Descriptions.Item>
        <Descriptions.Item label={t('detail.info.rotationEnabled')}>
          {data?.rotation_enabled ? t('common:status.yes') : t('common:status.no')}
        </Descriptions.Item>
      </Descriptions>

      <Table
        rowKey="account_no"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={data?.instances || []}
        pagination={false}
        scroll={{ x: 1900 }}
      />
    </div>
  )
}
