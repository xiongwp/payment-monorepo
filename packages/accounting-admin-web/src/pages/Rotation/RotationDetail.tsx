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
      message.error(e instanceof Error ? e.message : '加载失败')
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
    { title: 'Account No', dataIndex: 'account_no', key: 'account_no', width: 240, ellipsis: true },
    {
      title: 'Phase',
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
      title: 'Balance',
      dataIndex: 'balance',
      key: 'balance',
      width: 140,
      align: 'right',
      render: (v: number, row) => (
        <Space>
          {displayMoney(String(v), row.currency)}
          {row.is_zero ? (
            <Tag color="green" icon={<CheckCircleOutlined />}>0</Tag>
          ) : (
            <Tag color="orange" icon={<WarningOutlined />}>非零</Tag>
          )}
        </Space>
      ),
    },
    {
      title: 'Available',
      dataIndex: 'available_balance',
      key: 'available_balance',
      width: 140,
      align: 'right',
      render: (v: number, row) => displayMoney(String(v), row.currency),
    },
    {
      title: 'Frozen',
      dataIndex: 'frozen_balance',
      key: 'frozen_balance',
      width: 120,
      align: 'right',
      render: (v: number, row) => displayMoney(String(v), row.currency),
    },
    { title: '本期开始', dataIndex: 'period_start', key: 'period_start', width: 180, render: fmt },
    { title: '本期结束', dataIndex: 'period_end', key: 'period_end', width: 180, render: fmt },
    {
      title: 'Draining 起',
      dataIndex: 'draining_started_at',
      key: 'draining_started_at',
      width: 180,
      render: fmt,
    },
    { title: 'Frozen 时', dataIndex: 'frozen_at', key: 'frozen_at', width: 180, render: fmt },
    { title: 'Archived 时', dataIndex: 'archived_at', key: 'archived_at', width: 180, render: fmt },
    {
      title: 'Policy / Override',
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
          返回列表
        </Button>
        <Button icon={<ReloadOutlined />} onClick={load} loading={loading}>
          刷新
        </Button>
      </Space>

      <Title level={3}>{decodedKey}</Title>
      <Paragraph type="secondary">
        该 logical_account 下所有 instance（含已 archived）的状态 + 余额。
        归档 instance 的 <Text code>balance</Text> 必须为 0（不变量 I4）；本页用绿色"0"标识。
      </Paragraph>

      {data && !data.rotation_enabled && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="该 logical_account 未启用轮换"
          description="rotation_enabled = false；账户工作在 legacy 模式，单 instance 永久 active。"
        />
      )}
      {data && !data.all_instances_zero && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="存在非零余额的非 active instance"
          description="archived / frozen 的 instance 余额应为 0；若长期非零，请检查 Convergence Job / Migration Job 日志。"
        />
      )}

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={6}>
          <Card>
            <Statistic title="Currency" value={data?.currency || '—'} />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title="Instance 总数"
              value={data?.instances.length || 0}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title="所有余额之和"
              value={data ? displayMoney(String(data.total_balance), data.currency) : '—'}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title="全部归零?"
              value={data?.all_instances_zero ? '是 ✓' : '否 ⚠'}
              valueStyle={{
                color: data?.all_instances_zero ? '#52c41a' : '#fa8c16',
              }}
            />
          </Card>
        </Col>
      </Row>

      {data && (
        <Card title="Phase 分布" style={{ marginBottom: 16 }} size="small">
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
        <Descriptions.Item label="logical_account_id">
          {data?.logical_account_id || '—'}
        </Descriptions.Item>
        <Descriptions.Item label="rotation_enabled">
          {data?.rotation_enabled ? '是' : '否'}
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
