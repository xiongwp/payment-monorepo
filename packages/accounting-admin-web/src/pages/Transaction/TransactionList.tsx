import { Table, Input, Button, Space, DatePicker, Form } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import { useState } from 'react'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import { getTransactionList } from '../../api/accounting'
import type { AccountTransaction } from '../../types/accounting'
import { BookingType, BusinessType } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const { RangePicker } = DatePicker

interface SearchValues {
  account_no?: string;
  business_no?: string;
  transaction_id?: string;
  dateRange?: [dayjs.Dayjs, dayjs.Dayjs];
}

export default function TransactionList() {
  const { t } = useTranslation('transaction')
  const [form] = Form.useForm<SearchValues>()
  const [data, setData] = useState<AccountTransaction[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const pageSize = 30

  const BUSINESS_TYPE_LABEL: Record<number, string> = {
    [BusinessType.TRANSFER]:   t('businessTypes.transfer'),
    [BusinessType.PAYMENT]:    t('businessTypes.payment'),
    [BusinessType.REFUND]:     t('businessTypes.refund'),
    [BusinessType.WITHDRAW]:   t('businessTypes.withdraw'),
    [BusinessType.DEPOSIT]:    t('businessTypes.deposit'),
    [BusinessType.COMMISSION]: t('businessTypes.commission'),
  }

  const BOOKING_TYPE_LABEL: Record<number, string> = {
    [BookingType.NORMAL_BOOKING]:   t('bookingTypes.normal'),
    [BookingType.BUFFER_BOOKING]:   t('bookingTypes.buffer'),
  }

  const fetchData = (p = 1) => {
    const values = form.getFieldsValue()
    const params: Record<string, unknown> = {
      page: p,
      page_size: pageSize,
    }

    if (values.account_no?.trim())    params.account_no    = values.account_no.trim()
    if (values.business_no?.trim())   params.business_no   = values.business_no.trim()
    if (values.transaction_id?.trim()) params.transaction_id = values.transaction_id.trim()
    if (values.dateRange) {
      params.start_date = values.dateRange[0].format('YYYY-MM-DD')
      params.end_date   = values.dateRange[1].format('YYYY-MM-DD')
    }

    setLoading(true)
    getTransactionList(params as Parameters<typeof getTransactionList>[0])
      .then((res) => {
        setData(res.list)
        setTotal(res.total)
        setPage(p)
      })
      .catch(() => {})
      .finally(() => setLoading(false))
  }

  const handleSearch = () => fetchData(1)

  const columns = [
    { title: t('columns.transactionId'), dataIndex: 'transaction_id', key: 'transaction_id', ellipsis: true },
    { title: t('columns.accountNo'), dataIndex: 'account_no', key: 'account_no' },
    { title: t('columns.businessNo'), dataIndex: 'business_no', key: 'business_no', ellipsis: true },
    {
      title: t('columns.businessType'),
      dataIndex: 'business_type',
      key: 'business_type',
      render: (tt: number) => BUSINESS_TYPE_LABEL[tt] ?? tt,
    },
    {
      title: t('columns.debitAmount'),
      dataIndex: 'debit_amount',
      key: 'debit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.creditAmount'),
      dataIndex: 'credit_amount',
      key: 'credit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.balanceBefore'),
      dataIndex: 'balance_before',
      key: 'balance_before',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.balanceAfter'),
      dataIndex: 'balance_after',
      key: 'balance_after',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.bookingType'),
      dataIndex: 'booking_type',
      key: 'booking_type',
      render: (tt: unknown) => BOOKING_TYPE_LABEL[tt as number] ?? tt,
    },
    { title: t('columns.transactionDate'), dataIndex: 'transaction_date', key: 'transaction_date' },
    {
      title: t('columns.transactionTime'),
      dataIndex: 'transaction_time',
      key: 'transaction_time',
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
  ]

  return (
    <div>
      <Form form={form} layout="inline" style={{ marginBottom: 16 }}>
        <Form.Item name="account_no">
          <Input
            placeholder={t('filters.accountNoPlaceholder')}
            prefix={<SearchOutlined />}
            style={{ width: 160 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="business_no">
          <Input
            placeholder={t('filters.businessNoPlaceholder')}
            style={{ width: 180 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="transaction_id">
          <Input
            placeholder={t('filters.transactionIdPlaceholder')}
            style={{ width: 180 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="dateRange">
          <RangePicker />
        </Form.Item>
        <Form.Item>
          <Space>
            <Button type="primary" onClick={handleSearch} loading={loading}>
              {t('common:actions.search')}
            </Button>
            <Button
              onClick={() => {
                form.resetFields()
                setData([])
                setTotal(0)
              }}
            >
              {t('common:actions.reset')}
            </Button>
          </Space>
        </Form.Item>
      </Form>

      <Table
        columns={columns}
        dataSource={data}
        rowKey="transaction_id"
        loading={loading}
        pagination={{
          current: page,
          pageSize,
          total,
          showTotal: (tt) => t('common:table.total', { count: tt }),
          onChange: (p) => fetchData(p),
        }}
      />
    </div>
  )
}
