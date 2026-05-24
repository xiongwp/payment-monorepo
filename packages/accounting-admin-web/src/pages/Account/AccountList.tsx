import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Table, Button, Input, Space, Tag, Modal, Form, Select, message, Radio } from 'antd'
import { SearchOutlined, PlusOutlined, LockOutlined, UnlockOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import {
  createAccount,
  getAccountByQuery,
  getAccountByUserAndBusinessType,
  listBusinessTypes,
  CATEGORY_BY_ACCOUNT_TYPE,
  CATEGORY_LABEL,
  BUSINESS_ACCOUNT_TYPES,
  BUSINESS_ACCOUNT_TYPE_VALUES,
} from '../../api/accounting'
import { AccountType, AccountBusinessType, AccountStatus } from '../../types/accounting'
import type { Account, BusinessTypeInfo } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'


type SearchMode = 'account_no' | 'user_business_type'

export default function AccountList() {
  const { t } = useTranslation('account')
  const ACCOUNT_TYPE_LABELS: Record<number, string> = {
    [AccountType.USER]: t('accountTypes.user'),
    [AccountType.MERCHANT]: t('accountTypes.merchant'),
    [AccountType.MERCHANT_PENDING_SETTLE]: t('accountTypes.merchantPendingSettle'),
    [AccountType.PLATFORM]: t('accountTypes.platform'),
    [AccountType.TRANSIT_CHANNEL_RECEIVABLE]: t('accountTypes.transitChannelReceivable'),
    [AccountType.TRANSIT_CHANNEL_PAYABLE]: t('accountTypes.transitChannelPayable'),
    [AccountType.TRANSACTION_FEE]: t('accountTypes.transactionFee'),
    [AccountType.CHARGE_FEE]: t('accountTypes.chargeFee'),
    [AccountType.TRANSIT]: t('accountTypes.transit'),
  }

  const [searchMode, setSearchMode] = useState<SearchMode>('account_no')
  const [accountNoInput, setAccountNoInput] = useState('')
  const [userIdInput, setUserIdInput] = useState('')
  const [bizTypeInput, setBizTypeInput] = useState<number | undefined>()
  const [loading, setLoading] = useState(false)
  const [accounts, setAccounts] = useState<Account[]>([])
  const [isModalVisible, setIsModalVisible] = useState(false)
  const [form] = Form.useForm()
  // 业务类型注册表（从 meta DB 拉）：
  //   - 创建账户表单用来根据选中的 accountType 动态过滤合法 (business_type, category)
  //   - 顶部 "按用户ID+业务类型查询" 筛选框也用它，这样 BusinessTypes 页新增的
  //     business_type 会立刻出现在这里（之前是硬编码 3 项，新增后不可见）。
  const [btRegistry, setBtRegistry] = useState<BusinessTypeInfo[]>([])

  // 进入页面就拉一次，保证搜索筛选可见；打开创建弹窗时再刷一次，保证刚注册的也能看到。
  useEffect(() => {
    listBusinessTypes().then(setBtRegistry).catch(() => setBtRegistry([]))
  }, [])
  useEffect(() => {
    if (isModalVisible) {
      listBusinessTypes().then(setBtRegistry).catch(() => {/* keep previous */})
    }
  }, [isModalVisible])

  // 顶部筛选：只展示业务账户（is_platform=0）下的 business_type。
  // 平台内部渠道账户（ALIPAY_RECEIVABLE 等）不允许按 userID 查，避免把
  // reserved_id 0..99 曝露给这个通道。
  const filterBusinessTypes = useMemo(
    () => btRegistry
      .filter(r => r.enabled === 1 && BUSINESS_ACCOUNT_TYPE_VALUES.has(r.account_type))
      .sort((a, b) => a.business_type - b.business_type),
    [btRegistry],
  )
  // 业务类型列表格展示：从 registry 查 code；registry 还没加载时落到 USER_BALANCE 等系统默认。
  const businessTypeLabel = (v: number): string => {
    const row = btRegistry.find(r => r.business_type === v)
    if (row) return `${v} · ${row.business_type_code}`
    if (v === AccountBusinessType.USER_BALANCE) return t('businessTypes.userBalance')
    if (v === AccountBusinessType.MERCHANT_BALANCE) return t('businessTypes.merchantBalance')
    if (v === AccountBusinessType.MERCHANT_PENDING_SETTLE) return t('businessTypes.merchantPendingSettle')
    return String(v)
  }

  // Watch accountType 变化 → 清空依赖它的 business_type + 自动带入 category
  const selectedAccountType = Form.useWatch('accountType', form) as number | undefined
  const allowedBusinessTypes = useMemo(() => {
    if (!selectedAccountType) return []
    // 额外保险：即使下拉限制过了，也只接受业务账户 type 的 business_type（防御性）
    if (!BUSINESS_ACCOUNT_TYPE_VALUES.has(selectedAccountType)) return []
    return btRegistry.filter(r => r.account_type === selectedAccountType && r.enabled === 1)
  }, [btRegistry, selectedAccountType])
  // 选中的 businessType → 查到它绑定的 account_type → 由 account_type 派生 category（数字枚举）
  // (category 不存 registry 表，由 CATEGORY_BY_ACCOUNT_TYPE 单一权威推导)
  // 保存的是 AccountCategory 数字，后端 gRPC req.category 是 int32；展示时用 CATEGORY_LABEL 映射。
  const selectedBusinessType = Form.useWatch('accountBusinessType', form) as number | undefined
  const derivedCategory = useMemo(() => {
    const row = btRegistry.find(r => r.business_type === selectedBusinessType)
    if (!row) return 0 // 0 表示未选
    return CATEGORY_BY_ACCOUNT_TYPE[row.account_type] ?? 0
  }, [btRegistry, selectedBusinessType])
  useEffect(() => {
    // accountType 变化时清理 businessType，避免保留非法组合
    form.setFieldValue('accountBusinessType', undefined)
  }, [selectedAccountType, form])
  useEffect(() => {
    form.setFieldValue('category', derivedCategory)
  }, [derivedCategory, form])

  const handleSearch = async () => {
    setLoading(true)
    try {
      if (searchMode === 'account_no') {
        if (!accountNoInput.trim()) {
          message.warning(t('list.messages.requireAccountNo'))
          return
        }
        const acc = await getAccountByQuery(accountNoInput.trim())
        setAccounts([acc])
      } else {
        if (!userIdInput.trim() || bizTypeInput === undefined) {
          message.warning(t('list.messages.requireUserAndBusinessType'))
          return
        }
        const resp = await getAccountByUserAndBusinessType(Number(userIdInput), bizTypeInput)
        setAccounts(resp.accounts || [])
        if ((resp.count ?? 0) === 0) {
          message.info(t('list.messages.notFound'))
        } else if ((resp.count ?? 0) > 1) {
          message.info(t('list.messages.multipleCurrencies', { count: resp.count }))
        }
      }
    } catch (err: any) {
      message.error(err.message || t('list.messages.queryFailed'))
      setAccounts([])
    } finally {
      setLoading(false)
    }
  }

  const handleCreateAccount = async () => {
    try {
      const values = await form.validateFields()
      await createAccount({
        user_id: Number(values.userId),
        account_type: values.accountType,
        category: values.category,
        account_business_type: values.accountBusinessType,
        currency: values.currency || 'PHP',
      })
      message.success(t('list.messages.createSuccess'))
      setIsModalVisible(false)
      form.resetFields()
    } catch (err: any) {
      if (err?.message) message.error(err.message)
    }
  }

  const columns: ColumnsType<Account> = [
    {
      title: t('list.columns.accountNo'),
      dataIndex: 'account_no',
      key: 'account_no',
      fixed: 'left',
    },
    {
      title: t('list.columns.userId'),
      dataIndex: 'user_id',
      key: 'user_id',
    },
    {
      title: t('list.columns.accountType'),
      dataIndex: 'account_type',
      key: 'account_type',
      render: (v: number) => ACCOUNT_TYPE_LABELS[v] ?? v,
    },
    {
      title: t('list.columns.businessType'),
      dataIndex: 'account_business_type',
      key: 'account_business_type',
      render: (v: number) => businessTypeLabel(v),
    },
    {
      title: t('list.columns.balance'),
      dataIndex: 'balance',
      key: 'balance',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: t('list.columns.availableBalance'),
      dataIndex: 'available_balance',
      key: 'available_balance',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: t('list.columns.currency'),
      dataIndex: 'currency',
      key: 'currency',
    },
    {
      title: t('list.columns.status'),
      dataIndex: 'status',
      key: 'status',
      render: (s: AccountStatus) => {
        const map: Record<number, { color: string; label: string }> = {
          [AccountStatus.ACTIVE]: { color: 'green', label: 'ACTIVE' },
          [AccountStatus.FROZEN]: { color: 'red', label: 'FROZEN' },
          [AccountStatus.DISABLED]: { color: 'default', label: 'DISABLED' },
        }
        const { color, label } = map[s] ?? { color: 'default', label: String(s) }
        return <Tag color={color}>{label}</Tag>
      },
    },
    {
      title: t('common:table.actions'),
      key: 'action',
      fixed: 'right',
      render: (_: any, record: Account) => (
        <Space size="small">
          <Button type="link" size="small" href={`/accounts/${record.account_no}`}>
            {t('list.actions.detail')}
          </Button>
          {record.status === AccountStatus.ACTIVE ? (
            <Button type="link" size="small" danger icon={<LockOutlined />}>
              {t('list.actions.freeze')}
            </Button>
          ) : (
            <Button type="link" size="small" icon={<UnlockOutlined />}>
              {t('list.actions.unfreeze')}
            </Button>
          )}
        </Space>
      ),
    },
  ]

  return (
    <div>
      <div style={{ marginBottom: 16, display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
        <Space direction="vertical" size="small">
          <Radio.Group
            value={searchMode}
            onChange={(e) => {
              setSearchMode(e.target.value)
              setAccounts([])
            }}
          >
            <Radio.Button value="account_no">{t('list.searchMode.byAccountNo')}</Radio.Button>
            <Radio.Button value="user_business_type">{t('list.searchMode.byUserAndBusinessType')}</Radio.Button>
          </Radio.Group>

          {searchMode === 'account_no' ? (
            <Space>
              <Input
                placeholder={t('list.placeholders.accountNo')}
                prefix={<SearchOutlined />}
                value={accountNoInput}
                onChange={(e) => setAccountNoInput(e.target.value)}
                onPressEnter={handleSearch}
                style={{ width: 280 }}
              />
              <Button type="primary" loading={loading} onClick={handleSearch}>
                {t('common:actions.search')}
              </Button>
            </Space>
          ) : (
            <Space>
              <Input
                placeholder={t('list.placeholders.userId')}
                value={userIdInput}
                onChange={(e) => setUserIdInput(e.target.value)}
                style={{ width: 140 }}
              />
              <Select
                placeholder={t('list.placeholders.businessType')}
                value={bizTypeInput}
                onChange={(v) => setBizTypeInput(v)}
                style={{ width: 240 }}
                showSearch
                optionFilterProp="label"
                notFoundContent={t('list.businessTypeEmpty')}
                options={filterBusinessTypes.map(r => ({
                  value: r.business_type,
                  label: `${r.business_type} · ${r.business_type_code}`,
                }))}
              />
              <Button type="primary" loading={loading} onClick={handleSearch}>
                {t('common:actions.search')}
              </Button>
            </Space>
          )}
        </Space>

        <Button type="primary" icon={<PlusOutlined />} onClick={() => setIsModalVisible(true)}>
          {t('list.createButton')}
        </Button>
      </div>

      <Table
        columns={columns}
        dataSource={accounts}
        rowKey="account_no"
        loading={loading}
        pagination={false}
        locale={{ emptyText: t('list.tableEmpty') }}
      />

      <Modal
        title={t('list.modal.title')}
        open={isModalVisible}
        onOk={handleCreateAccount}
        onCancel={() => { setIsModalVisible(false); form.resetFields() }}
      >
        <Form form={form} layout="vertical">
          <Form.Item name="userId" label={t('list.modal.userIdLabel')} rules={[{ required: true }]} tooltip={t('list.modal.userIdTooltip')}>
            <Input placeholder={t('list.modal.userIdPlaceholder')} />
          </Form.Item>
          <Form.Item name="accountType" label={t('list.modal.accountTypeLabel')} rules={[{ required: true }]}>
            <Select
              placeholder={t('list.modal.accountTypePlaceholder')}
              // 只显示业务账户类型（is_platform=0）；平台账户请去"系统账户"页创建
              options={BUSINESS_ACCOUNT_TYPES.map(t => ({ value: t.value, label: t.label }))}
            />
          </Form.Item>
          <Form.Item
            name="accountBusinessType"
            label={t('list.modal.businessTypeLabel')}
            rules={[{ required: true, message: t('list.modal.businessTypeRequired') }]}
            tooltip={t('list.modal.businessTypeTooltip')}
          >
            <Select
              placeholder={selectedAccountType ? t('list.modal.businessTypePlaceholderSelect') : t('list.modal.businessTypePlaceholderPickType')}
              disabled={!selectedAccountType}
              options={allowedBusinessTypes.map(bt => ({
                value: bt.business_type,
                label: `${bt.business_type} · ${bt.business_type_code}`,
              }))}
              notFoundContent={selectedAccountType ? t('list.modal.businessTypeEmpty') : undefined}
            />
          </Form.Item>
          {/*
            form 里 category 字段存数字（enum AccountCategory，后端 gRPC req.category int32）；
            展示用单独一个只读 Input 映射成 ASSET/LIABILITY/…。避免字符串被提交导致
            后端 json.Unmarshal 报 "cannot unmarshal string into Go struct field .category of type int32"。
          */}
          <Form.Item name="category" label={t('list.modal.categoryLabel')} hidden>
            <Input />
          </Form.Item>
          <Form.Item label={t('list.modal.categoryLabel')}>
            <Input value={CATEGORY_LABEL[derivedCategory] || ''} readOnly placeholder={t('list.modal.categoryPlaceholder')} />
          </Form.Item>
          <Form.Item name="currency" label={t('list.modal.currencyLabel')} initialValue="PHP">
            <Select>
              <Select.Option value="PHP">{t('list.modal.currencyOptions.php')}</Select.Option>
              <Select.Option value="USD">{t('list.modal.currencyOptions.usd')}</Select.Option>
            </Select>
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
