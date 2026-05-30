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
import { useTranslation, Trans } from 'react-i18next'
import type { TFunction } from 'i18next'
import {
  listRotationLogicalAccounts,
  rotationManualSwitch,
  rotationManualProvision,
  rotationRegisterLogicalAccount,
  listBusinessTypes,
  CATEGORY_BY_ACCOUNT_TYPE,
  CATEGORY_LABEL,
  ACCOUNT_TYPE_LABEL,
} from '../../api/accounting'
import type {
  RotationLogicalAccountRow,
  RotationRegisterRequest,
  BusinessTypeInfo,
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

function fmtDuration(seconds: number, t: TFunction): string {
  if (seconds < 0) return t('duration.expiredPrefix', { value: fmtDuration(-seconds, t) })
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
  const { t } = useTranslation('rotation')
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
      message.success(p.action === 'switch' ? t('manualOp.switchSuccess') : t('manualOp.provisionSuccess'))
      form.resetFields()
      p.onClose()
      p.onDone()
    } catch (e) {
      if ((e as { errorFields?: unknown }).errorFields) return // form 校验失败
      message.error(e instanceof Error ? e.message : t('manualOp.operationFailed'))
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
      okText={t('manualOp.okText')}
      cancelText={t('common:actions.cancel')}
      destroyOnClose
    >
      <Alert
        message={
          p.action === 'switch'
            ? t('manualOp.switchAlert')
            : t('manualOp.provisionAlert')
        }
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
      />
      <Form form={form} layout="vertical" preserve={false}>
        <Form.Item label={t('manualOp.logicalAccountKeyLabel')}>
          <Input value={p.logicalAccountKey} disabled />
        </Form.Item>
        <Form.Item
          label={t('manualOp.operatorLabel')}
          name="operator"
          rules={[{ required: true, message: t('manualOp.operatorRequired') }]}
        >
          <Input placeholder={t('manualOp.operatorPlaceholder')} />
        </Form.Item>
        <Form.Item
          label={t('manualOp.reasonLabel')}
          name="reason"
          rules={[{ required: true, message: t('manualOp.reasonRequired') }]}
        >
          <Input.TextArea
            rows={3}
            placeholder={t('manualOp.reasonPlaceholder')}
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
  { prefix: 'channel-receivable:',       labelKey: 'keyPrefixes.channelReceivable',       suggestedAccountType: 5 },
  { prefix: 'channel-suspense:',         labelKey: 'keyPrefixes.channelSuspense',         suggestedAccountType: 9 },
  { prefix: 'channel-fee:',              labelKey: 'keyPrefixes.channelFee',              suggestedAccountType: 7 },
  { prefix: 'channel-payable:',          labelKey: 'keyPrefixes.channelPayable',          suggestedAccountType: 6 },
  // 平台侧
  { prefix: 'platform-fee-clearing:',    labelKey: 'keyPrefixes.platformFeeClearing',     suggestedAccountType: 4 },
  { prefix: 'platform-fee-revenue:',     labelKey: 'keyPrefixes.platformFeeRevenue',      suggestedAccountType: 4 },
  { prefix: 'platform-withdraw-pending:',labelKey: 'keyPrefixes.platformWithdrawPending', suggestedAccountType: 9 },
  // 用户侧中间
  { prefix: 'user-suspense:',            labelKey: 'keyPrefixes.userSuspense',            suggestedAccountType: 9 },
  // 通用兜底
  { prefix: 'transit:',                  labelKey: 'keyPrefixes.transit',                 suggestedAccountType: 9 },
]

// business_type / account_type / category 的关系（见"业务类型管理"页面）：
//   business_type (registry 主键, 1-999) ──查表──→ account_type (1-9)
//   account_type ──CATEGORY_BY_ACCOUNT_TYPE 派生──→ category (ASSET / LIABILITY / EQUITY / REVENUE / EXPENSE)
//
// 创建 LA 表单：用户只选 business_type；account_type 自动从 registry 推；
// category 派生展示。category 不存表，只是 UI 提示用户"这是什么会计本质"。

const CURRENCY_OPTIONS = ['USD', 'PHP', 'CNY', 'HKD', 'SGD', 'JPY', 'EUR']

interface RegisterLAModalProps {
  open: boolean
  onClose: () => void
  onDone: () => void
}

function RegisterLAModal(p: RegisterLAModalProps) {
  const { t } = useTranslation('rotation')
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)

  // business_type registry(来自 /v1/business-types),首次打开 modal 时拉取。
  // LA↔biz_type 是 1:1,选 biz 后 logical_account_key 完全从 biz_code 反推,
  // 不再要 prefix 下拉 + full-key 输入(用户出错的来源)。
  const [btRegistry, setBtRegistry] = useState<BusinessTypeInfo[]>([])
  const [btLoading, setBtLoading] = useState(false)
  const [selectedBT, setSelectedBT] = useState<number | undefined>(undefined)

  useEffect(() => {
    if (!p.open || btRegistry.length > 0) return
    setBtLoading(true)
    listBusinessTypes()
      .then((rows) => {
        // 只显示 enabled=1 的，禁用的不让选
        setBtRegistry(rows.filter((r) => r.enabled === 1))
      })
      .catch((e) => message.error(e instanceof Error ? e.message : t('register.loadRegistryFailed')))
      .finally(() => setBtLoading(false))
  }, [p.open, btRegistry.length, t])

  // 选中 business_type 时派生 account_type + category（不让用户手填，避免 schema drift）
  const derived = useMemo(() => {
    if (selectedBT === undefined) return null
    const hit = btRegistry.find((r) => r.business_type === selectedBT)
    if (!hit) return null
    const accountType = hit.account_type
    const categoryNum = CATEGORY_BY_ACCOUNT_TYPE[accountType] ?? 0
    return {
      accountType,
      accountTypeLabel: ACCOUNT_TYPE_LABEL[accountType] || `account_type=${accountType}`,
      category: CATEGORY_LABEL[categoryNum] || 'UNKNOWN',
      code: hit.business_type_code,
      description: hit.description ?? '',
    }
  }, [selectedBT, btRegistry])

  // 从 biz_code 反推 logical_account_key。两条规则,从严到松:
  //
  // 规则 1 (strict): biz_code 是 <PREFIX_UPPER_UNDERSCORED>_<SUFFIX_UPPER> 形式
  //   (fleet-prepare 注册的 biz 都长这样,例 CHANNEL_RECEIVABLE_ALIPAY)。
  //   取 KEY_PREFIX_OPTIONS 每个 prefix 把 - 和 : 都换成 _ 大写,跟 biz_code 前缀匹配,
  //   最长匹配胜出,剩余部分 lowercase 当 suffix。
  //
  // 规则 2 (fallback): 预置 biz 的 code 是单段语义名(TRANSACTION_FEE / TRANSIT /
  //   PLATFORM_PNL ...),没法按规则 1 切分。按 biz 的 account_type 在 KEY_PREFIX_OPTIONS
  //   里找 suggestedAccountType 匹配的 prefix(账号类型 4-9 都至少有一个 prefix 匹配);
  //   多个 prefix 匹配同一 account_type 时取字母序首位,deterministic。
  //   suffix = biz_code lowercased + _ 换 -(例 TRANSACTION_FEE → transaction-fee)。
  //
  // 都不匹配返回 null(只有 account_type 1-3 = user/merchant 业务账户会走到这,
  // 这些本来就不该进 LA 体系,UI 也已经过滤掉了)。
  const derivedKey = useMemo<string | null>(() => {
    if (!derived) return null
    const bizCode = derived.code
    if (!bizCode) return null
    // ── 规则 1 严格匹配(带 account_type 守卫,避免 TRANSIT_CHANNEL_RECEIVABLE 这种
    // 字符串以 TRANSIT_ 开头但 account_type=5 应走 channel-receivable: 的误判)─────
    const sortedByLen = [...KEY_PREFIX_OPTIONS].sort((a, b) => b.prefix.length - a.prefix.length)
    for (const opt of sortedByLen) {
      if (opt.suggestedAccountType !== derived.accountType) continue
      const pat = opt.prefix.replace(/[-:]/g, '_').toUpperCase()
      if (bizCode.startsWith(pat)) {
        const suffix = bizCode.slice(pat.length).toLowerCase()
        if (suffix) return `${opt.prefix}${suffix}`
      }
    }
    // ── 规则 2 按 account_type fallback ─────────────────────────────────
    const matchingPrefixes = KEY_PREFIX_OPTIONS
      .filter((o) => o.suggestedAccountType === derived.accountType)
      .sort((a, b) => a.prefix.localeCompare(b.prefix))
    if (matchingPrefixes.length > 0) {
      const suffix = bizCode.toLowerCase().replace(/_/g, '-')
      return `${matchingPrefixes[0].prefix}${suffix}`
    }
    return null
  }, [derived])

  const submit = async () => {
    try {
      const values = await form.validateFields()
      if (!derived) {
        message.error(t('register.businessTypeRequired'))
        return
      }
      if (!derivedKey) {
        message.error(t('register.deriveKeyFailed', { code: derived.code }))
        return
      }
      if (derivedKey.length < 8 || derivedKey.length > 64) {
        message.error(t('register.keyLengthError', { length: derivedKey.length }))
        return
      }
      setLoading(true)
      const req: RotationRegisterRequest = {
        logical_account_key:   derivedKey,                  // 完全由 biz_code 反推,用户不填
        account_type:          derived.accountType,         // biz registry 派生,用户不填
        account_business_type: values.business_type,        // 用户唯一要选的
        currency:              values.currency,
        description:           values.description || undefined,
        rotation_enabled:      !!values.rotation_enabled,
        operator:              values.operator,
      }
      await rotationRegisterLogicalAccount(req)
      message.success(t('register.createdSuccess', { key: derivedKey }))
      form.resetFields()
      setSelectedBT(undefined)
      p.onClose()
      p.onDone()
    } catch (e) {
      if ((e as { errorFields?: unknown }).errorFields) return // form 校验失败
      message.error(e instanceof Error ? e.message : t('register.createFailed'))
    } finally {
      setLoading(false)
    }
  }

  // category 字符串 → tag 颜色
  const categoryColor = (cat: string): string => {
    switch (cat) {
      case 'ASSET':     return 'blue'
      case 'LIABILITY': return 'green'
      case 'EQUITY':    return 'purple'
      case 'REVENUE':   return 'orange'
      case 'EXPENSE':   return 'red'
      default:          return 'default'
    }
  }

  return (
    <Modal
      title={t('register.title')}
      open={p.open}
      onCancel={p.onClose}
      onOk={submit}
      confirmLoading={loading}
      okText={t('register.okText')}
      cancelText={t('common:actions.cancel')}
      width={680}
      destroyOnClose
    >
      <Alert
        message={t('register.alert')}
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
      />
      <Form
        form={form}
        layout="vertical"
        preserve={false}
        initialValues={{
          currency: 'PHP',
          rotation_enabled: true,
        }}
      >
        {/* LA↔biz_type 是 1:1 — 用户只选 biz,prefix + logical_account_key + account_type 都自动派生。
            biz 选项只列 account_type 4-9(平台 / 中间账户) 的,1-3 是 user/merchant 业务账户,
            不进 LA 体系。 */}
        <Form.Item
          label={t('register.businessTypeLabel')}
          name="business_type"
          rules={[{ required: true, message: t('register.businessTypeRequiredMsg') }]}
          tooltip={t('register.businessTypeTooltip')}
        >
          <Select
            loading={btLoading}
            placeholder={t('register.businessTypePlaceholder')}
            showSearch
            optionFilterProp="label"
            onChange={(v: number) => setSelectedBT(v)}
            options={btRegistry
              .filter((r) => {
                // account_type 4-9 才进 LA 体系(1-3 是 user/merchant 业务账户)。
                if (r.account_type < 4 || r.account_type > 9) return false
                // 还得有至少一个 prefix 匹配该 account_type,否则反推不出 logical_account_key。
                // 当前只有 type=8 (CHARGE_FEE) 没对应 prefix —— 后端 AllowedKeyPrefixes
                // 没登记 platform-service-fee:。等后端补 prefix 后这里自动放开。
                return KEY_PREFIX_OPTIONS.some((o) => o.suggestedAccountType === r.account_type)
              })
              .map((r) => ({
                value: r.business_type,
                label: `${r.business_type} - ${r.business_type_code} (${ACCOUNT_TYPE_LABEL[r.account_type] || `type=${r.account_type}`})`,
              }))}
          />
        </Form.Item>
        {derived && (
          <Alert
            type={derivedKey ? 'success' : 'warning'}
            showIcon={false}
            message={
              <Space direction="vertical" size="small" style={{ width: '100%' }}>
                <Space size="middle" wrap>
                  <span>
                    <Text type="secondary">{t('register.derivedKey')}</Text>{' '}
                    {derivedKey
                      ? <Text code copyable>{derivedKey}</Text>
                      : <Text type="danger">{t('register.derivedKeyFailed', { code: derived.code })}</Text>}
                  </span>
                </Space>
                <Space size="middle" wrap>
                  <span>
                    <Text type="secondary">{t('register.derivedAccountType')}</Text>{' '}
                    <Tag color="cyan">{derived.accountType} - {derived.accountTypeLabel}</Tag>
                  </span>
                  <span>
                    <Text type="secondary">{t('register.derivedCategory')}</Text>{' '}
                    <Tag color={categoryColor(derived.category)}>{derived.category}</Tag>
                  </span>
                  {derived.description && (
                    <span>
                      <Text type="secondary">{t('register.derivedDescription')}</Text> <Text>{derived.description}</Text>
                    </span>
                  )}
                </Space>
              </Space>
            }
            style={{ marginBottom: 16 }}
          />
        )}
        <Form.Item
          label={t('register.currencyLabel')}
          name="currency"
          rules={[{ required: true, message: t('register.currencyRequired') }]}
        >
          <Select options={CURRENCY_OPTIONS.map((c) => ({ value: c, label: c }))} />
        </Form.Item>
        <Form.Item
          label={t('register.rotationEnabledLabel')}
          name="rotation_enabled"
          valuePropName="checked"
          tooltip={t('register.rotationEnabledTooltip')}
        >
          <Switch />
        </Form.Item>
        <Form.Item label={t('register.descriptionLabel')} name="description">
          <Input.TextArea rows={2} placeholder={t('register.descriptionPlaceholder')} />
        </Form.Item>
        <Form.Item
          label={t('register.operatorLabel')}
          name="operator"
          rules={[{ required: true, message: t('register.operatorRequired') }]}
        >
          <Input placeholder={t('register.operatorPlaceholder')} />
        </Form.Item>
      </Form>
    </Modal>
  )
}

export default function RotationDashboard() {
  const { t } = useTranslation('rotation')
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
      message.error(e instanceof Error ? e.message : t('dashboard.loadFailed'))
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
      title: t('columns.logicalAccountKey'),
      dataIndex: 'logical_account_key',
      key: 'logical_account_key',
      width: 280,
      render: (v: string) => (
        <a onClick={() => navigate(`/rotation/${encodeURIComponent(v)}`)}>{v}</a>
      ),
    },
    {
      title: t('columns.currency'),
      dataIndex: 'currency',
      key: 'currency',
      width: 80,
    },
    {
      title: t('columns.rotation'),
      dataIndex: 'rotation_enabled',
      key: 'rotation_enabled',
      width: 90,
      render: (v: boolean) =>
        v ? <Tag color="green">{t('columns.rotationEnabled')}</Tag> : <Tag color="default">{t('columns.rotationDisabled')}</Tag>,
    },
    {
      title: t('columns.activeAccount'),
      dataIndex: 'active_account_no',
      key: 'active_account_no',
      width: 220,
      ellipsis: true,
      render: (v: string) => v || <Text type="secondary">—</Text>,
    },
    {
      title: t('columns.balance'),
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
      title: t('columns.periodEnd'),
      dataIndex: 'period_end',
      key: 'period_end',
      width: 200,
      render: (v: string, row) => (
        <Space direction="vertical" size={0}>
          <Text>{fmtTime(v)}</Text>
          {row.rotation_enabled && (
            <Tag color={ttlColor(row.time_to_end_seconds)}>
              {fmtDuration(row.time_to_end_seconds, t)}
            </Tag>
          )}
        </Space>
      ),
    },
    {
      title: t('columns.provisionedReady'),
      dataIndex: 'provisioned_ready',
      key: 'provisioned_ready',
      width: 130,
      render: (v: boolean, row) =>
        v ? (
          <Tooltip title={row.provisioned_account_no || ''}>
            <Tag color="cyan">{t('columns.provisionedReadyYes')}</Tag>
          </Tooltip>
        ) : (
          <Tag color="default">{t('columns.provisionedReadyNo')}</Tag>
        ),
    },
    {
      title: t('columns.actions'),
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
            {t('dashboard.actions.switchNow')}
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
            {t('dashboard.actions.provision')}
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Title level={3}>
        <SwapOutlined /> {t('dashboard.title')}
      </Title>
      <Paragraph type="secondary">
        <Trans i18nKey="dashboard.description" t={t}>
          {'管理需要按周期（月/季/任意）切换的中间账户：channel-payable、channel-receivable、user-suspense 等。点击 '}
          <Text code>logical_account_key</Text>
          {' 进详情页查看每个历史 instance 余额是否归零。'}
        </Trans>
        <Tooltip title={t('dashboard.infoTooltip')}>
          <InfoCircleOutlined style={{ marginLeft: 4 }} />
        </Tooltip>
      </Paragraph>

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={4}>
          <Card><Statistic title={t('dashboard.stats.totalLA')} value={stats.total} /></Card>
        </Col>
        <Col span={5}>
          <Card><Statistic title={t('dashboard.stats.rotating')} value={stats.rotating} suffix={`/ ${stats.total}`} /></Card>
        </Col>
        <Col span={5}>
          <Card>
            <Statistic
              title={t('dashboard.stats.expiring24h')}
              value={stats.expiring}
              valueStyle={{ color: stats.expiring > 0 ? '#fa8c16' : undefined }}
            />
          </Card>
        </Col>
        <Col span={5}>
          <Card>
            <Statistic
              title={t('dashboard.stats.overdue')}
              value={stats.overdue}
              valueStyle={{ color: stats.overdue > 0 ? '#f5222d' : undefined }}
            />
          </Card>
        </Col>
        <Col span={5}>
          <Card><Statistic title={t('dashboard.stats.provisionedNext')} value={stats.provisioned} /></Card>
        </Col>
      </Row>

      <Card style={{ marginBottom: 16 }}>
        <Form layout="inline" onFinish={load}>
          <Form.Item label={t('dashboard.filter.keyPrefix')}>
            <Input
              value={prefix}
              onChange={(e) => setPrefix(e.target.value)}
              placeholder={t('dashboard.filter.keyPrefixPlaceholder')}
              style={{ width: 280 }}
              allowClear
            />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" icon={<SearchOutlined />} loading={loading}>
              {t('common:actions.search')}
            </Button>
          </Form.Item>
          <Form.Item>
            <Button onClick={load} icon={<ReloadOutlined />}>
              {t('common:actions.refresh')}
            </Button>
          </Form.Item>
          <Form.Item>
            <Button
              type="primary"
              ghost
              icon={<PlusOutlined />}
              onClick={() => setRegisterOpen(true)}
            >
              {t('dashboard.actions.createLA')}
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
        title={modal.action === 'switch' ? t('manualOp.switchTitle') : t('manualOp.provisionTitle')}
        action={modal.action}
        logicalAccountKey={modal.key}
        onClose={() => setModal((m) => ({ ...m, open: false }))}
        onDone={load}
      />
    </div>
  )
}
