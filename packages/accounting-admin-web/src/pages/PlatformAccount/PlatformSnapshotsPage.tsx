import { useEffect, useMemo, useState } from 'react'
import { Card, Form, Select, DatePicker, Button, Table, Typography, message, Tag, Space } from 'antd'
import { SearchOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { BusinessTypeInfo, PlatformAccountSnapshotRow } from '../../types/accounting'
import { getPlatformSnapshots, listBusinessTypes, isPlatformAccountType } from '../../api/accounting'
import { display as displayMoney, toMinorBigInt, formatMinorBigInt } from '../../utils/money'
import dayjs, { Dayjs } from 'dayjs'

const { Title, Paragraph, Text } = Typography

const DEFAULT_CURRENCY = 'PHP'

export default function PlatformSnapshotsPage() {
  const [registry, setRegistry] = useState<BusinessTypeInfo[]>([])
  const [loading, setLoading] = useState(false)
  const [businessType, setBusinessType] = useState<number | undefined>()
  const [currency, setCurrency] = useState<string>('PHP')
  const [date, setDate] = useState<Dayjs | null>(dayjs())
  const [rows, setRows] = useState<PlatformAccountSnapshotRow[]>([])

  useEffect(() => {
    // 过滤：只查平台内部账户的 business_type（is_platform=1 对应的 account_type）
    listBusinessTypes()
      .then((rows) => setRegistry(rows.filter(r => isPlatformAccountType(r.account_type))))
      .catch(() => setRegistry([]))
  }, [])

  const onQuery = async () => {
    if (!businessType || !date) {
      message.warning('请选择 business_type 和日期')
      return
    }
    if (!currency) {
      message.warning('请选择币种')
      return
    }
    setLoading(true)
    try {
      const resp = await getPlatformSnapshots(businessType, date.format('YYYY-MM-DD'), currency)
      setRows(resp.rows || [])
    } catch (e) {
      message.error(e instanceof Error ? e.message : '查询失败')
      setRows([])
    } finally {
      setLoading(false)
    }
  }

  const stats = useMemo(() => {
    // Sum exact minor units via BigInt, then format once. toMinorBigInt normalises
    // both legacy int64 storage strings and the gRPC-formatted "200.00" decimals
    // the backend now hands us. Single-currency assumption (one business_type =
    // one currency) still holds — we just scale based on the row's currency.
    let totalBegin = 0n, totalEnd = 0n, totalDebit = 0n, totalCredit = 0n, hasSnapCount = 0
    let currency = DEFAULT_CURRENCY
    for (const r of rows) {
      const cur = r.snapshot?.currency || r.account?.currency || currency
      currency = cur
      if (r.snapshot) {
        hasSnapCount += 1
        try { totalBegin += toMinorBigInt(r.snapshot.beginning_balance || '0', cur) } catch { /* skip */ }
        try { totalEnd += toMinorBigInt(r.snapshot.ending_balance || '0', cur) } catch { /* skip */ }
        try { totalDebit += toMinorBigInt(r.snapshot.total_debit || '0', cur) } catch { /* skip */ }
        try { totalCredit += toMinorBigInt(r.snapshot.total_credit || '0', cur) } catch { /* skip */ }
      }
    }
    return { totalBegin, totalEnd, totalDebit, totalCredit, hasSnapCount, currency }
  }, [rows])

  const columns: ColumnsType<PlatformAccountSnapshotRow> = [
    { title: 'User ID', dataIndex: ['account', 'user_id'], key: 'user_id', width: 90 },
    { title: '账户号', dataIndex: ['account', 'account_no'], key: 'account_no', width: 220, ellipsis: true },
    {
      title: 'Group',
      dataIndex: ['account', 'account_group'],
      key: 'account_group',
      width: 75,
      filters: [
        { text: 'A', value: 'A' },
        { text: 'B', value: 'B' },
      ],
      onFilter: (val, r) => r.account.account_group === val,
      render: (g: string) => g === 'B' ? <Tag color="orange">B</Tag> : <Tag color="blue">A</Tag>,
    },
    {
      title: '期初余额', key: 'beginning',
      render: (_, r) => r.snapshot ? displayMoney(r.snapshot.beginning_balance, r.snapshot.currency || r.account.currency) : <Tag color="default">无快照</Tag>,
      width: 140, align: 'right',
    },
    {
      title: '期末余额', key: 'ending',
      render: (_, r) => r.snapshot ? displayMoney(r.snapshot.ending_balance, r.snapshot.currency || r.account.currency) : '-',
      width: 140, align: 'right',
    },
    {
      title: '借方合计', key: 'debit',
      render: (_, r) => r.snapshot ? displayMoney(r.snapshot.total_debit, r.snapshot.currency || r.account.currency) : '-',
      width: 130, align: 'right',
    },
    {
      title: '贷方合计', key: 'credit',
      render: (_, r) => r.snapshot ? displayMoney(r.snapshot.total_credit, r.snapshot.currency || r.account.currency) : '-',
      width: 130, align: 'right',
    },
    {
      title: '交易笔数', key: 'count',
      render: (_, r) => r.snapshot?.transaction_count ?? '-',
      width: 100, align: 'right',
    },
    {
      title: '当前余额 (对账)', dataIndex: ['account', 'balance'], key: 'live_balance',
      width: 150, align: 'right',
      render: (v: string, r) => {
        const formatted = displayMoney(v, r.account.currency)
        if (!r.snapshot) return formatted
        // Compare in exact minor units so mixed storage-int / decimal-major
        // row shapes from the two different backends (HTTP proxy vs gRPC)
        // still diff correctly.
        const cur = r.snapshot.currency || r.account.currency || DEFAULT_CURRENCY
        let diff = 0n
        try { diff = toMinorBigInt(v || '0', cur) - toMinorBigInt(r.snapshot.ending_balance || '0', cur) } catch { /* treat as zero */ }
        if (diff === 0n) return <span style={{ color: '#3f8600' }}>{formatted}</span>
        return <span style={{ color: '#cf1322' }} title={`与期末差 ${diff.toString()} minor`}>{formatted}</span>
      },
    },
  ]

  return (
    <div>
      <Title level={3}>平台账户快照查询</Title>
      <Paragraph type="secondary">
        按 <Text code>business_type + 日期</Text> 查询该渠道 100 个平台账户的日切快照。
        右侧"当前余额"与"期末余额"对账：绿色一致、红色不一致（可能已有新增流水或未 flush）。
      </Paragraph>

      <Card style={{ marginBottom: 16 }}>
        <Form layout="inline" onFinish={onQuery}>
          <Form.Item label="Business Type">
            <Select
              placeholder="选择业务类型"
              style={{ width: 320 }}
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
          <Form.Item label="日切日期">
            <DatePicker value={date} onChange={setDate} allowClear={false} />
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

      {rows.length > 0 && (
        <Card style={{ marginBottom: 16 }}>
          <Space>
            <Tag>有快照账户: {stats.hasSnapCount} / {rows.length}</Tag>
            <Tag color="blue">Σ 期初 {formatMinorBigInt(stats.totalBegin, stats.currency)}</Tag>
            <Tag color="blue">Σ 期末 {formatMinorBigInt(stats.totalEnd, stats.currency)}</Tag>
            <Tag color="orange">Σ 借方 {formatMinorBigInt(stats.totalDebit, stats.currency)}</Tag>
            <Tag color="green">Σ 贷方 {formatMinorBigInt(stats.totalCredit, stats.currency)}</Tag>
          </Space>
        </Card>
      )}

      <Table
        rowKey={(r) => r.account.account_no}
        size="small"
        loading={loading}
        columns={columns}
        dataSource={rows}
        pagination={{ pageSize: 25 }}
        scroll={{ y: 520 }}
      />
    </div>
  )
}

