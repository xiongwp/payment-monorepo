import { Card, Descriptions, Table, Tag, Spin, Alert } from 'antd'
import { useParams } from 'react-router-dom'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { getAccount, getTransactionList } from '../../api/accounting'
import type { Account, AccountTransaction } from '../../types/accounting'
import { AccountStatus, AccountType, AccountCategory, BusinessType, BookingType } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const STATUS_COLOR: Record<number, string> = {
  [AccountStatus.DISABLED]: 'default',
  [AccountStatus.ACTIVE]:   'green',
  [AccountStatus.FROZEN]:   'orange',
}

export default function AccountDetail() {
  const { t } = useTranslation('account')
  const { accountNo } = useParams<{ accountNo: string }>()
  const [account, setAccount] = useState<Account | null>(null)
  const [accountLoading, setAccountLoading] = useState(true)
  const [accountError, setAccountError] = useState<string | null>(null)

  const [transactions, setTransactions] = useState<AccountTransaction[]>([])
  const [total, setTotal] = useState(0)
  const [txLoading, setTxLoading] = useState(false)
  const [page, setPage] = useState(1)
  const pageSize = 10

  const ACCOUNT_TYPE_LABEL: Record<number, string> = {
    [AccountType.USER]:     t('accountTypes.user'),
    [AccountType.MERCHANT]: t('accountTypes.merchant'),
    [AccountType.PLATFORM]: t('accountTypes.platformSimple'),
    [AccountType.TRANSIT]:  t('accountTypes.transit'),
    [AccountType.TRANSIT_CHANNEL_RECEIVABLE]: t('accountTypes.transitChannelReceivable'),
    [AccountType.TRANSIT_CHANNEL_PAYABLE]: t('accountTypes.transitChannelPayable'),
    [AccountType.MERCHANT_PENDING_SETTLE]: t('accountTypes.merchantPendingSettleLong'),
    [AccountType.CHARGE_FEE]: t('accountTypes.chargeFee'),
    [AccountType.TRANSACTION_FEE]: t('accountTypes.transactionFee'),
  }

  const BOOKING_TYPE_LABEL: Record<number, string> = {
    [BookingType.NORMAL_BOOKING]:    t('bookingTypes.normal'),
    [BookingType.BUFFER_BOOKING]:    t('bookingTypes.buffer'),
  }

  const ACCOUNT_CATEGORY_LABEL: Record<number, string> = {
    [AccountCategory.ASSET]:     t('categories.asset'),
    [AccountCategory.LIABILITY]: t('categories.liability'),
    [AccountCategory.EQUITY]:    t('categories.equity'),
    [AccountCategory.REVENUE]:   t('categories.revenue'),
    [AccountCategory.EXPENSE]:   t('categories.expense'),
  }

  const BUSINESS_TYPE_LABEL: Record<number, string> = {
    [BusinessType.TRANSFER]:   t('businessTypes.transfer'),
    [BusinessType.PAYMENT]:    t('businessTypes.payment'),
    [BusinessType.REFUND]:     t('businessTypes.refund'),
    [BusinessType.WITHDRAW]:   t('businessTypes.withdraw'),
    [BusinessType.DEPOSIT]:    t('businessTypes.deposit'),
    [BusinessType.COMMISSION]: t('businessTypes.commission'),
  }

  const STATUS_LABEL: Record<number, string> = {
    [AccountStatus.DISABLED]: t('statusLabels.disabled'),
    [AccountStatus.ACTIVE]:   t('statusLabels.active'),
    [AccountStatus.FROZEN]:   t('statusLabels.frozen'),
  }

  useEffect(() => {
    if (!accountNo) return
    setAccountLoading(true)
    setAccountError(null)
    getAccount(accountNo)
      .then(setAccount)
      .catch((err) => setAccountError(err?.message ?? t('detail.loadFailed')))
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
    { title: t('detail.txColumns.transactionId'), dataIndex: 'transaction_id', key: 'transaction_id', ellipsis: true },
    { title: t('detail.txColumns.businessNo'), dataIndex: 'business_no', key: 'business_no', ellipsis: true },
    {
      title: t('detail.txColumns.businessType'),
      dataIndex: 'business_type',
      key: 'business_type',
      render: (t: number) => BUSINESS_TYPE_LABEL[t] ?? t,
    },
    {
      title: t('detail.txColumns.debit'),
      dataIndex: 'debit_amount',
      key: 'debit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('detail.txColumns.credit'),
      dataIndex: 'credit_amount',
      key: 'credit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('detail.txColumns.bookingType'),
      dataIndex: 'booking_type',
      key: 'booking_type',
      render: (t: unknown) => BOOKING_TYPE_LABEL[t as number] ?? t,
    },
    {
      title: t('detail.txColumns.balanceBefore'),
      dataIndex: 'balance_before',
      key: 'balance_before',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('detail.txColumns.balanceAfter'),
      dataIndex: 'balance_after',
      key: 'balance_after',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    { title: t('detail.txColumns.transactionDate'), dataIndex: 'transaction_date', key: 'transaction_date' },
  ]

  if (accountLoading) {
    return <Spin tip={t('detail.loading')} style={{ display: 'block', marginTop: 48 }} />
  }
  if (accountError) {
    return <Alert type="error" message={accountError} />
  }
  if (!account) return null

  return (
    <div>
      <Card title={t('detail.cardTitle')} style={{ marginBottom: 16 }}>
        <Descriptions column={2} bordered size="small">
          <Descriptions.Item label={t('detail.fields.accountNo')}>{account.account_no}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.userId')}>{account.user_id}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.accountType')}>
            {ACCOUNT_TYPE_LABEL[account.account_type] ?? account.account_type}
          </Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.accountCategory')}>
            {ACCOUNT_CATEGORY_LABEL[account.account_category] ?? account.account_category}
          </Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.status')}>
            <Tag color={STATUS_COLOR[account.status]}>{STATUS_LABEL[account.status]}</Tag>
          </Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.currency')}>{account.currency || 'PHP'}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.balance')}>{displayMoney(account.balance, account.currency)}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.availableBalance')}>{displayMoney(account.available_balance, account.currency)}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.frozenBalance')}>{displayMoney(account.frozen_balance, account.currency)}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.version')}>{account.version}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.createdAt')}>{account.created_at}</Descriptions.Item>
          <Descriptions.Item label={t('detail.fields.updatedAt')}>{account.updated_at}</Descriptions.Item>
        </Descriptions>
      </Card>

      <Card title={t('detail.txCardTitle')}>
        <Table
          columns={txColumns}
          dataSource={transactions}
          rowKey="transaction_id"
          loading={txLoading}
          pagination={{
            current: page,
            pageSize,
            total,
            showTotal: (count) => t('detail.pagination.showTotal', { count }),
            onChange: setPage,
          }}
        />
      </Card>
    </div>
  )
}
