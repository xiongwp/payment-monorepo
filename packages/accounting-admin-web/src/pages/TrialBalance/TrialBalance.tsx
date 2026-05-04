import {
  Card,
  Button,
  DatePicker,
  Select,
  Space,
  Table,
  Tag,
  Descriptions,
  Alert,
  Statistic,
  Row,
  Col,
  message,
  List,
} from 'antd'
import { CheckCircleOutlined, CloseCircleOutlined, ReloadOutlined } from '@ant-design/icons'
import { useState, useEffect, useCallback } from 'react'
import dayjs from 'dayjs'
import { runTrialBalance, listSnapshotDates } from '../../api/accounting'
import type { TrialBalanceResult, TrialBalanceCategorySummary } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

// TrialBalanceResult has no currency field — every row on this page is an
// aggregate across accounts, and the system is single-currency (PHP) in
// practice. If multi-currency ever ships, aggregation already breaks upstream
// so this fallback is defensive.
const FALLBACK_CURRENCY = 'PHP'

const CATEGORY_LABEL: Record<string, string> = {
  ASSET:     '资产',
  LIABILITY: '负债',
  EQUITY:    '所有者权益',
  REVENUE:   '收入',
  EXPENSE:   '费用',
}

const ACCOUNT_TYPE_LABEL: Record<number, string> = {
  1: '用户账户',
  2: '商户账户',
  3: '商户待结算账户',
  4: '平台损益账户',
  5: '中间账户渠道应收款',
  6: '中间账户渠道应付款',
  7: '平台手续费账户',
  8: '平台服务费账户',
  9: '平台中间账户',
}

// 与 DayCut 页面保持同一份；后端按 currency.precisionMap 校验，未在内的会被拒。
const SUPPORTED_CURRENCIES = ['PHP', 'USD', 'CNY', 'EUR', 'JPY', 'HKD', 'SGD', 'THB', 'IDR', 'MYR', 'VND', 'KRW']

export default function TrialBalance() {
  const [selectedDate, setSelectedDate] = useState<dayjs.Dayjs | null>(
    dayjs().subtract(1, 'day'),
  )
  const [selectedCurrency, setSelectedCurrency] = useState<string>('PHP')
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<TrialBalanceResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [snapshotDates, setSnapshotDates] = useState<string[]>([])
  const [datesLoading, setDatesLoading] = useState(false)

  const loadSnapshotDates = useCallback(async () => {
    setDatesLoading(true)
    try {
      const res = await listSnapshotDates()
      setSnapshotDates(res.dates ?? [])
    } catch {
      // non-fatal: just show empty list
    } finally {
      setDatesLoading(false)
    }
  }, [])

  useEffect(() => {
    loadSnapshotDates()
  }, [loadSnapshotDates])

  const handleRun = async (dateStr?: string) => {
    const date = dateStr ?? (selectedDate ? selectedDate.format('YYYY-MM-DD') : '')
    if (!date) {
      message.warning('请选择快照日期')
      return
    }
    setLoading(true)
    setError(null)
    setResult(null)
    // sync picker to the date being run
    if (!selectedDate || selectedDate.format('YYYY-MM-DD') !== date) {
      setSelectedDate(dayjs(date))
    }
    if (!selectedCurrency) {
      message.warning('请选择币种')
      setLoading(false)
      return
    }
    try {
      const res = await runTrialBalance(date, selectedCurrency)
      setResult(res)
      if (res.is_balanced && res.is_equation_valid) {
        message.success('试算平衡通过，借贷平衡且会计恒等式成立')
      } else {
        message.warning('试算平衡发现差异，请检查明细')
      }
      // refresh dates list to pick up newly created snapshots
      loadSnapshotDates()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? '试算平衡失败'
      setError(msg)
      message.error(msg)
    } finally {
      setLoading(false)
    }
  }

  // 币种优先级：result 自带的 → 用户选的 → PHP 兜底。
  // 历史代码里硬编码过 FALLBACK_CURRENCY=PHP，导致 USD 试算结果也印 ₱ 符号。
  const displayCurrency = result?.currency || selectedCurrency || FALLBACK_CURRENCY
  const fmt = (v: string | number) => displayMoney(v as string | number, displayCurrency)

  const summaryColumns = [
    {
      title: '账户类别',
      dataIndex: 'category',
      key: 'category',
      render: (v: string) => CATEGORY_LABEL[v] ?? v,
    },
    {
      title: '账户类型',
      dataIndex: 'type',
      key: 'type',
      render: (v: number) => ACCOUNT_TYPE_LABEL[v] ?? v,
    },
    { title: '账户数', dataIndex: 'account_count', key: 'account_count' },
    { title: '期初余额合计', dataIndex: 'sum_beginning', key: 'sum_beginning', render: (v: string) => fmt(v) },
    { title: '期末余额合计', dataIndex: 'sum_ending', key: 'sum_ending', render: (v: string) => fmt(v) },
    { title: '期间借方合计', dataIndex: 'sum_debit', key: 'sum_debit', render: (v: string) => fmt(v) },
    { title: '期间贷方合计', dataIndex: 'sum_credit', key: 'sum_credit', render: (v: string) => fmt(v) },
  ]

  return (
    <div>
      <Row gutter={16}>
        {/* ─── 左侧：历史日期列表 ─────────────────────────────────── */}
        <Col span={5}>
          <Card
            title="历史快照日期"
            size="small"
            extra={
              <Button size="small" icon={<ReloadOutlined />} onClick={loadSnapshotDates} loading={datesLoading} />
            }
            style={{ height: '100%' }}
            bodyStyle={{ padding: 0 }}
          >
            <List
              loading={datesLoading}
              dataSource={snapshotDates}
              locale={{ emptyText: '暂无快照' }}
              renderItem={(date) => (
                <List.Item
                  style={{
                    padding: '8px 12px',
                    cursor: 'pointer',
                    background: result?.snapshot_date === date ? '#e6f7ff' : undefined,
                  }}
                  onClick={() => handleRun(date)}
                >
                  <span style={{ fontSize: 13 }}>{date}</span>
                </List.Item>
              )}
            />
          </Card>
        </Col>

        {/* ─── 右侧：试算平衡操作区 ────────────────────────────────── */}
        <Col span={19}>
          <Card title="试算平衡" style={{ marginBottom: 16 }}>
            <Space>
              <DatePicker
                value={selectedDate}
                onChange={setSelectedDate}
                placeholder="选择快照日期"
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
              <Button type="primary" loading={loading} onClick={() => handleRun()}>
                执行试算平衡
              </Button>
            </Space>
            <div style={{ marginTop: 8, color: '#888', fontSize: 13 }}>
              说明：试算平衡在日切完成后执行，验证指定日期的借贷平衡与会计恒等式。也可直接点击左侧历史日期查看。
            </div>
          </Card>

          {error && <Alert type="error" message={error} closable onClose={() => setError(null)} style={{ marginBottom: 16 }} />}

          {result && (
            <>
              {/* 汇总校验状态 */}
              <Row gutter={16} style={{ marginBottom: 16 }}>
                <Col span={6}>
                  <Card>
                    <Statistic
                      title="借贷平衡"
                      value={result.is_balanced ? '通过' : '不平衡'}
                      prefix={
                        result.is_balanced ? (
                          <CheckCircleOutlined />
                        ) : (
                          <CloseCircleOutlined />
                        )
                      }
                      valueStyle={{ color: result.is_balanced ? '#3f8600' : '#cf1322' }}
                    />
                  </Card>
                </Col>
                <Col span={6}>
                  <Card>
                    <Statistic
                      title="会计恒等式"
                      value={result.is_equation_valid ? '成立' : '不成立'}
                      prefix={
                        result.is_equation_valid ? (
                          <CheckCircleOutlined />
                        ) : (
                          <CloseCircleOutlined />
                        )
                      }
                      valueStyle={{ color: result.is_equation_valid ? '#3f8600' : '#cf1322' }}
                    />
                  </Card>
                </Col>
                <Col span={6}>
                  <Card>
                    <Statistic title="期间借方合计" value={fmt(result.total_debit)} />
                  </Card>
                </Col>
                <Col span={6}>
                  <Card>
                    <Statistic title="期间贷方合计" value={fmt(result.total_credit)} />
                  </Card>
                </Col>
              </Row>

              {/* 会计恒等式明细 */}
              <Card title="会计恒等式明细（资产 = 负债 + 所有者权益 + 收入 - 费用）" style={{ marginBottom: 16 }}>
                <Descriptions column={3} bordered size="small">
                  <Descriptions.Item label="快照日期">{result.snapshot_date}</Descriptions.Item>
                  <Descriptions.Item label="币种">
                    <Tag color="blue">{result.currency || '-'}</Tag>
                  </Descriptions.Item>
                  <Descriptions.Item label="借贷差额">{fmt(result.imbalance)}</Descriptions.Item>
                  <Descriptions.Item label="等式差额">{fmt(result.equation_diff)}</Descriptions.Item>
                  <Descriptions.Item label="资产期末余额合计">
                    <span style={{ fontWeight: 'bold', color: '#1890ff' }}>{fmt(result.asset_ending_balance)}</span>
                  </Descriptions.Item>
                  <Descriptions.Item label="负债期末余额合计">{fmt(result.liability_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label="权益期末余额合计">{fmt(result.equity_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label="收入期末余额合计">{fmt(result.revenue_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label="费用期末余额合计">{fmt(result.expense_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label="会计恒等式">
                    <Tag color={result.is_equation_valid ? 'green' : 'red'}>
                      {result.is_equation_valid ? '资产 = 负债+权益+收入-费用 ✓' : '不平衡 ✗'}
                    </Tag>
                  </Descriptions.Item>
                </Descriptions>
              </Card>

              {/* 分类明细 */}
              <Card title="账户类别 × 类型 明细">
                <Table
                  columns={summaryColumns}
                  dataSource={result.summaries}
                  rowKey={(r: TrialBalanceCategorySummary) => `${r.category}-${r.type}`}
                  pagination={false}
                  size="small"
                />
              </Card>
            </>
          )}
        </Col>
      </Row>
    </div>
  )
}
