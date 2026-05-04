import { useEffect, useMemo, useState } from 'react'
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

const ACCOUNT_TYPE_LABELS: Record<number, string> = {
  [AccountType.USER]: '用户账户',
  [AccountType.MERCHANT]: '商户账户',
  [AccountType.MERCHANT_PENDING_SETTLE]: '商户待结算账户',
  [AccountType.PLATFORM]: '平台损益账户',
  [AccountType.TRANSIT_CHANNEL_RECEIVABLE]: '中间渠道应收账户',
  [AccountType.TRANSIT_CHANNEL_PAYABLE]: '中间渠道应付账户',
  [AccountType.TRANSACTION_FEE]: '平台手续费账户',
  [AccountType.CHARGE_FEE]: '平台服务费账户',
  [AccountType.TRANSIT]: '中间账户',
}


type SearchMode = 'account_no' | 'user_business_type'

export default function AccountList() {
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
    if (v === AccountBusinessType.USER_BALANCE) return '用户余额'
    if (v === AccountBusinessType.MERCHANT_BALANCE) return '商户结算'
    if (v === AccountBusinessType.MERCHANT_PENDING_SETTLE) return '商户待结算'
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
          message.warning('请输入账户号')
          return
        }
        const acc = await getAccountByQuery(accountNoInput.trim())
        setAccounts([acc])
      } else {
        if (!userIdInput.trim() || bizTypeInput === undefined) {
          message.warning('请输入用户ID并选择业务类型')
          return
        }
        const resp = await getAccountByUserAndBusinessType(Number(userIdInput), bizTypeInput)
        setAccounts(resp.accounts || [])
        if ((resp.count ?? 0) === 0) {
          message.info('没有找到对应账户')
        } else if ((resp.count ?? 0) > 1) {
          message.info(`该用户在该业务类型下持有 ${resp.count} 个币种账户`)
        }
      }
    } catch (err: any) {
      message.error(err.message || '查询失败')
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
      message.success('账户创建成功')
      setIsModalVisible(false)
      form.resetFields()
    } catch (err: any) {
      if (err?.message) message.error(err.message)
    }
  }

  const columns: ColumnsType<Account> = [
    {
      title: '账户号',
      dataIndex: 'account_no',
      key: 'account_no',
      fixed: 'left',
    },
    {
      title: '用户ID',
      dataIndex: 'user_id',
      key: 'user_id',
    },
    {
      title: '账户类型',
      dataIndex: 'account_type',
      key: 'account_type',
      render: (v: number) => ACCOUNT_TYPE_LABELS[v] ?? v,
    },
    {
      title: '业务类型',
      dataIndex: 'account_business_type',
      key: 'account_business_type',
      render: (v: number) => businessTypeLabel(v),
    },
    {
      title: '余额',
      dataIndex: 'balance',
      key: 'balance',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: '可用余额',
      dataIndex: 'available_balance',
      key: 'available_balance',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: '币种',
      dataIndex: 'currency',
      key: 'currency',
    },
    {
      title: '状态',
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
      title: '操作',
      key: 'action',
      fixed: 'right',
      render: (_: any, record: Account) => (
        <Space size="small">
          <Button type="link" size="small" href={`/accounts/${record.account_no}`}>
            详情
          </Button>
          {record.status === AccountStatus.ACTIVE ? (
            <Button type="link" size="small" danger icon={<LockOutlined />}>
              冻结
            </Button>
          ) : (
            <Button type="link" size="small" icon={<UnlockOutlined />}>
              解冻
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
            <Radio.Button value="account_no">按账户号查询</Radio.Button>
            <Radio.Button value="user_business_type">按用户ID+业务类型查询</Radio.Button>
          </Radio.Group>

          {searchMode === 'account_no' ? (
            <Space>
              <Input
                placeholder="输入账户号"
                prefix={<SearchOutlined />}
                value={accountNoInput}
                onChange={(e) => setAccountNoInput(e.target.value)}
                onPressEnter={handleSearch}
                style={{ width: 280 }}
              />
              <Button type="primary" loading={loading} onClick={handleSearch}>
                查询
              </Button>
            </Space>
          ) : (
            <Space>
              <Input
                placeholder="用户ID"
                value={userIdInput}
                onChange={(e) => setUserIdInput(e.target.value)}
                style={{ width: 140 }}
              />
              <Select
                placeholder="业务类型"
                value={bizTypeInput}
                onChange={(v) => setBizTypeInput(v)}
                style={{ width: 240 }}
                showSearch
                optionFilterProp="label"
                notFoundContent="请先在「业务类型」页登记 business_type"
                options={filterBusinessTypes.map(r => ({
                  value: r.business_type,
                  label: `${r.business_type} · ${r.business_type_code}`,
                }))}
              />
              <Button type="primary" loading={loading} onClick={handleSearch}>
                查询
              </Button>
            </Space>
          )}
        </Space>

        <Button type="primary" icon={<PlusOutlined />} onClick={() => setIsModalVisible(true)}>
          创建账户
        </Button>
      </div>

      <Table
        columns={columns}
        dataSource={accounts}
        rowKey="account_no"
        loading={loading}
        pagination={false}
        locale={{ emptyText: '请输入查询条件' }}
      />

      <Modal
        title="创建账户"
        open={isModalVisible}
        onOk={handleCreateAccount}
        onCancel={() => { setIsModalVisible(false); form.resetFields() }}
      >
        <Form form={form} layout="vertical">
          <Form.Item name="userId" label="用户ID" rules={[{ required: true }]} tooltip="用户 base 1e8、商户 base 9e8、系统保留 [1,10000]">
            <Input placeholder="请输入用户ID" />
          </Form.Item>
          <Form.Item name="accountType" label="账户类型" rules={[{ required: true }]}>
            <Select
              placeholder="请选择账户类型"
              // 只显示业务账户类型（is_platform=0）；平台账户请去"系统账户"页创建
              options={BUSINESS_ACCOUNT_TYPES.map(t => ({ value: t.value, label: t.label }))}
            />
          </Form.Item>
          <Form.Item
            name="accountBusinessType"
            label="业务类型"
            rules={[{ required: true, message: '请先选择账户类型，再选业务类型' }]}
            tooltip="仅显示与选中账户类型关联、且已在 meta DB 登记的 business_type"
          >
            <Select
              placeholder={selectedAccountType ? '请选择业务类型' : '请先选择账户类型'}
              disabled={!selectedAccountType}
              options={allowedBusinessTypes.map(bt => ({
                value: bt.business_type,
                label: `${bt.business_type} · ${bt.business_type_code}`,
              }))}
              notFoundContent={selectedAccountType ? '该 account_type 下暂无已登记 business_type；请先在"系统账户"页渠道注册' : undefined}
            />
          </Form.Item>
          {/*
            form 里 category 字段存数字（enum AccountCategory，后端 gRPC req.category int32）；
            展示用单独一个只读 Input 映射成 ASSET/LIABILITY/…。避免字符串被提交导致
            后端 json.Unmarshal 报 "cannot unmarshal string into Go struct field .category of type int32"。
          */}
          <Form.Item name="category" label="账户分类 (自动由业务类型推导)" hidden>
            <Input />
          </Form.Item>
          <Form.Item label="账户分类 (自动由业务类型推导)">
            <Input value={CATEGORY_LABEL[derivedCategory] || ''} readOnly placeholder="选择业务类型后自动填写" />
          </Form.Item>
          <Form.Item name="currency" label="币种" initialValue="PHP">
            <Select>
              <Select.Option value="PHP">菲律宾比索</Select.Option>
              <Select.Option value="USD">美元</Select.Option>
            </Select>
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
