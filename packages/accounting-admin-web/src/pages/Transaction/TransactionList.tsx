import { Table, Input, Button, Space, DatePicker, Form, Tag } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import { useState } from 'react'
import dayjs from 'dayjs'
import { getTransactionList } from '../../api/accounting'
import type { AccountTransaction } from '../../types/accounting'
import { BookingType, BusinessType } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const { RangePicker } = DatePicker

const BUSINESS_TYPE_LABEL: Record<number, string> = {
  [BusinessType.TRANSFER]:   '转账',
  [BusinessType.PAYMENT]:    '支付',
  [BusinessType.REFUND]:     '退款',
  [BusinessType.WITHDRAW]:   '提现',
  [BusinessType.DEPOSIT]:    '充值',
  [BusinessType.COMMISSION]: '手续费',
}

const BOOKING_TYPE_LABEL: Record<number, string> = {
  [BookingType.NORMAL_BOOKING]:   '实时记账',
  [BookingType.BUFFER_BOOKING]:    '缓冲记账',
}

interface SearchValues {
  account_no?: string;
  business_no?: string;
  transaction_id?: string;
  dateRange?: [dayjs.Dayjs, dayjs.Dayjs];
}

export default function TransactionList() {
  const [form] = Form.useForm<SearchValues>()
  const [data, setData] = useState<AccountTransaction[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const pageSize = 30

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
    { title: '交易ID', dataIndex: 'transaction_id', key: 'transaction_id', ellipsis: true },
    { title: '账户号', dataIndex: 'account_no', key: 'account_no' },
    { title: '业务订单号', dataIndex: 'business_no', key: 'business_no', ellipsis: true },
    {
      title: '业务类型',
      dataIndex: 'business_type',
      key: 'business_type',
      render: (t: number) => BUSINESS_TYPE_LABEL[t] ?? t,
    },
    {
      title: '借方金额',
      dataIndex: 'debit_amount',
      key: 'debit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
    },
    {
      title: '贷方金额',
      dataIndex: 'credit_amount',
      key: 'credit_amount',
      render: (v: string, row: AccountTransaction) => displayMoney(v, row.currency),
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
    {
      title: '记账类型',
      dataIndex: 'booking_type',
      key: 'booking_type',
      render: (t: unknown) => BOOKING_TYPE_LABEL[t as number] ?? t,
    },
    { title: '交易日期', dataIndex: 'transaction_date', key: 'transaction_date' },
    {
      title: '交易时间',
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
            placeholder="账户号"
            prefix={<SearchOutlined />}
            style={{ width: 160 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="business_no">
          <Input
            placeholder="业务订单号"
            style={{ width: 180 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="transaction_id">
          <Input
            placeholder="交易ID"
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
              查询
            </Button>
            <Button
              onClick={() => {
                form.resetFields()
                setData([])
                setTotal(0)
              }}
            >
              重置
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
          showTotal: (t) => `共 ${t} 条`,
          onChange: (p) => fetchData(p),
        }}
      />
    </div>
  )
}
