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
  Drawer,
} from 'antd'
import {
  CheckCircleOutlined,
  CloseCircleOutlined,
  ReloadOutlined,
  DownloadOutlined,
  SearchOutlined,
} from '@ant-design/icons'
import { useState, useEffect, useCallback } from 'react'
import { useTranslation } from 'react-i18next'
import dayjs from 'dayjs'
import {
  runTrialBalance,
  listSnapshotDates,
  drilldownTrialBalance,
  exportTrialBalanceURL,
} from '../../api/accounting'
// AccountBalanceDetail（4 层下钻明细行）定义在 api 层，跟 drilldown API 同源。
import type { AccountBalanceDetail } from '../../api/accounting'
import type {
  TrialBalanceResult,
  TrialBalanceCategorySummary,
} from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

// TrialBalanceResult has no currency field — every row on this page is an
// aggregate across accounts, and the system is single-currency (PHP) in
// practice. If multi-currency ever ships, aggregation already breaks upstream
// so this fallback is defensive.
const FALLBACK_CURRENCY = 'PHP'

// 与 DayCut 页面保持同一份；后端按 currency.precisionMap 校验，未在内的会被拒。
const SUPPORTED_CURRENCIES = ['PHP', 'USD', 'CNY', 'EUR', 'JPY', 'HKD', 'SGD', 'THB', 'IDR', 'MYR', 'VND', 'KRW']

export default function TrialBalance() {
  const { t } = useTranslation('trial')
  const [selectedDate, setSelectedDate] = useState<dayjs.Dayjs | null>(
    dayjs().subtract(1, 'day'),
  )
  const [selectedCurrency, setSelectedCurrency] = useState<string>('PHP')
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<TrialBalanceResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [snapshotDates, setSnapshotDates] = useState<string[]>([])
  const [datesLoading, setDatesLoading] = useState(false)
  // 下钻抽屉状态
  const [drilldownOpen, setDrilldownOpen] = useState(false)
  const [drilldownLoading, setDrilldownLoading] = useState(false)
  const [drilldownRows, setDrilldownRows] = useState<AccountBalanceDetail[]>([])
  const [drilldownTitle, setDrilldownTitle] = useState('')

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
      message.warning(t('form.dateRequired'))
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
      message.warning(t('form.currencyRequired'))
      setLoading(false)
      return
    }
    try {
      const res = await runTrialBalance(date, selectedCurrency)
      setResult(res)
      if (res.is_balanced && res.is_equation_valid) {
        message.success(t('form.balancedMessage'))
      } else {
        message.warning(t('form.imbalancedMessage'))
      }
      // refresh dates list to pick up newly created snapshots
      loadSnapshotDates()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('form.runFailed')
      setError(msg)
      message.error(msg)
    } finally {
      setLoading(false)
    }
  }

  // 下钻：点 summaries 某行 → 拉该 (category, type, business_type) 下的 account 级明细。
  const handleDrilldown = async (row: TrialBalanceCategorySummary) => {
    setDrilldownOpen(true)
    setDrilldownLoading(true)
    setDrilldownRows([])
    const catLabel = t(`category.${row.category}`, { defaultValue: row.category })
    const typeLabel = t(`accountType.${row.type}`, { defaultValue: String(row.type) })
    setDrilldownTitle(
      `${catLabel} / ${typeLabel}` + (row.business_type ? ` / ${t('summary.columns.businessType')} ${row.business_type}` : ''),
    )
    try {
      const res = await drilldownTrialBalance({
        currency: displayCurrency,
        category: row.category,
        account_type: row.type,
        business_type: row.business_type || 0,
        snapshot_date: result?.snapshot_date ?? '',
      })
      setDrilldownRows(res.accounts ?? [])
      if (res.warning) message.warning(res.warning)
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('drilldown.failed')
      message.error(msg)
    } finally {
      setDrilldownLoading(false)
    }
  }

  // CSV 导出:当前 currency + snapshot_date → 触发浏览器下载。
  const handleExport = () => {
    if (!selectedCurrency) {
      message.warning(t('form.currencyRequired'))
      return
    }
    const url = exportTrialBalanceURL(displayCurrency, result?.snapshot_date ?? '')
    window.open(url, '_blank')
  }

  // 币种优先级：result 自带的 → 用户选的 → PHP 兜底。
  // 历史代码里硬编码过 FALLBACK_CURRENCY=PHP，导致 USD 试算结果也印 ₱ 符号。
  const displayCurrency = result?.currency || selectedCurrency || FALLBACK_CURRENCY
  const fmt = (v: string | number) => displayMoney(v as string | number, displayCurrency)

  const summaryColumns = [
    {
      title: t('summary.columns.category'),
      dataIndex: 'category',
      key: 'category',
      render: (v: string) => t(`category.${v}`, { defaultValue: v }),
    },
    {
      title: t('summary.columns.type'),
      dataIndex: 'type',
      key: 'type',
      render: (v: number) => t(`accountType.${v}`, { defaultValue: String(v) }),
    },
    {
      title: t('summary.columns.businessType'),
      dataIndex: 'business_type',
      key: 'business_type',
      render: (v: number | undefined) => (v ? v : '-'),
    },
    { title: t('summary.columns.accountCount'), dataIndex: 'account_count', key: 'account_count' },
    { title: t('summary.columns.sumBeginning'), dataIndex: 'sum_beginning', key: 'sum_beginning', render: (v: string) => fmt(v) },
    { title: t('summary.columns.sumEnding'), dataIndex: 'sum_ending', key: 'sum_ending', render: (v: string) => fmt(v) },
    { title: t('summary.columns.sumDebit'), dataIndex: 'sum_debit', key: 'sum_debit', render: (v: string) => fmt(v) },
    { title: t('summary.columns.sumCredit'), dataIndex: 'sum_credit', key: 'sum_credit', render: (v: string) => fmt(v) },
    {
      title: t('summary.columns.action'),
      key: 'action',
      render: (_: unknown, row: TrialBalanceCategorySummary) => (
        <Button size="small" icon={<SearchOutlined />} onClick={() => handleDrilldown(row)}>
          {t('drilldown.button')}
        </Button>
      ),
    },
  ]

  const drilldownColumns = [
    { title: t('drilldown.columns.accountNo'), dataIndex: 'account_no', key: 'account_no' },
    {
      title: t('summary.columns.type'),
      dataIndex: 'account_type',
      key: 'account_type',
      render: (v: number) => t(`accountType.${v}`, { defaultValue: String(v) }),
    },
    { title: t('summary.columns.businessType'), dataIndex: 'account_business_type', key: 'account_business_type' },
    { title: t('drilldown.columns.beginning'), dataIndex: 'beginning', key: 'beginning', render: (v: number) => fmt(v) },
    { title: t('drilldown.columns.debit'), dataIndex: 'debit', key: 'debit', render: (v: number) => fmt(v) },
    { title: t('drilldown.columns.credit'), dataIndex: 'credit', key: 'credit', render: (v: number) => fmt(v) },
    { title: t('drilldown.columns.ending'), dataIndex: 'ending', key: 'ending', render: (v: number) => fmt(v) },
    {
      title: t('drilldown.columns.balance'),
      dataIndex: 'balance',
      key: 'balance',
      render: (v: number) => <span style={{ fontWeight: 'bold' }}>{fmt(v)}</span>,
    },
  ]

  return (
    <div>
      <Row gutter={16}>
        {/* ─── 左侧：历史日期列表 ─────────────────────────────────── */}
        <Col span={5}>
          <Card
            title={t('snapshots.title')}
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
              locale={{ emptyText: t('snapshots.empty') }}
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
          <Card title={t('form.title')} style={{ marginBottom: 16 }}>
            <Space>
              <DatePicker
                value={selectedDate}
                onChange={setSelectedDate}
                placeholder={t('form.datePlaceholder')}
                disabledDate={(d) => d && d.isAfter(dayjs(), 'day')}
              />
              <Select
                value={selectedCurrency}
                onChange={setSelectedCurrency}
                placeholder={t('form.currencyPlaceholder')}
                style={{ width: 120 }}
                options={SUPPORTED_CURRENCIES.map((c) => ({ value: c, label: c }))}
                showSearch
              />
              <Button type="primary" loading={loading} onClick={() => handleRun()}>
                {t('form.submit')}
              </Button>
              <Button
                icon={<DownloadOutlined />}
                disabled={!result}
                onClick={handleExport}
              >
                {t('export.button')}
              </Button>
            </Space>
            <div style={{ marginTop: 8, color: '#888', fontSize: 13 }}>
              {t('form.hint')}
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
                      title={t('stats.balanceTitle')}
                      value={result.is_balanced ? t('stats.balancePass') : t('stats.balanceFail')}
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
                      title={t('stats.equationTitle')}
                      value={result.is_equation_valid ? t('stats.equationPass') : t('stats.equationFail')}
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
                    <Statistic title={t('stats.totalDebit')} value={fmt(result.total_debit)} />
                  </Card>
                </Col>
                <Col span={6}>
                  <Card>
                    <Statistic title={t('stats.totalCredit')} value={fmt(result.total_credit)} />
                  </Card>
                </Col>
              </Row>

              {/* 会计恒等式明细 */}
              <Card title={t('equation.title')} style={{ marginBottom: 16 }}>
                <Descriptions column={3} bordered size="small">
                  <Descriptions.Item label={t('equation.snapshotDate')}>{result.snapshot_date}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.currency')}>
                    <Tag color="blue">{result.currency || '-'}</Tag>
                  </Descriptions.Item>
                  <Descriptions.Item label={t('equation.imbalance')}>{fmt(result.imbalance)}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.equationDiff')}>{fmt(result.equation_diff)}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.assetEnding')}>
                    <span style={{ fontWeight: 'bold', color: '#1890ff' }}>{fmt(result.asset_ending_balance)}</span>
                  </Descriptions.Item>
                  <Descriptions.Item label={t('equation.liabilityEnding')}>{fmt(result.liability_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.equityEnding')}>{fmt(result.equity_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.revenueEnding')}>{fmt(result.revenue_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.expenseEnding')}>{fmt(result.expense_ending_balance)}</Descriptions.Item>
                  <Descriptions.Item label={t('equation.accountingEquation')}>
                    <Tag color={result.is_equation_valid ? 'green' : 'red'}>
                      {result.is_equation_valid ? t('equation.equationValid') : t('equation.equationInvalid')}
                    </Tag>
                  </Descriptions.Item>
                </Descriptions>
              </Card>

              {/* 分类明细 */}
              <Card title={t('summary.title')}>
                <Table
                  columns={summaryColumns}
                  dataSource={result.summaries}
                  rowKey={(r: TrialBalanceCategorySummary) => `${r.category}-${r.type}-${r.business_type ?? 0}`}
                  pagination={false}
                  size="small"
                />
              </Card>
            </>
          )}
        </Col>
      </Row>

      {/* 下钻明细抽屉：account 级，按 |balance| 降序 */}
      <Drawer
        title={`${t('drilldown.title')}${drilldownTitle ? ' — ' + drilldownTitle : ''}`}
        open={drilldownOpen}
        onClose={() => setDrilldownOpen(false)}
        width={880}
      >
        <Table
          columns={drilldownColumns}
          dataSource={drilldownRows}
          loading={drilldownLoading}
          rowKey={(r: AccountBalanceDetail) => r.account_no}
          pagination={{ pageSize: 20, showSizeChanger: true }}
          size="small"
          locale={{ emptyText: t('drilldown.empty') }}
        />
      </Drawer>
    </div>
  )
}
