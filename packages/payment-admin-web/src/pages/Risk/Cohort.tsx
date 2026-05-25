// 风控数据分析 ── /risk/cohort
//
// 按商户 / 国家 / 支付方式拆分决策，让运营一眼看出：
//   - 哪个商户 block_rate 异常高 → 业务突变 vs 风控误伤
//   - 哪个国家 actual_fraud_rate 飙升 → 加规则
//   - 哪个 payment_method precision 暴跌 → 该渠道 fraud pattern 变了
//
// 数据来自 risk-manage audit MemSink + feedback Recorder（in-mem ring，
// 实际窗口 ~ 4096 条）。生产部署后建议改 ClickHouse 长期聚合表。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Radio, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import { dashboardCohort, dashboardCohortTimeseries } from '../../api/risk'
import type {
  CohortBucket, CohortGroupBy, CohortStats, CohortTimeBucket,
} from '../../api/risk'

const GROUP_BY_KEYS: CohortGroupBy[] = ['merchant_id', 'country', 'payment_method']

export default function Cohort() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [groupBy, setGroupBy] = useState<CohortGroupBy>('merchant_id')
  const [minTotal, setMinTotal] = useState(10)
  const [rows, setRows] = useState<CohortStats[]>([])
  const [sample, setSample] = useState(0)
  const [timelineKey, setTimelineKey] = useState<string | null>(null)

  const groupByLabel = (k: CohortGroupBy) => t(`cohort.groupBy.${k}`)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await dashboardCohort({ group_by: groupBy, min_total: minTotal })
      setRows(r.cohorts || [])
      setSample(r.sample)
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [groupBy, minTotal])

  useEffect(() => { load() }, [load])

  // 平台均值（all cohorts 加权）— 给 cohort 跟均值对比标"异常"用
  const totalAll = rows.reduce((s, r) => s + r.total, 0)
  const blockAll = rows.reduce((s, r) => s + r.block, 0)
  const platformBlockRate = totalAll > 0 ? blockAll / totalAll : 0
  const labeledAll = rows.reduce((s, r) => s + r.labeled_total, 0)
  const fraudAll = rows.reduce((s, r) => s + r.actual_fraud, 0)
  const platformFraudRate = labeledAll > 0 ? fraudAll / labeledAll : 0

  // "异常 cohort" 阈值：block_rate 或 fraud_rate 高于平台 2 倍
  const anomalyBlockThreshold = platformBlockRate * 2
  const anomalyFraudThreshold = platformFraudRate * 2

  return (
    <div>
      <Typography.Title level={3}>{t('cohort.title')}</Typography.Title>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('cohort.infoAlert', { sample })}
      />
      <Card>
        <Space style={{ marginBottom: 16 }} size="large" wrap>
          <Space>
            <Typography.Text strong>{t('cohort.groupByLabel')}</Typography.Text>
            <Radio.Group
              value={groupBy} onChange={(e) => setGroupBy(e.target.value)}
              optionType="button" buttonStyle="solid"
              options={GROUP_BY_KEYS.map((k) => ({
                value: k, label: groupByLabel(k),
              }))}
            />
          </Space>
          <Space>
            <Typography.Text strong>{t('cohort.minSampleLabel')}</Typography.Text>
            <Select
              style={{ width: 100 }} value={minTotal} onChange={setMinTotal}
              options={[
                { value: 0, label: t('cohort.minSampleOptions.all') },
                { value: 5, label: t('cohort.minSampleOptions.ge5') },
                { value: 10, label: t('cohort.minSampleOptions.ge10') },
                { value: 50, label: t('cohort.minSampleOptions.ge50') },
                { value: 100, label: t('cohort.minSampleOptions.ge100') },
              ]}
            />
          </Space>
          <Typography.Text type="secondary">
            {t('cohort.platformSummaryPart1')}<strong>{(platformBlockRate * 100).toFixed(2)}%</strong>
            {t('cohort.platformSummaryPart2')}<strong>{(platformFraudRate * 100).toFixed(2)}%</strong>
          </Typography.Text>
        </Space>
        <Table<CohortStats>
          rowKey="key" size="small" loading={loading} dataSource={rows}
          pagination={{ pageSize: 50 }}
          columns={[
            {
              title: groupByLabel(groupBy), dataIndex: 'key', width: 220, ellipsis: true,
              render: (v) => <Typography.Text code>{v}</Typography.Text>,
            },
            {
              title: t('cohort.columns.decisions'), dataIndex: 'total', width: 90, align: 'right' as const,
              sorter: (a, b) => a.total - b.total,
            },
            {
              title: t('cohort.columns.blockCount'), width: 100, align: 'right' as const,
              render: (_v, r) => `${r.block} / ${r.total}`,
            },
            {
              title: t('cohort.columns.blockRate'), dataIndex: 'block_rate', width: 110, align: 'right' as const,
              sorter: (a, b) => a.block_rate - b.block_rate,
              render: (v: number) => {
                const pct = (v * 100).toFixed(2) + '%'
                if (anomalyBlockThreshold > 0 && v > anomalyBlockThreshold) {
                  return <Tag color="error">{pct}</Tag>
                }
                return pct
              },
            },
            {
              title: t('cohort.columns.labeledTotal'), dataIndex: 'labeled_total', width: 100, align: 'right' as const,
            },
            {
              title: t('cohort.columns.actualFraud'), width: 130, align: 'right' as const,
              render: (_v, r) => r.labeled_total > 0
                ? `${r.actual_fraud} / ${r.labeled_total}`
                : <Typography.Text type="secondary">-</Typography.Text>,
            },
            {
              title: t('cohort.columns.fraudRate'), dataIndex: 'actual_fraud_rate', width: 110, align: 'right' as const,
              sorter: (a, b) => a.actual_fraud_rate - b.actual_fraud_rate,
              render: (v: number, r) => {
                if (r.labeled_total === 0) return <Typography.Text type="secondary">-</Typography.Text>
                const pct = (v * 100).toFixed(2) + '%'
                if (anomalyFraudThreshold > 0 && v > anomalyFraudThreshold) {
                  return <Tag color="error">{pct}</Tag>
                }
                return pct
              },
            },
            {
              title: t('cohort.columns.tpFp'), width: 100, align: 'right' as const,
              render: (_v, r) => r.blocked_fraud + r.blocked_legit > 0
                ? `${r.blocked_fraud} / ${r.blocked_legit}`
                : <Typography.Text type="secondary">-</Typography.Text>,
            },
            {
              title: t('cohort.columns.precisionAtBlock'), dataIndex: 'precision_at_block', width: 130,
              align: 'right' as const,
              sorter: (a, b) => a.precision_at_block - b.precision_at_block,
              render: (v: number, r) => {
                const blockLabeled = r.blocked_fraud + r.blocked_legit
                if (blockLabeled < 5) return <Typography.Text type="secondary">{t('cohort.nLessThan5')}</Typography.Text>
                const color = v < 0.3 ? 'red' : v < 0.6 ? 'orange' : 'green'
                return <Tag color={color}>{(v * 100).toFixed(1)}%</Tag>
              },
            },
            {
              title: t('cohort.columns.trend'), width: 80,
              render: (_v, r) => (
                <Button size="small" type="link" onClick={() => setTimelineKey(r.key)}>
                  {t('cohort.timelineButton')}
                </Button>
              ),
            },
          ]}
        />
      </Card>
      <CohortTimelineDrawer
        groupBy={groupBy}
        cohortKey={timelineKey}
        onClose={() => setTimelineKey(null)}
      />
    </div>
  )
}

// ── Timeline drawer：单个 cohort 的 block_rate / fraud_rate 趋势 ──

function CohortTimelineDrawer({
  groupBy, cohortKey, onClose,
}: { groupBy: CohortGroupBy; cohortKey: string | null; onClose: () => void }) {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [series, setSeries] = useState<CohortTimeBucket[]>([])
  const [bucket, setBucket] = useState<CohortBucket>('day')
  const [sample, setSample] = useState(0)

  const open = cohortKey !== null

  const load = useCallback(async () => {
    if (!cohortKey) return
    setLoading(true)
    try {
      const r = await dashboardCohortTimeseries({
        group_by: groupBy, key: cohortKey, bucket,
      })
      setSeries(r.series || [])
      setSample(r.sample)
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [groupBy, cohortKey, bucket])

  useEffect(() => { if (open) load() }, [open, load])

  return (
    <Drawer
      open={open} onClose={onClose} width={760}
      title={t('cohort.timeline.title', { key: cohortKey || '', sample })}
    >
      <Space style={{ marginBottom: 16 }}>
        <Typography.Text strong>{t('cohort.timeline.bucketLabel')}</Typography.Text>
        <Radio.Group
          value={bucket} onChange={(e) => setBucket(e.target.value)}
          optionType="button" buttonStyle="solid"
          options={[
            { value: 'hour', label: t('cohort.timeline.bucketHour') },
            { value: 'day', label: t('cohort.timeline.bucketDay') },
            { value: 'week', label: t('cohort.timeline.bucketWeek') },
          ]}
        />
        <Button onClick={load} loading={loading}>{t('common:actions.refresh')}</Button>
      </Space>
      {series.length > 0 ? (
        <>
          <SparkChart
            title={t('cohort.timeline.blockRateTitle')} color="#fa541c"
            buckets={series} valueOf={(b) => b.block_rate}
          />
          <SparkChart
            title={t('cohort.timeline.fraudRateTitle')} color="#cf1322"
            buckets={series.filter((b) => b.labeled_total > 0)}
            valueOf={(b) => b.actual_fraud_rate}
          />
          <Table<CohortTimeBucket>
            rowKey="bucket_start" size="small" dataSource={series}
            pagination={{ pageSize: 30 }}
            columns={[
              {
                title: t('cohort.timeline.columns.time'), dataIndex: 'bucket_start', width: 160,
                render: (v: string) => dayjs(v).format(bucket === 'hour' ? 'MM-DD HH:mm' : 'YYYY-MM-DD'),
              },
              { title: t('cohort.timeline.columns.decisions'), dataIndex: 'total', width: 80, align: 'right' as const },
              {
                title: t('cohort.timeline.columns.block'), width: 100, align: 'right' as const,
                render: (_v, r) => `${r.block} (${(r.block_rate * 100).toFixed(1)}%)`,
              },
              {
                title: t('cohort.timeline.columns.outcome'), width: 90, align: 'right' as const,
                render: (_v, r) => r.labeled_total > 0 ? r.labeled_total : '-',
              },
              {
                title: t('cohort.timeline.columns.fraudRate'), dataIndex: 'actual_fraud_rate', width: 110,
                align: 'right' as const,
                render: (v: number, r) => r.labeled_total > 0
                  ? (v * 100).toFixed(2) + '%'
                  : <Typography.Text type="secondary">-</Typography.Text>,
              },
            ]}
          />
        </>
      ) : (
        <Typography.Text type="secondary">{t('cohort.timeline.emptyText')}</Typography.Text>
      )}
    </Drawer>
  )
}

// SparkChart 简单 SVG 折线图（无外部 lib）。给 cohort timeline 画 metric
// 趋势用。x 轴按桶顺序；y 轴归一化到 [0, max]；横线表示均值。
function SparkChart({
  title, color, buckets, valueOf,
}: {
  title: string; color: string; buckets: CohortTimeBucket[];
  valueOf: (b: CohortTimeBucket) => number
}) {
  const { t } = useTranslation('risk')
  const W = 700, H = 100, padX = 30, padY = 12
  if (buckets.length === 0) {
    return (
      <div style={{ marginBottom: 24 }}>
        <Typography.Text strong>{title}</Typography.Text>
        <div><Typography.Text type="secondary">{t('cohort.timeline.sparkEmpty')}</Typography.Text></div>
      </div>
    )
  }
  const values = buckets.map(valueOf)
  const max = Math.max(...values, 0.001)
  const min = 0
  const avg = values.reduce((s, v) => s + v, 0) / values.length
  const xStep = (W - padX * 2) / Math.max(buckets.length - 1, 1)
  const yScale = (v: number) => H - padY - ((v - min) / (max - min)) * (H - padY * 2)
  const path = values.map((v, i) => `${i === 0 ? 'M' : 'L'} ${padX + i * xStep} ${yScale(v)}`).join(' ')
  return (
    <div style={{ marginBottom: 24 }}>
      <Typography.Text strong>{title}</Typography.Text>
      <span style={{ marginLeft: 12 }}>
        <Typography.Text type="secondary">
          avg {(avg * 100).toFixed(2)}% · max {(max * 100).toFixed(2)}% · n={buckets.length}
        </Typography.Text>
      </span>
      <svg width={W} height={H} style={{ display: 'block', marginTop: 6, background: '#fafafa', borderRadius: 4 }}>
        {/* 平均线 */}
        <line x1={padX} y1={yScale(avg)} x2={W - padX} y2={yScale(avg)}
              stroke="#888" strokeDasharray="3,3" strokeWidth={1} />
        {/* 主曲线 */}
        <path d={path} fill="none" stroke={color} strokeWidth={2} />
        {/* 数据点 */}
        {values.map((v, i) => (
          <circle key={i} cx={padX + i * xStep} cy={yScale(v)} r={2.5} fill={color}>
            <title>{`${dayjs(buckets[i].bucket_start).format('MM-DD HH:mm')} → ${(v * 100).toFixed(2)}%`}</title>
          </circle>
        ))}
        {/* y 轴 max 标 */}
        <text x={4} y={padY + 4} fontSize="10" fill="#888">{(max * 100).toFixed(1)}%</text>
        <text x={4} y={H - padY + 4} fontSize="10" fill="#888">0%</text>
      </svg>
    </div>
  )
}
