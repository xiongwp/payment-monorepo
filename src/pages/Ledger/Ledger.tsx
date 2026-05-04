import { useCallback, useEffect, useState } from 'react'
import {
  Card, Drawer, Input, Select, Space, Table, Tabs, Tag, Typography, message,
} from 'antd'
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

const OWNER_LABELS: Record<string, string> = {
  platform: '平台',
  merchant: '商户',
  channel: '渠道',
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
          placeholder="账户维度" allowClear style={{ width: 140 }}
          onChange={(v) => setFilter((f) => ({ ...f, owner_type: v }))}
          options={[
            { value: 'platform', label: '平台' },
            { value: 'merchant', label: '商户' },
            { value: 'channel', label: '渠道' },
          ]}
        />
        <Input
          placeholder="owner_id (mch_xxx / gcash)" allowClear
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
          { title: 'ID', dataIndex: 'id', width: 320, render: (v: string) => <Typography.Text code>{v}</Typography.Text> },
          { title: '名称', dataIndex: 'name', ellipsis: true },
          { title: '类型', dataIndex: 'type', width: 110, render: (v: string) => <Tag color={TYPE_COLORS[v] || 'default'}>{v}</Tag> },
          { title: '维度', dataIndex: 'owner_type', width: 80, render: (v: string) => OWNER_LABELS[v] || v },
          { title: 'owner', dataIndex: 'owner_id', width: 140, ellipsis: true },
          { title: 'Dr', dataIndex: 'debit_balance', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
          { title: 'Cr', dataIndex: 'credit_balance', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
          {
            title: 'Net', dataIndex: 'net_balance', width: 120, align: 'right',
            render: (v: number, r) => <Typography.Text strong>{fmtAmount(v, r.currency)}</Typography.Text>,
          },
          { title: '', width: 70, render: (_, r) => <a onClick={() => openAccount(r)}>明细</a> },
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
                <span>Debit: <Typography.Text strong>{fmtAmount(detail.debit_balance, detail.currency)}</Typography.Text></span>
                <span>Credit: <Typography.Text strong>{fmtAmount(detail.credit_balance, detail.currency)}</Typography.Text></span>
                <span>Net: <Typography.Text strong type="success">{fmtAmount(detail.net_balance, detail.currency)}</Typography.Text></span>
              </Space>
            </Typography.Paragraph>
            <Typography.Title level={5}>近 100 条条目</Typography.Title>
            <Table<GLEntry>
              rowKey="id"
              size="small"
              dataSource={entries}
              pagination={false}
              columns={[
                { title: '时间', dataIndex: 'created_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
                { title: 'txn', dataIndex: 'txn_id', width: 160, ellipsis: true },
                { title: 'Dr', dataIndex: 'debit_amount', width: 110, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: 'Cr', dataIndex: 'credit_amount', width: 110, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: 'Memo', dataIndex: 'memo', ellipsis: true },
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

  const open = async (t: GLTransaction) => {
    try {
      const full = await getLedgerTransaction(t.id)
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
          placeholder="事件类型" allowClear style={{ width: 220 }}
          onChange={(v) => setFilter((f) => ({ ...f, event_type: v }))}
          options={[
            { value: 'charge.succeeded', label: 'charge.succeeded' },
            { value: 'refund.succeeded', label: 'refund.succeeded' },
            { value: 'settlement.payout', label: 'settlement.payout' },
            { value: 'settlement.received', label: 'settlement.received' },
            { value: 'adjustment.manual', label: 'adjustment.manual' },
          ]}
        />
        <Input placeholder="ref_type" allowClear style={{ width: 160 }}
          onPressEnter={(e) => setFilter((f) => ({ ...f, ref_type: (e.target as HTMLInputElement).value }))}
        />
        <Input placeholder="ref_id" allowClear style={{ width: 220 }}
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
          { title: '时间', dataIndex: 'created_ms', width: 170, render: (v) => dayjs(v).format('YYYY-MM-DD HH:mm:ss') },
          { title: 'txn', dataIndex: 'id', width: 180, ellipsis: true, render: (v) => <Typography.Text code>{v}</Typography.Text> },
          { title: 'Event', dataIndex: 'event_type', width: 200, render: (v) => <Tag>{v}</Tag> },
          { title: 'Ref', width: 240, render: (_, r) => r.ref_type ? `${r.ref_type} / ${r.ref_id}` : '-' },
          { title: '金额', dataIndex: 'total_debit', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, txnCurrency(r)) },
          { title: 'Memo', dataIndex: 'memo', ellipsis: true },
          { title: '', width: 70, render: (_, r) => <a onClick={() => open(r)}>明细</a> },
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
                <span>Ref: {detail.ref_type} / <Typography.Text code>{detail.ref_id}</Typography.Text></span>
                <span>金额: <Typography.Text strong>{fmtAmount(detail.total_debit, txnCurrency(detail))}</Typography.Text></span>
              </Space>
            </Typography.Paragraph>
            {detail.memo && <Typography.Paragraph type="secondary">{detail.memo}</Typography.Paragraph>}
            <Typography.Title level={5}>Entries</Typography.Title>
            <Table<GLEntry>
              rowKey="id"
              size="small"
              dataSource={detail.entries || []}
              pagination={false}
              columns={[
                { title: '账户', dataIndex: 'account_id', ellipsis: true, render: (v) => <Typography.Text code>{v}</Typography.Text> },
                { title: 'Dr', dataIndex: 'debit_amount', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: 'Cr', dataIndex: 'credit_amount', width: 120, align: 'right', render: (v: number, r) => fmtAmount(v, r.currency) },
                { title: 'Memo', dataIndex: 'memo', ellipsis: true },
              ]}
            />
          </div>
        )}
      </Drawer>
    </>
  )
}

export default function LedgerPage() {
  return (
    <div>
      <Typography.Title level={3}>
        总账 (Ledger)
        <Typography.Text type="secondary" style={{ fontSize: 14, marginLeft: 12 }}>
          双账记账 · 金额按后端 storage (minor × 100) 存储，展示时按币种精度格式化
        </Typography.Text>
      </Typography.Title>
      <Card>
        <Tabs
          items={[
            { key: 'accounts', label: '账户', children: <AccountsTab /> },
            { key: 'txns', label: '凭证', children: <TransactionsTab /> },
          ]}
        />
      </Card>
    </div>
  )
}
