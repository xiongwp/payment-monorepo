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
  Select,
  Switch,
} from 'antd'
import {
  SearchOutlined,
  ReloadOutlined,
  SwapOutlined,
  PlusCircleOutlined,
  PlusOutlined,
  InfoCircleOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useNavigate } from 'react-router-dom'
import {
  listRotationLogicalAccounts,
  rotationManualSwitch,
  rotationManualProvision,
  rotationRegisterLogicalAccount,
} from '../../api/accounting'
import type {
  RotationLogicalAccountRow,
  RotationRegisterRequest,
} from '../../types/accounting'
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

// ────────────────────────────────────────────────────────────────
// "创建 LA" 表单常量
// ────────────────────────────────────────────────────────────────

// AllowedKeyPrefixes — 跟 accounting-system model.AllowedKeyPrefixes 对齐
// 每个 prefix 配一个推荐的 account_type（见 model.AccountType 枚举）。
//
// 原则：所有"平台/中间账户"都支持轮换。后端 model.AllowedKeyPrefixes 是 source of truth；
// 这里只是 UI 推荐选项，新增 / 删除 prefix 时两边都要改。
const KEY_PREFIX_OPTIONS = [
  // 渠道侧（4 子类）
  { prefix: 'channel-receivable:',       label: 'channel-receivable: 渠道应收款',        suggestedAccountType: 5 },
  { prefix: 'channel-suspense:',         label: 'channel-suspense: 渠道入金挂账',        suggestedAccountType: 9 },
  { prefix: 'channel-fee:',              label: 'channel-fee: 渠道手续费应付',           suggestedAccountType: 7 },
  { prefix: 'channel-payable:',          label: 'channel-payable: 渠道应付款',           suggestedAccountType: 6 },
  // 平台侧
  { prefix: 'platform-fee-clearing:',    label: 'platform-fee-clearing: 平台待清算费用', suggestedAccountType: 4 },
  { prefix: 'platform-fee-revenue:',     label: 'platform-fee-revenue: 平台手续费收入',  suggestedAccountType: 4 },
  { prefix: 'platform-withdraw-pending:',label: 'platform-withdraw-pending: 平台提现挂账', suggestedAccountType: 9 },
  // 用户侧中间
  { prefix: 'user-suspense:',            label: 'user-suspense: 用户挂账',               suggestedAccountType: 9 },
  // 通用兜底
  { prefix: 'transit:',                  label: 'transit: 通用中间账户',                  suggestedAccountType: 9 },
]

// AccountType 枚举 — 跟 accounting-system model.AccountType 对齐
// 完整枚举见 internal/domain/model/account.go；这里只列 LA 常用的几个
const ACCOUNT_TYPE_OPTIONS = [
  { value: 5, label: '5 - 渠道应收 (channel receivable)' },
  { value: 6, label: '6 - 渠道应付 (channel payable)' },
  { value: 7, label: '7 - 手续费 (fee)' },
  { value: 8, label: '8 - 平台收入 (revenue)' },
  { value: 9, label: '9 - 中间挂账 (suspense / transit)' },
]

// AccountBusinessType 枚举 — 通用 1=资产 / 2=负债 / 3=权益 / 4=收入 / 5=费用
const ACCOUNT_BUSINESS_TYPE_OPTIONS = [
  { value: 1, label: '1 - 资产 (asset)' },
  { value: 2, label: '2 - 负债 (liability)' },
  { value: 3, label: '3 - 权益 (equity)' },
  { value: 4, label: '4 - 收入 (revenue)' },
  { value: 5, label: '5 - 费用 (expense)' },
]

const CURRENCY_OPTIONS = ['USD', 'PHP', 'CNY', 'HKD', 'SGD', 'JPY', 'EUR']

interface RegisterLAModalProps {
  open: boolean
  onClose: () => void
  onDone: () => void
}

function RegisterLAModal(p: RegisterLAModalProps) {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [prefix, setPrefix] = useState<string>(KEY_PREFIX_OPTIONS[0].prefix)
  const [keySuffix, setKeySuffix] = useState<string>('')

  const fullKey = useMemo(() => `${prefix}${keySuffix.trim()}`, [prefix, keySuffix])

  const submit = async () => {
    try {
      const values = await form.validateFields()
      if (!keySuffix.trim()) {
        message.error('请填写 logical_account_key 后缀')
        return
      }
      if (fullKey.length < 8 || fullKey.length > 64) {
        message.error(`完整 key 长度需在 [8, 64]: 当前 ${fullKey.length}`)
        return
      }
      setLoading(true)
      const req: RotationRegisterRequest = {
        logical_account_key:   fullKey,
        account_type:          values.account_type,
        account_business_type: values.account_business_type,
        currency:              values.currency,
        description:           values.description || undefined,
        rotation_enabled:      !!values.rotation_enabled,
        operator:              values.operator,
      }
      await rotationRegisterLogicalAccount(req)
      message.success(`已创建 ${fullKey}`)
      form.resetFields()
      setKeySuffix('')
      p.onClose()
      p.onDone()
    } catch (e) {
      if ((e as { errorFields?: unknown }).errorFields) return // form 校验失败
      message.error(e instanceof Error ? e.message : '创建失败')
    } finally {
      setLoading(false)
    }
  }

  // prefix 切换时自动 suggest 一个 account_type
  const onPrefixChange = (v: string) => {
    setPrefix(v)
    const hit = KEY_PREFIX_OPTIONS.find((o) => o.prefix === v)
    if (hit) form.setFieldValue('account_type', hit.suggestedAccountType)
  }

  return (
    <Modal
      title="创建 LogicalAccount"
      open={p.open}
      onCancel={p.onClose}
      onOk={submit}
      confirmLoading={loading}
      okText="创建"
      cancelText="取消"
      width={640}
      destroyOnClose
    >
      <Alert
        message="LA 是业务侧稳定的账户键，背后挂多个 instance 按周期轮换。注册后 status=enabled；若 rotation_enabled=true，还需点击「预创建」+「立即切换」启动首个 active instance。"
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
      />
      <Form
        form={form}
        layout="vertical"
        preserve={false}
        initialValues={{
          account_type: KEY_PREFIX_OPTIONS[0].suggestedAccountType,
          account_business_type: 2, // 负债（中间账户多数是负债）
          currency: 'PHP',
          rotation_enabled: true,
        }}
      >
        <Form.Item label="logical_account_key 前缀" required>
          <Select
            value={prefix}
            onChange={onPrefixChange}
            options={KEY_PREFIX_OPTIONS.map((o) => ({ value: o.prefix, label: o.label }))}
            style={{ width: '100%' }}
          />
        </Form.Item>
        <Form.Item
          label={`完整 key: ${fullKey || '(填后缀)'}`}
          required
          help="拼接：前缀 + 后缀。完整长度 ∈ [8, 64]，ASCII 可见字符（不含空格）"
        >
          <Input
            value={keySuffix}
            onChange={(e) => setKeySuffix(e.target.value)}
            placeholder="e.g. alipay / wechatpay-cn / merchant-12345"
            addonBefore={prefix}
          />
        </Form.Item>
        <Form.Item
          label="account_type"
          name="account_type"
          rules={[{ required: true, message: '必填' }]}
        >
          <Select options={ACCOUNT_TYPE_OPTIONS} />
        </Form.Item>
        <Form.Item
          label="account_business_type"
          name="account_business_type"
          rules={[{ required: true, message: '必填' }]}
        >
          <Select options={ACCOUNT_BUSINESS_TYPE_OPTIONS} />
        </Form.Item>
        <Form.Item
          label="币种 (currency)"
          name="currency"
          rules={[{ required: true, message: '必填' }]}
        >
          <Select options={CURRENCY_OPTIONS.map((c) => ({ value: c, label: c }))} />
        </Form.Item>
        <Form.Item
          label="启用轮换 (rotation_enabled)"
          name="rotation_enabled"
          valuePropName="checked"
          tooltip="false → 单 instance 兼容模式；true → 启用轮换，可点「预创建」「立即切换」"
        >
          <Switch />
        </Form.Item>
        <Form.Item label="描述 (可选)" name="description">
          <Input.TextArea rows={2} placeholder="e.g. 支付宝渠道应付款主账户" />
        </Form.Item>
        <Form.Item
          label="Operator (操作人邮箱 / 工号)"
          name="operator"
          rules={[{ required: true, message: '必填，写入 registered_by 做审计' }]}
        >
          <Input placeholder="e.g. ops-alice@example.com" />
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
  const [registerOpen, setRegisterOpen] = useState(false)

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
          <Form.Item>
            <Button
              type="primary"
              ghost
              icon={<PlusOutlined />}
              onClick={() => setRegisterOpen(true)}
            >
              创建 LA
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

      <RegisterLAModal
        open={registerOpen}
        onClose={() => setRegisterOpen(false)}
        onDone={load}
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
