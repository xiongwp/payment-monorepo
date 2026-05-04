import { Table, Input, Button, Space, DatePicker, Form, Alert } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import { useState } from 'react'
import dayjs from 'dayjs'
import { getBalanceSnapshot } from '../../api/accounting'
import type { AccountBalanceSnapshot } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

export default function SnapshotList() {
  const [form] = Form.useForm<{ account_no: string; snapshot_date: dayjs.Dayjs }>()
  const [data, setData] = useState<AccountBalanceSnapshot[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const handleSearch = () => {
    const values = form.getFieldsValue()
    const accountNo = values.account_no?.trim()
    if (!accountNo) {
      setError('请输入账户号')
      return
    }
    setError(null)
    setLoading(true)
    const date = values.snapshot_date ? values.snapshot_date.format('YYYY-MM-DD') : undefined
    getBalanceSnapshot(accountNo, date)
      .then((snap) => setData([snap]))
      .catch((err) => setError(err?.message ?? '查询失败'))
      .finally(() => setLoading(false))
  }

  const columns = [
    { title: '账户号', dataIndex: 'account_no', key: 'account_no' },
    { title: '快照日期', dataIndex: 'snapshot_date', key: 'snapshot_date' },
    {
      title: '期初余额',
      dataIndex: 'beginning_balance',
      key: 'beginning_balance',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    {
      title: '期末余额',
      dataIndex: 'ending_balance',
      key: 'ending_balance',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    {
      title: '借方总额',
      dataIndex: 'total_debit',
      key: 'total_debit',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    {
      title: '贷方总额',
      dataIndex: 'total_credit',
      key: 'total_credit',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    { title: '交易笔数', dataIndex: 'transaction_count', key: 'transaction_count' },
    { title: '币种', dataIndex: 'currency', key: 'currency' },
  ]

  return (
    <div>
      <h2>余额快照</h2>

      <Form form={form} layout="inline" style={{ marginBottom: 16 }}>
        <Form.Item name="account_no">
          <Input
            placeholder="账户号"
            prefix={<SearchOutlined />}
            style={{ width: 220 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="snapshot_date">
          <DatePicker placeholder="快照日期（默认最新）" />
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
                setError(null)
              }}
            >
              重置
            </Button>
          </Space>
        </Form.Item>
      </Form>

      {error && <Alert type="error" message={error} style={{ marginBottom: 16 }} />}

      <Table
        columns={columns}
        dataSource={data}
        rowKey={(r) => `${r.account_no}-${r.snapshot_date}`}
        loading={loading}
        pagination={false}
      />
    </div>
  )
}
