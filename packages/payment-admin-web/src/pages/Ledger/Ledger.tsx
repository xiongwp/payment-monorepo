import { useCallback, useEffect, useState } from 'react'
import {
  Card, Drawer, Input, Select, Space, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { useTranslation } from 'react-i18next'
import dayjs from 'dayjs'
import {
  listLedgerAccounts, listLedgerEntries, listLedgerTransactions, getLedgerTransaction,
} from '../../api'
import type { GLAccount, GLEntry, GLTransaction } from '../../api'
import { display } from '../../utils/money'

const TYPE_COLORS: Record<string, string> = {
  asset: 'green',
  liability: 'orange',
  revenue: 'blue',
  expense: 'red',
  equity: 'purple',
}

// fmtAmount renders a backend storage amount using the row's currency.
// storage = minor_units * 100; display() applies banker's rounding and the
// currency-specific symbol + precision.
function fmtAmount(storage: number | undefined | null, currency: string): string {
  if (storage == null) return '-'
  return display(storage, currency || 'PHP')
}

// Accounts tab — browse chart of accounts + balances.
function AccountsTab() {
  const { t } = useTranslation('ledger')
  const ownerLabel = (v: string): string => {
    const key = `ownerTypes.${v}`
    const translated = t(key)
    return translated === key ? v : translated
  }
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<GLAccount[]>([])
  const [filter, setFilter] = useState<{ owner_type?: string; owner_id?: string }>({})
  const [detail, setDetail] = useState<GLAccount | null>(null)
  const [entries, setEntries] = useState<GLEntry[]>([])

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listLedgerAccounts({ ...filter, limit: 200 })
      setRows(r.accounts || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [filter])

  useEffect(() => { load() }, [load])

  const openAccount = async (a: GLAccount) => {
    setDetail(a)
    try {
      const r = await listLedgerEntries({ account_id: a.id, limit: 100 })
      setEntries(r.entries || [])
    } catch (e) {
      message.error(String(e))
    }
  }

  return (
    <>
      <Space wrap style={{ marginBottom: 16 }}>
        <Select
          placeholder={t('accounts.filters.ownerType')} allowClear style={{ width: 140 }}
          onChange={(v) => setFilter((f) => ({ ...f, owner_type: v }))}
          options={[
            { value: 'platform', label: t('ownerTypes.platform') },
            { value: 'merchant', label: t('ownerTypes.merchant') },
            { value: 'channel', label: t('ownerTypes.channel') },
          ]}
        />
        <Input
          placeholder={t('accounts.filters.ownerIdPlaceholder')} allowClear
          style={{ width: 260 }}
          onPressEnter={(e) => setFilter((f) => ({ ...f, owner_id: (e.target as HTMLInputElement).value }))}
        />
      </Space>
      <Table<GLAccount>
        rowKey="id"
        size="small"
        loading={loading}
        dataSource={rows}
        pagination={{ pageSize: 30 }}
        columns={[
          { title: t('accounts.columns.id'), dataIndex: 'id', width: 320, render: (v: string) => <Typography.Text code>{v}</Typography.Text> },
          { title: t('accounts.columns.name'), dataIndex: 'name', ellipsis: true },
          { title: t('accounts.columns.type'), dataIndex: 'type', width: 110, render: (v: string) => <Tag color={TYPE_COLORS[v] || 'default'}>{v}</Tag> },
          { title: t('accounts.columns.ownerType'), dataIndex: 'owner_type', width: 80, render: (v: string) => ownerLabel(v) },
          { title: t('accounts.columns.owner'), dataIndex: 'owner_id', width: 140, ellipsis: true },
          { title: t('accounts.columns.debit'), dataIndex: 'debit_balance', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
          { title: t('accounts.columns.credit'), dataIndex: 'credit_balance', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
          {
            title: t('accounts.columns.net'), dataIndex: 'net_balance', width: 120, align: 'right',
            render: (v: number, r) => <Typography.Text strong>{fmtAmount(v, r.currency)}</Typography.Text>,
          },
          { title: '', width: 70, render: (_, r) => <a onClick={() => openAccount(r)}>{t('accounts.columns.detail')}</a> },
        ]}
      />
      <Drawer
        title={detail ? `${detail.name}` : ''}
        open={!!detail}
        onClose={() => setDetail(null)}
        width={860}
      >
        {detail && (
          <div>
            <Typography.Paragraph>
              <Typography.Text code copyable>{detail.id}</Typography.Text>
              <Tag color={TYPE_COLORS[detail.type]} style={{ marginLeft: 12 }}>{detail.type}</Tag>
            </Typography.Paragraph>
            <Typography.Paragraph>
              <Space size="large">
                <span>{t('accounts.drawer.debit')}: <Typography.Text strong>{fmtAmount(detail.debit_balance, detail.currency)}</Typography.Text></span>
                <span>{t('accounts.drawer.credit')}: <Typography.Text strong>{fmtAmount(detail.credit_balance, detail.currency)}</Typography.Text></span>
                <span>{t('accounts.drawer.net')}: <Typography.Text strong type="success">{fmtAmount(detail.net_balance, detail.currency)}</Typography.Text></span>
              </Space>
            </Typography.Paragraph>
            <Typography.Title level={5}>{t('accounts.drawer.recentEntries')}</Typography.Title>
            <Table<GLEntry>
              rowKey="id"
              size="small"
              dataSource={entries}
              pagination={false}
              columns={[
                { title: t('accounts.drawer.entryColumns.time'), dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
                { title: t('accounts.drawer.entryColumns.txn'), dataIndex: 'txn_id', width: 160, ellipsis: true },
                { title: t('accounts.drawer.entryColumns.debit'), dataIndex: 'debit_amount', width: 110, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: t('accounts.drawer.entryColumns.credit'), dataIndex: 'credit_amount', width: 110, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: t('accounts.drawer.entryColumns.memo'), dataIndex: 'memo', ellipsis: true },
              ]}
            />
          </div>
        )}
      </Drawer>
    </>
  )
}

// Transactions tab — browse gl_transaction with drill-into-entries.
function TransactionsTab() {
  const { t } = useTranslation('ledger')
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<GLTransaction[]>([])
  const [filter, setFilter] = useState<{ event_type?: string; ref_type?: string; ref_id?: string }>({})
  const [detail, setDetail] = useState<GLTransaction | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listLedgerTransactions({ ...filter, limit: 100 })
      setRows(r.transactions || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [filter])

  useEffect(() => { load() }, [load])

  const open = async (tx: GLTransaction) => {
    try {
      const full = await getLedgerTransaction(tx.id)
      setDetail(full)
    } catch (e) {
      message.error(String(e))
    }
  }

  // GLTransaction has no currency field; transactions are single-currency
  // at the entry level. Show totals with PHP default (all txns in this stack
  // are PHP) and fall back to raw storage if unknown.
  const txnCurrency = (_t: GLTransaction): string => 'PHP'

  return (
    <>
      <Space wrap style={{ marginBottom: 16 }}>
        <Select
          placeholder={t('transactions.filters.eventType')} allowClear style={{ width: 220 }}
          onChange={(v) => setFilter((f) => ({ ...f, event_type: v }))}
          options={[
            { value: 'charge.succeeded', label: 'charge.succeeded' },
            { value: 'refund.succeeded', label: 'refund.succeeded' },
            { value: 'settlement.payout', label: 'settlement.payout' },
            { value: 'settlement.received', label: 'settlement.received' },
            { value: 'adjustment.manual', label: 'adjustment.manual' },
          ]}
        />
        <Input placeholder={t('transactions.filters.refType')} allowClear style={{ width: 160 }}
          onPressEnter={(e) => setFilter((f) => ({ ...f, ref_type: (e.target as HTMLInputElement).value }))}
        />
        <Input placeholder={t('transactions.filters.refId')} allowClear style={{ width: 220 }}
          onPressEnter={(e) => setFilter((f) => ({ ...f, ref_id: (e.target as HTMLInputElement).value }))}
        />
      </Space>
      <Table<GLTransaction>
        rowKey="id"
        size="small"
        loading={loading}
        dataSource={rows}
        pagination={{ pageSize: 30 }}
        columns={[
          { title: t('transactions.columns.time'), dataIndex: 'created_ms', width: 170, render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss') },
          { title: t('transactions.columns.txn'), dataIndex: 'id', width: 180, ellipsis: true, render: (v) => <Typography.Text code>{v}</Typography.Text> },
          { title: t('transactions.columns.event'), dataIndex: 'event_type', width: 200, render: (v) => <Tag>{v}</Tag> },
          { title: t('transactions.columns.ref'), width: 240, render: (_, r) => r.ref_type ? `${r.ref_type} / ${r.ref_id}` : '-' },
          { title: t('transactions.columns.amount'), dataIndex: 'total_debit', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, txnCurrency(r)) },
          { title: t('transactions.columns.memo'), dataIndex: 'memo', ellipsis: true },
          { title: '', width: 70, render: (_, r) => <a onClick={() => open(r)}>{t('transactions.columns.detail')}</a> },
        ]}
      />
      <Drawer
        title={detail ? `${detail.event_type} · ${detail.id}` : ''}
        open={!!detail}
        onClose={() => setDetail(null)}
        width={860}
      >
        {detail && (
          <div>
            <Typography.Paragraph>
              <Space size="large">
                <span>{t('transactions.drawer.ref')}: {detail.ref_type} / <Typography.Text code>{detail.ref_id}</Typography.Text></span>
                <span>{t('transactions.drawer.amount')}: <Typography.Text strong>{fmtAmount(detail.total_debit, txnCurrency(detail))}</Typography.Text></span>
              </Space>
            </Typography.Paragraph>
            {detail.memo && <Typography.Paragraph type="secondary">{detail.memo}</Typography.Paragraph>}
            <Typography.Title level={5}>{t('transactions.drawer.entries')}</Typography.Title>
            <Table<GLEntry>
              rowKey="id"
              size="small"
              dataSource={detail.entries || []}
              pagination={false}
              columns={[
                { title: t('transactions.drawer.entryColumns.account'), dataIndex: 'account_id', ellipsis: true, render: (v) => <Typography.Text code>{v}</Typography.Text> },
                { title: t('transactions.drawer.entryColumns.debit'), dataIndex: 'debit_amount', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: t('transactions.drawer.entryColumns.credit'), dataIndex: 'credit_amount', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: t('transactions.drawer.entryColumns.memo'), dataIndex: 'memo', ellipsis: true },
              ]}
            />
          </div>
        )}
      </Drawer>
    </>
  )
}

export default function LedgerPage() {
  const { t } = useTranslation('ledger')
  return (
    <div>
      <Typography.Title level={3}>
        {t('title')}
        <Typography.Text type="secondary" style={{ fontSize: 14, marginLeft: 12 }}>
          {t('subtitle')}
        </Typography.Text>
      </Typography.Title>
      <Card>
        <Tabs
          items={[
            { key: 'accounts', label: t('tabs.accounts'), children: <AccountsTab /> },
            { key: 'txns', label: t('tabs.transactions'), children: <TransactionsTab /> },
          ]}
        />
      </Card>
    </div>
  )
}
