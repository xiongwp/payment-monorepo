import { useEffect, useMemo, useState } from 'react'
import { Card, Form, Select, Button, Table, Typography, message, Statistic, Row, Col, Tag } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { Account, BusinessTypeInfo } from '../../types/accounting'
import {
  getPlatformBalances,
  listBusinessTypes,
  CATEGORY_BY_ACCOUNT_TYPE,
  CATEGORY_LABEL,
  isPlatformAccountType,
} from '../../api/accounting'
import { display as displayMoney, toMinorBigInt, formatMinorBigInt } from '../../utils/money'

const { Title, Paragraph, Text } = Typography

// Platform accounts may mix currencies across fleet shards in theory, but in
// practice one business_type = one currency. When computing the sum we pick
// the first account's currency for the total formatting; individual rows use
// their own currency column. Fallback to PHP when the list is empty.
const DEFAULT_CURRENCY = 'PHP'

export default function PlatformBalances() {
  const [registry, setRegistry] = useState<BusinessTypeInfo[]>([])
  const [loading, setLoading] = useState(false)
  const [businessType, setBusinessType] = useState<number | undefined>()
  const [currency, setCurrency] = useState<string>('PHP')
  const [accounts, setAccounts] = useState<Account[]>([])

  useEffect(() => {
    // 只保留平台内部账户类型对应的 business_type（用户 / 商户账户没有"每渠道 100 个"结构，
    // 它们在账户管理页按 account_no 查）。
    listBusinessTypes()
      .then((rows) => setRegistry(rows.filter(r => isPlatformAccountType(r.account_type))))
      .catch(() => setRegistry([]))
  }, [])

  const selectedRow = useMemo(
    () => registry.find((r) => r.business_type === businessType),
    [registry, businessType],
  )

  // Sum in exact minor units. toMinorBigInt normalises both representations
  // the backend may return: legacy int64 storage (/v1/platform-accounts/balances
  // HTTP proxy) and gRPC-formatted "200.00" decimal strings.
  const { totalBalanceMinor, totalCurrency } = useMemo(() => {
    let sum = 0n
    let currency = DEFAULT_CURRENCY
    for (const a of accounts) {
      const cur = a.currency || currency
      currency = cur
      try {
        sum += toMinorBigInt(a.balance || '0', cur)
      } catch {
        // skip unparseable entries rather than crash the render
      }
    }
    return { totalBalanceMinor: sum, totalCurrency: currency }
  }, [accounts])

  const onQuery = async () => {
    if (!businessType) {
      message.warning('请选择 business_type')
      return
    }
    if (!currency) {
      message.warning('请选择币种')
      return
    }
    setLoading(true)
    try {
      const resp = await getPlatformBalances(businessType, currency)
      setAccounts(resp.accounts || [])
      if (resp.count < 100) {
        message.info(`已找到 ${resp.count} 个账户（少于 100 个，可能尚未完成 fleet 注册）`)
      }
    } catch (e) {
      message.error(e instanceof Error ? e.message : '查询失败')
      setAccounts([])
    } finally {
      setLoading(false)
    }
  }

  const columns: ColumnsType<Account> = [
    { title: 'User ID (分片)', dataIndex: 'user_id', key: 'user_id', width: 110 },
    { title: '账户号', dataIndex: 'account_no', key: 'account_no', width: 220, ellipsis: true },
    {
      title: 'Group',
      dataIndex: 'account_group',
      key: 'account_group',
      width: 80,
      filters: [
        { text: 'A', value: 'A' },
        { text: 'B', value: 'B' },
      ],
      onFilter: (val, row) => row.account_group === val,
      render: (g: string) => {
        if (g === 'B') return <Tag color="orange">B</Tag>
        return <Tag color="blue">A</Tag> // 空字符串也按 A 显示（兼容老数据）
      },
    },
    {
      title: 'Balance', dataIndex: 'balance', key: 'balance', width: 160, align: 'right',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: 'Available', dataIndex: 'available_balance', key: 'available_balance', width: 160, align: 'right',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: 'Frozen', dataIndex: 'frozen_balance', key: 'frozen_balance', width: 140, align: 'right',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: '状态', dataIndex: 'status', key: 'status', width: 90,
      render: (s: number) => s === 1 ? <Tag color="green">正常</Tag> : <Tag color="red">异常</Tag>,
    },
  ]

  return (
    <div>
      <Title level={3}>平台账户余额查询</Title>
      <Paragraph type="secondary">
        按 <Text code>business_type</Text> 查询对应的 100 个平台账户（user_id = 0..99，每分片一个）的当前余额。
        余额可能含 Redis/buffer 未刷入的增量 —— 以 account.balance 为准，不一定等于 snapshot。
      </Paragraph>

      <Card style={{ marginBottom: 16 }}>
        <Form layout="inline" onFinish={onQuery}>
          <Form.Item label="Business Type">
            <Select
              placeholder="选择已注册的业务类型"
              style={{ width: 360 }}
              value={businessType}
              onChange={setBusinessType}
              showSearch
              optionFilterProp="label"
              options={registry.map((r) => ({
                value: r.business_type,
                label: `${r.business_type} · ${r.business_type_code}`,
              }))}
            />
          </Form.Item>
          <Form.Item label="币种">
            <Select
              value={currency}
              onChange={setCurrency}
              style={{ width: 120 }}
              showSearch
              options={['PHP','USD','CNY','EUR','JPY','HKD','SGD','THB','IDR','MYR','VND','KRW'].map((c) => ({ value: c, label: c }))}
            />
          </Form.Item>
          <Form.Item>
            <Button type="primary" icon={<SearchOutlined />} htmlType="submit" loading={loading}>查询</Button>
          </Form.Item>
        </Form>
      </Card>

      {selectedRow && (
        <Row gutter={16} style={{ marginBottom: 16 }}>
          <Col span={6}><Card><Statistic title="Business Type" value={selectedRow.business_type} /></Card></Col>
          <Col span={6}><Card><Statistic title="Code" value={selectedRow.business_type_code} /></Card></Col>
          <Col span={6}><Card><Statistic title="Category (derived)" value={CATEGORY_LABEL[CATEGORY_BY_ACCOUNT_TYPE[selectedRow.account_type]] ?? '-'} /></Card></Col>
          <Col span={6}><Card><Statistic title="找到账户数" value={accounts.length} suffix="/ 100" /></Card></Col>
        </Row>
      )}
      {accounts.length > 0 && (
        <Card style={{ marginBottom: 16 }}>
          <Statistic title="所有账户余额汇总 (balance 之和)" value={formatMinorBigInt(totalBalanceMinor, totalCurrency)} />
        </Card>
      )}

      <Table
        rowKey="account_no"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={accounts}
        pagination={{ pageSize: 25 }}
        scroll={{ y: 520 }}
      />
    </div>
  )
}
