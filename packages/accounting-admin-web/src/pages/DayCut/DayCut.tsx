import { Card, Button, DatePicker, Select, Space, Table, Tag, Alert, message, Tooltip } from 'antd'
import { ReloadOutlined, SyncOutlined } from '@ant-design/icons'
import { useState, useEffect, useCallback, useRef } from 'react'
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

const statusLabel = (entry: DayCutHistoryEntry) => {
  if (entry.processing > 0 || entry.pending > 0) return '处理中'
  if (entry.failed > 0) return '失败'
  if (entry.completed === entry.total_shards) return '完成'
  return '未知'
}

// 系统当前支持的币种。和 accounting-system 的 currency.precisionMap 大类一致；
// 后端校验 currency 是否在 precisionMap 内，前端给个常用 short list 让用户少打字。
const SUPPORTED_CURRENCIES = ['PHP', 'USD', 'CNY', 'EUR', 'JPY', 'HKD', 'SGD', 'THB', 'IDR', 'MYR', 'VND', 'KRW']

export default function DayCut() {
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
      const msg = (err as { message?: string })?.message ?? '加载日切记录失败'
      setError(msg)
      return []
    } finally {
      if (!silent) setHistoryLoading(false)
    }
  }, [])

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
      message.warning('请选择币种')
      return
    }
    const cutDate = selectedDate ? selectedDate.format('YYYY-MM-DD') : dayjs().subtract(1, 'day').format('YYYY-MM-DD')
    setTriggerLoading(true)
    setError(null)
    try {
      await triggerDayCut(cutDate, selectedCurrency)
      message.success(`日切任务已触发，日期: ${cutDate}, 币种: ${selectedCurrency}`)
      await loadHistory()
      startPolling()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? '日切触发失败'
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
      message.warning('请先在上方选币种再重跑')
      return
    }
    setRetriggeringDate(cutDate)
    setError(null)
    try {
      await triggerDayCut(cutDate, useCurrency)
      message.success(`日切重跑已触发，日期: ${cutDate}, 币种: ${useCurrency}`)
      await loadHistory()
      startPolling()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? '重跑失败'
      setError(msg)
      message.error(msg)
    } finally {
      setRetriggeringDate(null)
    }
  }

  const columns = [
    {
      title: '日切日期',
      dataIndex: 'cut_date',
      key: 'cut_date',
      width: 120,
    },
    {
      title: '版本号',
      dataIndex: 'run_id',
      key: 'run_id',
      width: 70,
      render: (v: number) => <span style={{ color: '#888' }}>v{v}</span>,
    },
    {
      title: '币种',
      dataIndex: 'currency',
      key: 'currency',
      width: 80,
      render: (c: string | undefined) =>
        c ? <Tag color="blue">{c}</Tag> : <Tag color="default">全部</Tag>,
    },
    {
      title: '整体状态',
      key: 'overall',
      width: 100,
      render: (_: unknown, record: DayCutHistoryEntry) => (
        <Tag color={statusOverall(record)}>{statusLabel(record)}</Tag>
      ),
    },
    {
      title: '总分片',
      dataIndex: 'total_shards',
      key: 'total_shards',
      width: 80,
    },
    {
      title: '已完成',
      dataIndex: 'completed',
      key: 'completed',
      width: 80,
      render: (v: number) => <span style={{ color: v > 0 ? '#3f8600' : undefined }}>{v}</span>,
    },
    {
      title: '失败',
      dataIndex: 'failed',
      key: 'failed',
      width: 70,
      render: (v: number) => <span style={{ color: v > 0 ? '#cf1322' : undefined }}>{v}</span>,
    },
    {
      title: '处理中',
      dataIndex: 'processing',
      key: 'processing',
      width: 80,
    },
    {
      title: '待处理',
      dataIndex: 'pending',
      key: 'pending',
      width: 80,
    },
    {
      title: '操作',
      key: 'action',
      width: 100,
      render: (_: unknown, record: DayCutHistoryEntry) => (
        <Tooltip title="重新执行该日期的日切（新增一个版本，之前数据保留）">
          <Button
            size="small"
            icon={<ReloadOutlined />}
            loading={retriggeringDate === record.cut_date}
            onClick={() => handleRetrigger(record.cut_date, record.currency)}
          >
            重跑
          </Button>
        </Tooltip>
      ),
    },
  ]

  return (
    <div>
      <Card title="触发日切" style={{ marginBottom: 16 }}>
        <Space>
          <DatePicker
            placeholder="选择日切日期（默认昨天）"
            value={selectedDate}
            onChange={setSelectedDate}
            disabledDate={(d) => d && d.isAfter(dayjs(), 'day')}
          />
          <Select
            value={selectedCurrency}
            onChange={setSelectedCurrency}
            placeholder="币种"
            style={{ width: 120 }}
            options={SUPPORTED_CURRENCIES.map((c) => ({ value: c, label: c }))}
            showSearch
          />
          <Button type="primary" loading={triggerLoading} onClick={handleTriggerDayCut}>
            触发日切
          </Button>
        </Space>
        <div style={{ marginTop: 8, color: '#888', fontSize: 13 }}>
          提示：日切按币种独立执行——不同币种 precision 不同，汇总没有意义。
          每个币种各跑一次，各自一份 snapshot。重跑同样按上方所选币种过滤。
        </div>
      </Card>

      {error && <Alert type="error" message={error} closable onClose={() => setError(null)} style={{ marginBottom: 16 }} />}

      <Card
        title={
          <Space>
            历史日切记录
            {polling && <SyncOutlined spin style={{ color: '#1890ff', fontSize: 14 }} />}
          </Space>
        }
        extra={
          <Button size="small" icon={<ReloadOutlined />} onClick={() => loadHistory()} loading={historyLoading}>
            刷新
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
          locale={{ emptyText: '暂无日切记录' }}
        />
      </Card>
    </div>
  )
}
