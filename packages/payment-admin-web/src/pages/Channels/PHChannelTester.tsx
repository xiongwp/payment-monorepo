import { useMemo, useState } from 'react'
import {
  Alert, Button, Card, Descriptions, Form, Progress, Select, Space, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { useTranslation } from 'react-i18next'
import { probeRoute } from '../../api'
import type { ProbeRouteResponse } from '../../api/types'

// PHChannelTester drives payment-core's router end-to-end against the
// payment-channel mockserver. Single-shot and batch modes both use the same
// probeRoute backend call — only the driver loop differs.

const SCENARIOS: Array<{ id: string; amount: number; expect: 'succeeded' | 'requires_action' | 'failed' | 'processing' }> = [
  { id: 's1', amount: 1, expect: 'succeeded' },
  { id: 's2', amount: 2, expect: 'requires_action' },
  { id: 's3', amount: 3, expect: 'failed' },
  { id: 's4', amount: 4, expect: 'failed' },
  { id: 's5', amount: 5, expect: 'failed' },
  { id: 's6', amount: 6, expect: 'failed' },
  { id: 's7', amount: 7, expect: 'processing' },
  { id: 's8', amount: 8, expect: 'processing' },
  { id: 's9', amount: 9, expect: 'failed' },
]

const CHANNELS: Array<{ value: string; label: string; paymentMethod: string }> = [
  { value: 'gcash',     label: 'GCash',     paymentMethod: 'gcash' },
  { value: 'maya',      label: 'Maya',      paymentMethod: 'maya' },
  { value: 'grabpay',   label: 'GrabPay',   paymentMethod: 'grabpay' },
  { value: 'paymongo',  label: 'PayMongo',  paymentMethod: 'paymongo' },
  { value: 'xendit',    label: 'Xendit',    paymentMethod: 'xendit' },
  { value: 'dragonpay', label: 'Dragonpay', paymentMethod: 'dragonpay' },
  { value: 'shopeepay', label: 'ShopeePay', paymentMethod: 'shopeepay' },
  { value: 'billease',  label: 'BillEase',  paymentMethod: 'billease' },
  { value: 'coinsph',   label: 'Coins.ph',  paymentMethod: 'coinsph' },
  { value: 'instapay',  label: 'InstaPay',  paymentMethod: 'instapay' },
  { value: 'pesonet',   label: 'PESONet',   paymentMethod: 'pesonet' },
  { value: 'bdo',       label: 'BDO',       paymentMethod: 'bdo' },
  { value: 'bpi',       label: 'BPI',       paymentMethod: 'bpi' },
  { value: 'metrobank', label: 'Metrobank', paymentMethod: 'metrobank' },
  { value: 'landbank',  label: 'LandBank',  paymentMethod: 'landbank' },
]

interface Cell {
  channel: string
  amount: number
  status: 'idle' | 'running' | 'pass' | 'mismatch' | 'error'
  response?: ProbeRouteResponse
  error?: string
  durationMs?: number
}

export default function PHChannelTester() {
  const { t } = useTranslation('channel')
  return (
    <div>
      <Typography.Title level={3}>{t('phTester.title')}</Typography.Title>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('phTester.intro')}
      />
      <Tabs
        items={[
          { key: 'single', label: t('phTester.tabs.single'), children: <SingleTest /> },
          { key: 'matrix', label: t('phTester.tabs.matrix'), children: <MatrixTest /> },
        ]}
      />
    </div>
  )
}

// ─── Single test tab (existing form) ────────────────────────────────────────

function SingleTest() {
  const { t } = useTranslation('channel')
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<ProbeRouteResponse | null>(null)
  const [amount, setAmount] = useState<number>(1)

  const scenarioInfo = useMemo(() => SCENARIOS.find((s) => s.amount === amount), [amount])

  const onRun = async (v: { channel: string; amount: number }) => {
    const ch = CHANNELS.find((c) => c.value === v.channel)
    if (!ch) return
    setLoading(true)
    setResult(null)
    try {
      const r = await probeRoute({
        country: 'PH', payment_method: ch.paymentMethod, amount: v.amount, currency: 'PHP',
      })
      setResult(r)
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <>
      <Card>
        <Form form={form} layout="vertical" initialValues={{ channel: 'gcash', amount: 1 }} onFinish={onRun}>
          <Space wrap size="large">
            <Form.Item name="channel" label={t('phTester.form.channel')} rules={[{ required: true }]}>
              <Select style={{ width: 240 }} options={CHANNELS.map((c) => ({ value: c.value, label: c.label }))} />
            </Form.Item>
            <Form.Item name="amount" label={t('phTester.form.scenario')} rules={[{ required: true }]}>
              <Select style={{ width: 320 }}
                options={SCENARIOS.map((s) => ({ value: s.amount, label: t(`phTester.scenarios.${s.id}.label`) }))}
                onChange={(v) => setAmount(v)} />
            </Form.Item>
            <Form.Item label=" ">
              <Button type="primary" htmlType="submit" loading={loading}>{t('phTester.actions.sendTest')}</Button>
            </Form.Item>
          </Space>
          {scenarioInfo && (
            <Typography.Text type="secondary">{t(`phTester.scenarios.${scenarioInfo.id}.description`)}</Typography.Text>
          )}
        </Form>
      </Card>
      {result && (
        <Card title={t('phTester.result')} style={{ marginTop: 16 }}>
          <Descriptions column={1} bordered>
            <Descriptions.Item label="result_type">
              <Tag color={resultColor(result.result_type)}>{result.result_type || '(empty)'}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="external_ref_no">{result.external_ref_no || '-'}</Descriptions.Item>
            <Descriptions.Item label="failure_code">{result.failure_code || '-'}</Descriptions.Item>
            <Descriptions.Item label="failure_message">{result.failure_message || '-'}</Descriptions.Item>
          </Descriptions>
        </Card>
      )}
    </>
  )
}

// ─── Matrix tab: run every (channel × scenario) cell ────────────────────────

function MatrixTest() {
  const { t } = useTranslation('channel')
  const [selectedChannels, setSelectedChannels] = useState<string[]>(CHANNELS.slice(0, 5).map((c) => c.value))
  const [selectedAmounts, setSelectedAmounts] = useState<number[]>([1, 2, 3, 4, 5, 6, 7, 8]) // skip 9 (timeout) by default
  const [concurrency, setConcurrency] = useState(4)
  const [cells, setCells] = useState<Record<string, Cell>>({})
  const [running, setRunning] = useState(false)
  const [done, setDone] = useState(0)
  const total = selectedChannels.length * selectedAmounts.length

  const cellKey = (ch: string, amt: number) => `${ch}:${amt}`

  const runAll = async () => {
    setRunning(true)
    setDone(0)
    const pending: Array<{ channel: string; amount: number }> = []
    const next: Record<string, Cell> = {}
    for (const ch of selectedChannels) {
      for (const amt of selectedAmounts) {
        pending.push({ channel: ch, amount: amt })
        next[cellKey(ch, amt)] = { channel: ch, amount: amt, status: 'idle' }
      }
    }
    setCells({ ...next })

    let finished = 0
    const queue = pending.slice()
    const worker = async () => {
      while (queue.length) {
        const task = queue.shift()
        if (!task) return
        const key = cellKey(task.channel, task.amount)
        next[key] = { ...next[key], status: 'running' }
        setCells({ ...next })
        const pmMap = CHANNELS.find((c) => c.value === task.channel)!
        const scn = SCENARIOS.find((s) => s.amount === task.amount)!
        const start = performance.now()
        try {
          const r = await probeRoute({
            country: 'PH', payment_method: pmMap.paymentMethod, amount: task.amount, currency: 'PHP',
          })
          const status: Cell['status'] =
            r.result_type === scn.expect ? 'pass' : 'mismatch'
          next[key] = {
            channel: task.channel, amount: task.amount,
            status, response: r, durationMs: Math.round(performance.now() - start),
          }
        } catch (e) {
          next[key] = {
            channel: task.channel, amount: task.amount,
            status: 'error', error: String(e),
            durationMs: Math.round(performance.now() - start),
          }
        }
        finished++
        setDone(finished)
        setCells({ ...next })
      }
    }
    const workers = Array.from({ length: Math.min(concurrency, total) }, () => worker())
    await Promise.all(workers)
    setRunning(false)
  }

  const summary = useMemo(() => {
    const cs = Object.values(cells)
    return {
      pass: cs.filter((c) => c.status === 'pass').length,
      mismatch: cs.filter((c) => c.status === 'mismatch').length,
      error: cs.filter((c) => c.status === 'error').length,
    }
  }, [cells])

  return (
    <Card>
      <Alert
        type="warning" showIcon style={{ marginBottom: 16 }}
        message={t('phTester.matrix.timeoutWarning')}
      />
      <Space direction="vertical" style={{ width: '100%' }}>
        <Space wrap>
          <Typography.Text strong>{t('phTester.matrix.channelLabel')}</Typography.Text>
          <Select mode="multiple" style={{ minWidth: 400 }}
            value={selectedChannels} onChange={setSelectedChannels}
            options={CHANNELS.map((c) => ({ value: c.value, label: c.label }))}
            maxTagCount="responsive" />
          <Button size="small" onClick={() => setSelectedChannels(CHANNELS.map((c) => c.value))}>{t('phTester.matrix.selectAll')}</Button>
          <Button size="small" onClick={() => setSelectedChannels(CHANNELS.slice(0, 5).map((c) => c.value))}>{t('phTester.matrix.selectTop5')}</Button>
        </Space>
        <Space wrap>
          <Typography.Text strong>{t('phTester.matrix.scenarioLabel')}</Typography.Text>
          <Select mode="multiple" style={{ minWidth: 400 }}
            value={selectedAmounts} onChange={setSelectedAmounts}
            options={SCENARIOS.map((s) => ({ value: s.amount, label: t(`phTester.scenarios.${s.id}.label`) }))}
            maxTagCount="responsive" />
          <Button size="small" onClick={() => setSelectedAmounts(SCENARIOS.map((s) => s.amount))}>{t('phTester.matrix.selectAllWithTimeout')}</Button>
          <Button size="small" onClick={() => setSelectedAmounts([1, 2, 3, 4, 5, 6, 7, 8])}>{t('phTester.matrix.excludeTimeout')}</Button>
        </Space>
        <Space wrap>
          <Typography.Text strong>{t('phTester.matrix.concurrencyLabel')}</Typography.Text>
          <Select style={{ width: 100 }} value={concurrency} onChange={setConcurrency}
            options={[1, 2, 4, 8, 16].map((n) => ({ value: n, label: String(n) }))} />
          <Button type="primary" disabled={running || total === 0} onClick={runAll}>
            {running ? t('phTester.matrix.running', { done, total }) : t('phTester.matrix.start', { total })}
          </Button>
          {total > 0 && (
            <Progress percent={Math.round((done / total) * 100)} style={{ width: 200 }} size="small" />
          )}
        </Space>

        {(summary.pass || summary.mismatch || summary.error) > 0 && (
          <Space>
            <Tag color="green">{t('phTester.matrix.summary.pass', { count: summary.pass })}</Tag>
            <Tag color="orange">{t('phTester.matrix.summary.mismatch', { count: summary.mismatch })}</Tag>
            <Tag color="red">{t('phTester.matrix.summary.error', { count: summary.error })}</Tag>
          </Space>
        )}
      </Space>

      <Table<Cell>
        style={{ marginTop: 16 }}
        size="small"
        rowKey={(c) => cellKey(c.channel, c.amount)}
        dataSource={Object.values(cells).sort((a, b) =>
          a.channel === b.channel ? a.amount - b.amount : a.channel.localeCompare(b.channel))}
        pagination={false}
        columns={[
          { title: t('phTester.matrix.columns.channel'), dataIndex: 'channel', width: 120, render: (v) => <Tag>{v}</Tag> },
          {
            title: t('phTester.matrix.columns.scenario'), dataIndex: 'amount', width: 100,
            render: (v: number) => {
              const scn = SCENARIOS.find((s) => s.amount === v)
              const label = scn ? t(`phTester.scenarios.${scn.id}.label`) : ''
              return <Typography.Text>{v}·{label.split('·')[1]?.trim() || ''}</Typography.Text>
            },
          },
          {
            title: t('phTester.matrix.columns.expected'), width: 140,
            render: (_, c) => {
              const scn = SCENARIOS.find((s) => s.amount === c.amount)!
              return <Tag color={resultColor(scn.expect)}>{scn.expect}</Tag>
            },
          },
          {
            title: t('phTester.matrix.columns.actualResultType'), width: 160,
            render: (_, c) => {
              if (c.status === 'idle') return <Typography.Text type="secondary">—</Typography.Text>
              if (c.status === 'running') return <Tag color="processing">{t('phTester.matrix.statusTag.running')}</Tag>
              if (c.status === 'error') return <Tag color="red">{t('phTester.matrix.statusTag.error')}</Tag>
              return <Tag color={resultColor(c.response?.result_type)}>{c.response?.result_type || '-'}</Tag>
            },
          },
          { title: t('phTester.matrix.columns.failureCode'), dataIndex: ['response', 'failure_code'], ellipsis: true },
          { title: t('phTester.matrix.columns.failureMsg'), dataIndex: ['response', 'failure_message'], ellipsis: true },
          { title: t('phTester.matrix.columns.duration'), dataIndex: 'durationMs', width: 90, render: (v?: number) => v ? `${v}ms` : '-' },
          {
            title: t('phTester.matrix.columns.result'), width: 100,
            render: (_, c) => {
              if (c.status === 'pass') return <Tag color="green">{t('phTester.matrix.statusTag.pass')}</Tag>
              if (c.status === 'mismatch') return <Tag color="orange">{t('phTester.matrix.statusTag.mismatch')}</Tag>
              if (c.status === 'error') return <Tag color="red">{t('phTester.matrix.statusTag.errorBadge')}</Tag>
              return null
            },
          },
        ]}
      />
    </Card>
  )
}

function resultColor(s?: string): string {
  switch (s) {
    case 'succeeded':       return 'green'
    case 'authorized':      return 'blue'
    case 'requires_action': return 'gold'
    case 'processing':      return 'geekblue'
    case 'failed':          return 'red'
    default:                return 'default'
  }
}
