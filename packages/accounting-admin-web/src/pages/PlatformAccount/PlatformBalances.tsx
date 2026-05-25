import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('account')
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
      message.warning(t('balances.messages.needBusinessType'))
      return
    }
    if (!currency) {
      message.warning(t('balances.messages.needCurrency'))
      return
    }
    setLoading(true)
    try {
      const resp = await getPlatformBalances(businessType, currency)
      setAccounts(resp.accounts || [])
      if (resp.count < 100) {
        message.info(t('balances.messages.partialFleet', { count: resp.count }))
      }
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('balances.messages.queryFailed'))
      setAccounts([])
    } finally {
      setLoading(false)
    }
  }

  const columns: ColumnsType<Account> = [
    { title: t('balances.columns.userIdShard'), dataIndex: 'user_id', key: 'user_id', width: 110 },
    { title: t('balances.columns.accountNo'), dataIndex: 'account_no', key: 'account_no', width: 220, ellipsis: true },
    {
      title: t('balances.columns.group'),
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
      title: t('balances.columns.balance'), dataIndex: 'balance', key: 'balance', width: 160, align: 'right',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: t('balances.columns.available'), dataIndex: 'available_balance', key: 'available_balance', width: 160, align: 'right',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: t('balances.columns.frozen'), dataIndex: 'frozen_balance', key: 'frozen_balance', width: 140, align: 'right',
      render: (v: string, row: Account) => displayMoney(v, row.currency),
    },
    {
      title: t('balances.columns.status'), dataIndex: 'status', key: 'status', width: 90,
      render: (s: number) => s === 1 ? <Tag color="green">{t('statusLabels.active')}</Tag> : <Tag color="red">{t('statusLabels.abnormal')}</Tag>,
    },
  ]

  return (
    <div>
      <Title level={3}>{t('balances.pageTitle')}</Title>
      <Paragraph type="secondary">
        {t('balances.pageDescriptionPrefix')}<Text code>business_type</Text>{t('balances.pageDescriptionSuffix')}
      </Paragraph>

      <Card style={{ marginBottom: 16 }}>
        <Form layout="inline" onFinish={onQuery}>
          <Form.Item label={t('balances.form.businessTypeLabel')}>
            <Select
              placeholder={t('balances.form.businessTypePlaceholder')}
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
          <Form.Item label={t('balances.form.currencyLabel')}>
            <Select
              value={currency}
              onChange={setCurrency}
              style={{ width: 120 }}
              showSearch
              options={['PHP','USD','CNY','EUR','JPY','HKD','SGD','THB','IDR','MYR','VND','KRW'].map((c) => ({ value: c, label: c }))}
            />
          </Form.Item>
          <Form.Item>
            <Button type="primary" icon={<SearchOutlined />} htmlType="submit" loading={loading}>{t('balances.form.queryButton')}</Button>
          </Form.Item>
        </Form>
      </Card>

      {selectedRow && (
        <Row gutter={16} style={{ marginBottom: 16 }}>
          <Col span={6}><Card><Statistic title={t('balances.stats.businessType')} value={selectedRow.business_type} /></Card></Col>
          <Col span={6}><Card><Statistic title={t('balances.stats.code')} value={selectedRow.business_type_code} /></Card></Col>
          <Col span={6}><Card><Statistic title={t('balances.stats.category')} value={CATEGORY_LABEL[CATEGORY_BY_ACCOUNT_TYPE[selectedRow.account_type]] ?? '-'} /></Card></Col>
          <Col span={6}><Card><Statistic title={t('balances.stats.foundAccounts')} value={accounts.length} suffix={t('balances.stats.foundAccountsSuffix')} /></Card></Col>
        </Row>
      )}
      {accounts.length > 0 && (
        <Card style={{ marginBottom: 16 }}>
          <Statistic title={t('balances.stats.totalBalance')} value={formatMinorBigInt(totalBalanceMinor, totalCurrency)} />
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
