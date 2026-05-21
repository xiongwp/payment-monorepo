import { useEffect, useMemo, useState } from 'react'
import {
  Card,
  Form,
  Input,
  Button,
  Table,
  Typography,
  message,
  Tag,
  Space,
  Modal,
  Statistic,
  Row,
  Col,
  Tooltip,
  Alert,
} from 'antd'
import {
  SearchOutlined,
  ReloadOutlined,
  SwapOutlined,
  PlusCircleOutlined,
  InfoCircleOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useNavigate } from 'react-router-dom'
import {
  listRotationLogicalAccounts,
  rotationManualSwitch,
  rotationManualProvision,
} from '../../api/accounting'
import type { RotationLogicalAccountRow } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const { Title, Paragraph, Text } = Typography

// 时间到期颜色：< 1h 红，< 1day 橙，正常蓝，已过期紫
function ttlColor(seconds: number): string {
  if (seconds < 0) return 'magenta'
  if (seconds < 3600) return 'red'
  if (seconds < 86400) return 'orange'
  return 'blue'
}

function fmtDuration(seconds: number): string {
  if (seconds < 0) return `已过期 ${fmtDuration(-seconds)}`
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m${seconds % 60}s`
  if (seconds < 86400) {
    const h = Math.floor(seconds / 3600)
    const m = Math.floor((seconds % 3600) / 60)
    return `${h}h${m}m`
  }
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  return `${d}d${h}h`
}

function fmtTime(rfc3339: string): string {
  if (!rfc3339 || rfc3339.startsWith('0001-')) return '—'
  try {
    return new Date(rfc3339).toLocaleString()
  } catch {
    return rfc3339
  }
}

interface ManualOpModalProps {
  open: boolean
  title: string
  action: 'switch' | 'provision'
  logicalAccountKey: string
  onClose: () => void
  onDone: () => void
}

function ManualOpModal(p: ManualOpModalProps) {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)

  const submit = async () => {
    try {
      const values = await form.validateFields()
      setLoading(true)
      const req = {
        logical_account_key: p.logicalAccountKey,
        operator: values.operator,
        reason: values.reason,
      }
      const fn = p.action === 'switch' ? rotationManualSwitch : rotationManualProvision
      await fn(req)
      message.success(p.action === 'switch' ? '切换成功' : '预创建成功')
      form.resetFields()
      p.onClose()
      p.onDone()
    } catch (e) {
      if ((e as { errorFields?: unknown }).errorFields) return // form 校验失败
      message.error(e instanceof Error ? e.message : '操作失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <Modal
      title={p.title}
      open={p.open}
      onCancel={p.onClose}
      onOk={submit}
      confirmLoading={loading}
      okText="确认执行"
      cancelText="取消"
      destroyOnClose
    >
      <Alert
        message={
          p.action === 'switch'
            ? '将立即把当前 active instance 转为 draining，并把 provisioned 转为 active。'
            : '将立即创建下一期的 provisioned instance（不切换 active）。'
        }
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
      />
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item label="logical_account_key">
          <Input value={p.logicalAccountKey} disabled />
        </Form.Item>
        <Form.Item
          label="Operator（操作人邮箱 / 工号）"
          name="operator"
          rules={[{ required: true, message: '必填，记入审计日志' }]}
        >
          <Input placeholder="e.g. ops-alice@example.com" />
        </Form.Item>
        <Form.Item
          label="Reason（操作原因）"
          name="reason"
          rules={[{ required: true, message: '必填，记入审计日志' }]}
        >
          <Input.TextArea
            rows={3}
            placeholder="e.g. 故障演练 / 月末提前切换 / 余额异常待复核"
          />
        </Form.Item>
      </Form>
    </Modal>
  )
}

export default function RotationDashboard() {
  const navigate = useNavigate()
  const [rows, setRows] = useState<RotationLogicalAccountRow[]>([])
  const [loading, setLoading] = useState(false)
  const [prefix, setPrefix] = useState('')
  const [modal, setModal] = useState<{
    open: boolean
    action: 'switch' | 'provision'
    key: string
  }>({ open: false, action: 'switch', key: '' })

  const load = async () => {
    setLoading(true)
    try {
      const resp = await listRotationLogicalAccounts(prefix || undefined, 500)
      setRows(resp.rows || [])
    } catch (e) {
      message.error(e instanceof Error ? e.message : '加载失败')
      setRows([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const stats = useMemo(() => {
    const total = rows.length
    const rotating = rows.filter((r) => r.rotation_enabled).length
    const expiring = rows.filter(
      (r) => r.rotation_enabled && r.time_to_end_seconds < 86400 && r.time_to_end_seconds >= 0,
    ).length
    const overdue = rows.filter(
      (r) => r.rotation_enabled && r.time_to_end_seconds < 0,
    ).length
    const provisioned = rows.filter((r) => r.provisioned_ready).length
    return { total, rotating, expiring, overdue, provisioned }
  }, [rows])

  const columns: ColumnsType<RotationLogicalAccountRow> = [
    {
      title: 'Logical Account Key',
      dataIndex: 'logical_account_key',
      key: 'logical_account_key',
      width: 280,
      render: (v: string) => (
        <a onClick={() => navigate(`/rotation/${encodeURIComponent(v)}`)}>{v}</a>
      ),
    },
    {
      title: '币种',
      dataIndex: 'currency',
      key: 'currency',
      width: 80,
    },
    {
      title: '轮换',
      dataIndex: 'rotation_enabled',
      key: 'rotation_enabled',
      width: 90,
      render: (v: boolean) =>
        v ? <Tag color="green">已启用</Tag> : <Tag color="default">未启用</Tag>,
    },
    {
      title: '当前 Active 账户',
      dataIndex: 'active_account_no',
      key: 'active_account_no',
      width: 220,
      ellipsis: true,
      render: (v: string) => v || <Text type="secondary">—</Text>,
    },
    {
      title: 'Balance',
      dataIndex: 'active_account_balance',
      key: 'active_account_balance',
      width: 140,
      align: 'right',
      render: (v: number, row) => (
        <Space>
          {displayMoney(String(v), row.currency)}
          {row.active_is_zero && <Tag color="green">0</Tag>}
        </Space>
      ),
    },
    {
      title: '本期截止',
      dataIndex: 'period_end',
      key: 'period_end',
      width: 200,
      render: (v: string, row) => (
        <Space direction="vertical" size={0}>
          <Text>{fmtTime(v)}</Text>
          {row.rotation_enabled && (
            <Tag color={ttlColor(row.time_to_end_seconds)}>
              {fmtDuration(row.time_to_end_seconds)}
            </Tag>
          )}
        </Space>
      ),
    },
    {
      title: '下期已就绪',
      dataIndex: 'provisioned_ready',
      key: 'provisioned_ready',
      width: 130,
      render: (v: boolean, row) =>
        v ? (
          <Tooltip title={row.provisioned_account_no || ''}>
            <Tag color="cyan">已预创建</Tag>
          </Tooltip>
        ) : (
          <Tag color="default">无</Tag>
        ),
    },
    {
      title: '操作',
      key: 'actions',
      width: 200,
      fixed: 'right',
      render: (_, row) => (
        <Space>
          <Button
            size="small"
            type="link"
            icon={<SwapOutlined />}
            disabled={!row.rotation_enabled}
            onClick={() =>
              setModal({ open: true, action: 'switch', key: row.logical_account_key })
            }
          >
            立即切换
          </Button>
          <Button
            size="small"
            type="link"
            icon={<PlusCircleOutlined />}
            disabled={!row.rotation_enabled || row.provisioned_ready}
            onClick={() =>
              setModal({ open: true, action: 'provision', key: row.logical_account_key })
            }
          >
            预创建
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Title level={3}>
        <SwapOutlined /> 轮换账户管理
      </Title>
      <Paragraph type="secondary">
        管理需要按周期（月/季/任意）切换的中间账户：channel-payable、channel-receivable、
        user-suspense 等。点击 <Text code>logical_account_key</Text>{' '}
        进详情页查看每个历史 instance 余额是否归零。
        <Tooltip title="rotation feature 由 accounting-system 中的 Scheduler + Convergence + Migration + Audit 4 个 job 协作完成；本页面对应 service.AdminService（rotation_admin_service.go）。">
          <InfoCircleOutlined style={{ marginLeft: 4 }} />
        </Tooltip>
      </Paragraph>

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={4}>
          <Card><Statistic title="Total LA" value={stats.total} /></Card>
        </Col>
        <Col span={5}>
          <Card><Statistic title="启用轮换" value={stats.rotating} suffix={`/ ${stats.total}`} /></Card>
        </Col>
        <Col span={5}>
          <Card>
            <Statistic
              title="24h 内到期"
              value={stats.expiring}
              valueStyle={{ color: stats.expiring > 0 ? '#fa8c16' : undefined }}
            />
          </Card>
        </Col>
        <Col span={5}>
          <Card>
            <Statistic
              title="已过期"
              value={stats.overdue}
              valueStyle={{ color: stats.overdue > 0 ? '#f5222d' : undefined }}
            />
          </Card>
        </Col>
        <Col span={5}>
          <Card><Statistic title="已预创建下期" value={stats.provisioned} /></Card>
        </Col>
      </Row>

      <Card style={{ marginBottom: 16 }}>
        <Form layout="inline" onFinish={load}>
          <Form.Item label="Key 前缀过滤">
            <Input
              value={prefix}
              onChange={(e) => setPrefix(e.target.value)}
              placeholder="如：channel-payable:"
              style={{ width: 280 }}
              allowClear
            />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" icon={<SearchOutlined />} loading={loading}>
              查询
            </Button>
          </Form.Item>
          <Form.Item>
            <Button onClick={load} icon={<ReloadOutlined />}>
              刷新
            </Button>
          </Form.Item>
        </Form>
      </Card>

      <Table
        rowKey="logical_account_key"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={rows}
        pagination={{ pageSize: 25, showSizeChanger: true }}
        scroll={{ x: 1300, y: 560 }}
      />

      <ManualOpModal
        open={modal.open}
        title={modal.action === 'switch' ? '手动切换中间账户' : '手动预创建下一期'}
        action={modal.action}
        logicalAccountKey={modal.key}
        onClose={() => setModal((m) => ({ ...m, open: false }))}
        onDone={load}
      />
    </div>
  )
}
