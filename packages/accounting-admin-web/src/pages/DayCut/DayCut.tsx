import { Card, Button, DatePicker, Select, Space, Table, Tag, Alert, message, Tooltip } from 'antd'
import { ReloadOutlined, SyncOutlined } from '@ant-design/icons'
import { useState, useEffect, useCallback, useRef } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import dayjs from 'dayjs'
import { triggerDayCut, getDayCutHistory } from '../../api/accounting'
import type { DayCutHistoryEntry } from '../../types/accounting'

const POLL_INTERVAL_MS = 3000

const isInProgress = (entry: DayCutHistoryEntry) => entry.pending > 0 || entry.processing > 0

const statusOverall = (entry: DayCutHistoryEntry): 'success' | 'error' | 'processing' | 'warning' | 'default' => {
  if (entry.failed > 0 && entry.pending === 0 && entry.processing === 0) return 'error'
  if (entry.processing > 0) return 'processing'
  if (entry.pending > 0) return 'warning'
  if (entry.completed === entry.total_shards) return 'success'
  return 'default'
}

const statusLabel = (entry: DayCutHistoryEntry, t: TFunction) => {
  if (entry.processing > 0 || entry.pending > 0) return t('status.processing')
  if (entry.failed > 0) return t('status.failed')
  if (entry.completed === entry.total_shards) return t('status.completed')
  return t('status.unknown')
}

// 系统当前支持的币种。和 accounting-system 的 currency.precisionMap 大类一致；
// 后端校验 currency 是否在 precisionMap 内，前端给个常用 short list 让用户少打字。
const SUPPORTED_CURRENCIES = ['PHP', 'USD', 'CNY', 'EUR', 'JPY', 'HKD', 'SGD', 'THB', 'IDR', 'MYR', 'VND', 'KRW']

export default function DayCut() {
  const { t } = useTranslation('daycut')
  const [triggerLoading, setTriggerLoading] = useState(false)
  const [selectedDate, setSelectedDate] = useState<dayjs.Dayjs | null>(null)
  const [selectedCurrency, setSelectedCurrency] = useState<string>('PHP')
  const [historyLoading, setHistoryLoading] = useState(false)
  const [history, setHistory] = useState<DayCutHistoryEntry[]>([])
  const [retriggeringDate, setRetriggeringDate] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [polling, setPolling] = useState(false)
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const loadHistory = useCallback(async (silent = false) => {
    if (!silent) setHistoryLoading(true)
    try {
      const res = await getDayCutHistory()
      const entries = res.entries ?? []
      setHistory(entries)
      return entries
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('history.loadFailed')
      setError(msg)
      return []
    } finally {
      if (!silent) setHistoryLoading(false)
    }
  }, [t])

  // 有进行中分片时自动轮询
  const scheduleNextPoll = useCallback(() => {
    pollTimerRef.current = setTimeout(async () => {
      const entries = await loadHistory(true)
      if (entries.some(isInProgress)) {
        scheduleNextPoll()
      } else {
        setPolling(false)
      }
    }, POLL_INTERVAL_MS)
  }, [loadHistory])

  const startPolling = useCallback(() => {
    if (pollTimerRef.current) return  // already polling
    setPolling(true)
    scheduleNextPoll()
  }, [scheduleNextPoll])

  useEffect(() => {
    loadHistory().then(entries => {
      if (entries.some(isInProgress)) startPolling()
    })
    return () => {
      if (pollTimerRef.current) clearTimeout(pollTimerRef.current)
    }
  }, [loadHistory, startPolling])

  const handleTriggerDayCut = async () => {
    if (!selectedCurrency) {
      message.warning(t('trigger.currencyRequired'))
      return
    }
    const cutDate = selectedDate ? selectedDate.format('YYYY-MM-DD') : dayjs().subtract(1, 'day').format('YYYY-MM-DD')
    setTriggerLoading(true)
    setError(null)
    try {
      await triggerDayCut(cutDate, selectedCurrency)
      message.success(t('trigger.triggerSuccess', { date: cutDate, currency: selectedCurrency }))
      await loadHistory()
      startPolling()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('trigger.triggerFailed')
      setError(msg)
      message.error(msg)
    } finally {
      setTriggerLoading(false)
    }
  }

  // 重跑用 row 自己的 currency（保证语义一致：原 USD run 的"重跑"必然还是 USD），
  // row 没有 currency（历史数据）才回落到表单当前选中。
  const handleRetrigger = async (cutDate: string, rowCurrency?: string) => {
    const useCurrency = rowCurrency || selectedCurrency
    if (!useCurrency) {
      message.warning(t('retrigger.currencyRequired'))
      return
    }
    setRetriggeringDate(cutDate)
    setError(null)
    try {
      await triggerDayCut(cutDate, useCurrency)
      message.success(t('retrigger.success', { date: cutDate, currency: useCurrency }))
      await loadHistory()
      startPolling()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('retrigger.failed')
      setError(msg)
      message.error(msg)
    } finally {
      setRetriggeringDate(null)
    }
  }

  const columns = [
    {
      title: t('columns.cutDate'),
      dataIndex: 'cut_date',
      key: 'cut_date',
      width: 120,
    },
    {
      title: t('columns.runId'),
      dataIndex: 'run_id',
      key: 'run_id',
      width: 70,
      render: (v: number) => <span style={{ color: '#888' }}>v{v}</span>,
    },
    {
      title: t('columns.currency'),
      dataIndex: 'currency',
      key: 'currency',
      width: 80,
      render: (c: string | undefined) =>
        c ? <Tag color="blue">{c}</Tag> : <Tag color="default">{t('columns.currencyAll')}</Tag>,
    },
    {
      title: t('columns.overall'),
      key: 'overall',
      width: 100,
      render: (_: unknown, record: DayCutHistoryEntry) => (
        <Tag color={statusOverall(record)}>{statusLabel(record, t)}</Tag>
      ),
    },
    {
      title: t('columns.totalShards'),
      dataIndex: 'total_shards',
      key: 'total_shards',
      width: 80,
    },
    {
      title: t('columns.completed'),
      dataIndex: 'completed',
      key: 'completed',
      width: 80,
      render: (v: number) => <span style={{ color: v > 0 ? '#3f8600' : undefined }}>{v}</span>,
    },
    {
      title: t('columns.failed'),
      dataIndex: 'failed',
      key: 'failed',
      width: 70,
      render: (v: number) => <span style={{ color: v > 0 ? '#cf1322' : undefined }}>{v}</span>,
    },
    {
      title: t('columns.processing'),
      dataIndex: 'processing',
      key: 'processing',
      width: 80,
    },
    {
      title: t('columns.pending'),
      dataIndex: 'pending',
      key: 'pending',
      width: 80,
    },
    {
      title: t('columns.action'),
      key: 'action',
      width: 100,
      render: (_: unknown, record: DayCutHistoryEntry) => (
        <Tooltip title={t('retrigger.tooltip')}>
          <Button
            size="small"
            icon={<ReloadOutlined />}
            loading={retriggeringDate === record.cut_date}
            onClick={() => handleRetrigger(record.cut_date, record.currency)}
          >
            {t('retrigger.button')}
          </Button>
        </Tooltip>
      ),
    },
  ]

  return (
    <div>
      <Card title={t('trigger.title')} style={{ marginBottom: 16 }}>
        <Space>
          <DatePicker
            placeholder={t('trigger.datePlaceholder')}
            value={selectedDate}
            onChange={setSelectedDate}
            disabledDate={(d) => d && d.isAfter(dayjs(), 'day')}
          />
          <Select
            value={selectedCurrency}
            onChange={setSelectedCurrency}
            placeholder={t('trigger.currencyPlaceholder')}
            style={{ width: 120 }}
            options={SUPPORTED_CURRENCIES.map((c) => ({ value: c, label: c }))}
            showSearch
          />
          <Button type="primary" loading={triggerLoading} onClick={handleTriggerDayCut}>
            {t('trigger.submit')}
          </Button>
        </Space>
        <div style={{ marginTop: 8, color: '#888', fontSize: 13 }}>
          {t('trigger.hint')}
        </div>
      </Card>

      {error && <Alert type="error" message={error} closable onClose={() => setError(null)} style={{ marginBottom: 16 }} />}

      <Card
        title={
          <Space>
            {t('history.title')}
            {polling && <SyncOutlined spin style={{ color: '#1890ff', fontSize: 14 }} />}
          </Space>
        }
        extra={
          <Button size="small" icon={<ReloadOutlined />} onClick={() => loadHistory()} loading={historyLoading}>
            {t('history.refresh')}
          </Button>
        }
      >
        <Table
          columns={columns}
          dataSource={history}
          rowKey={(r: DayCutHistoryEntry) => `${r.cut_date}_${r.run_id}_${r.currency || ''}`}
          loading={historyLoading}
          pagination={{ pageSize: 20, showSizeChanger: false }}
          size="small"
          locale={{ emptyText: t('history.empty') }}
        />
      </Card>
    </div>
  )
}
