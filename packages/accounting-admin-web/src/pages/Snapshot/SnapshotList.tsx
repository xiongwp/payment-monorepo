import { Table, Input, Button, Space, DatePicker, Form, Alert } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import { useState } from 'react'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import { getBalanceSnapshot } from '../../api/accounting'
import type { AccountBalanceSnapshot } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

export default function SnapshotList() {
  const { t } = useTranslation('snapshot')
  const [form] = Form.useForm<{ account_no: string; snapshot_date: dayjs.Dayjs }>()
  const [data, setData] = useState<AccountBalanceSnapshot[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const handleSearch = () => {
    const values = form.getFieldsValue()
    const accountNo = values.account_no?.trim()
    if (!accountNo) {
      setError(t('errors.accountNoRequired'))
      return
    }
    setError(null)
    setLoading(true)
    const date = values.snapshot_date ? values.snapshot_date.format('YYYY-MM-DD') : undefined
    getBalanceSnapshot(accountNo, date)
      .then((snap) => setData([snap]))
      .catch((err) => setError(err?.message ?? t('errors.queryFailed')))
      .finally(() => setLoading(false))
  }

  const columns = [
    { title: t('columns.accountNo'), dataIndex: 'account_no', key: 'account_no' },
    { title: t('columns.snapshotDate'), dataIndex: 'snapshot_date', key: 'snapshot_date' },
    {
      title: t('columns.beginningBalance'),
      dataIndex: 'beginning_balance',
      key: 'beginning_balance',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.endingBalance'),
      dataIndex: 'ending_balance',
      key: 'ending_balance',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.totalDebit'),
      dataIndex: 'total_debit',
      key: 'total_debit',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    {
      title: t('columns.totalCredit'),
      dataIndex: 'total_credit',
      key: 'total_credit',
      render: (v: string, row: AccountBalanceSnapshot) => displayMoney(v, row.currency),
    },
    { title: t('columns.transactionCount'), dataIndex: 'transaction_count', key: 'transaction_count' },
    { title: t('columns.currency'), dataIndex: 'currency', key: 'currency' },
  ]

  return (
    <div>
      <h2>{t('title')}</h2>

      <Form form={form} layout="inline" style={{ marginBottom: 16 }}>
        <Form.Item name="account_no">
          <Input
            placeholder={t('filters.accountNoPlaceholder')}
            prefix={<SearchOutlined />}
            style={{ width: 220 }}
            allowClear
          />
        </Form.Item>
        <Form.Item name="snapshot_date">
          <DatePicker placeholder={t('filters.snapshotDatePlaceholder')} />
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
                setError(null)
              }}
            >
              {t('common:actions.reset')}
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
