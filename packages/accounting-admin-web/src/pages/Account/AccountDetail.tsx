import { Card, Descriptions, Table, Tag, Spin, Alert } from 'antd'
import { useParams } from 'react-router-dom'
import { useEffect, useState } from 'react'
import { getAccount, getTransactionList } from '../../api/accounting'
import type { Account, AccountTransaction } from '../../types/accounting'
import { AccountStatus, AccountType, AccountCategory, BusinessType, BookingType } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const ACCOUNT_TYPE_LABEL: Record<number, string> = {
  [AccountType.USER]:     '用户账户',
  [AccountType.MERCHANT]: '商户账户',
  [AccountType.PLATFORM]: '平台账户',
  [AccountType.TRANSIT]:  '中间账户',
  [AccountType.TRANSIT_CHANNEL_RECEIVABLE]: '中间渠道应收账户',
  [AccountType.TRANSIT_CHANNEL_PAYABLE]: '中间渠道应付账户',
  [AccountType.MERCHANT_PENDING_SETTLE]: '商户待结算余额账户',
  [AccountType.CHARGE_FEE]: '平台服务费账户',
  [AccountType.TRANSACTION_FEE]: '平台手续费账户',
}

const BOOKING_TYPE_LABEL: Record<number, string> = {
  [BookingType.NORMAL_BOOKING]:    '实时记账',
  [BookingType.BUFFER_BOOKING]:    '缓冲记账',
}

const ACCOUNT_CATEGORY_LABEL: Record<number, string> = {
  [AccountCategory.ASSET]:     '资产',
  [AccountCategory.LIABILITY]: '负债',
  [AccountCategory.EQUITY]:    '所有者权益',
  [AccountCategory.REVENUE]:   '收入',
  [AccountCategory.EXPENSE]:   '费用',
}

const BUSINESS_TYPE_LABEL: Record<number, string> = {
  [BusinessType.TRANSFER]:   '转账',
  [BusinessType.PAYMENT]:    '支付',
  [BusinessType.REFUND]:     '退款',
  [BusinessType.WITHDRAW]:   '提现',
  [BusinessType.DEPOSIT]:    '充值',
  [BusinessType.COMMISSION]: '手续费',
}

const STATUS_COLOR: Record<number, string> = {
  [AccountStatus.DISABLED]: 'default',
  [AccountStatus.ACTIVE]:   'green',
  [AccountStatus.FROZEN]:   'orange',
}
const STATUS_LABEL: Record<number, string> = {
  [AccountStatus.DISABLED]: '禁用',
  [AccountStatus.ACTIVE]:   '正常',
  [AccountStatus.FROZEN]:   '冻结',
}

export default function AccountDetail() {
  const { accountNo } = useParams<{ accountNo: string }>()
  const [account, setAccount] = useState<Account | null>(null)
  const [accountLoading, setAccountLoading] = useState(true)
  const [accountError, setAccountError] = useState<string | null>(null)

  const [transactions, setTransactions] = useState<AccountTransaction[]>([])
  const [total, setTotal] = useState(0)
  const [txLoading, setTxLoading] = useState(false)
  const [page, setPage] = useState(1)
  const pageSize = 10

  useEffect(() => {
    if (!accountNo) return
    setAccountLoading(true)
    setAccountError(null)
    getAccount(accountNo)
      .then(setAccount)
      .catch((err) => setAccountError(err?.message ?? '查询账户失败'))
      .finally(() => setAccountLoading(false))
  }, [accountNo])

  useEffect(() => {
    if (!accountNo) return
    setTxLoading(true)
    getTransactionList({ account_no: accountNo, page, page_size: pageSize })
      .then((res) => {
        setTransactions(res.list)
        setTotal(res.total)
      })
      .catch(() => {})
      .finally(() => setTxLoading(false))
  }, [accountNo, page])

  const txColumns = [
    { title: '交易ID', dataIndex: 'transaction_id', key: 'transaction_id', ellipsis: true },
    { title: '业务订单号', dataIndex: 'business_no', key: 'business_no', ellipsis: true },
    {
      title: '业务类型',
      dataIndex: 'business_type',
      key: 'business_type',
      render: (t: number) => BUSINESS_TYPE_LABEL[t] ?? t,
    },
    {
      title: '借方',
      dataIndex: 'debit_amount',
      key: 'debit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: '贷方',
      dataIndex: 'credit_amount',
      key: 'credit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: '记账类型',
      dataIndex: 'booking_type',
      key: 'booking_type',
      render: (t: unknown) => BOOKING_TYPE_LABEL[t as number] ?? t,
    },
    {
      title: '交易前余额',
      dataIndex: 'balance_before',
      key: 'balance_before',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: '交易后余额',
      dataIndex: 'balance_after',
      key: 'balance_after',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    { title: '交易日期', dataIndex: 'transaction_date', key: 'transaction_date' },
  ]

  if (accountLoading) {
    return <Spin tip="加载中..." style={{ display: 'block', marginTop: 48 }} />
  }
  if (accountError) {
    return <Alert type="error" message={accountError} />
  }
  if (!account) return null

  return (
    <div>
      <Card title="账户详情" style={{ marginBottom: 16 }}>
        <Descriptions column={2} bordered size="small">
          <Descriptions.Item label="账户号">{account.account_no}</Descriptions.Item>
          <Descriptions.Item label="用户ID">{account.user_id}</Descriptions.Item>
          <Descriptions.Item label="账户类型">
            {ACCOUNT_TYPE_LABEL[account.account_type] ?? account.account_type}
          </Descriptions.Item>
          <Descriptions.Item label="账户类别">
            {ACCOUNT_CATEGORY_LABEL[account.account_category] ?? account.account_category}
          </Descriptions.Item>
          <Descriptions.Item label="账户状态">
            <Tag color={STATUS_COLOR[account.status]}>{STATUS_LABEL[account.status]}</Tag>
          </Descriptions.Item>
          <Descriptions.Item label="币种">{account.currency || 'PHP'}</Descriptions.Item>
          <Descriptions.Item label="余额">{displayMoney(account.balance, account.currency)}</Descriptions.Item>
          <Descriptions.Item label="可用余额">{displayMoney(account.available_balance, account.currency)}</Descriptions.Item>
          <Descriptions.Item label="冻结余额">{displayMoney(account.frozen_balance, account.currency)}</Descriptions.Item>
          <Descriptions.Item label="版本号">{account.version}</Descriptions.Item>
          <Descriptions.Item label="创建时间">{account.created_at}</Descriptions.Item>
          <Descriptions.Item label="更新时间">{account.updated_at}</Descriptions.Item>
        </Descriptions>
      </Card>

      <Card title="交易记录">
        <Table
          columns={txColumns}
          dataSource={transactions}
          rowKey="transaction_id"
          loading={txLoading}
          pagination={{
            current: page,
            pageSize,
            total,
            showTotal: (t) => `共 ${t} 条`,
            onChange: setPage,
          }}
        />
      </Card>
    </div>
  )
}
