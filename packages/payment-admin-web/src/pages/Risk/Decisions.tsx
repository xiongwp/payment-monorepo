// Risk Decisions 审计 + 单笔 Explain ── /risk/decisions
//
// 列出最近决策，并支持点击 "解释" 把这笔用**当前**规则集再跑一次（手算
// counterfactual）。运营调试客户投诉、误伤分析、规则改完想看效果时用。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, DatePicker, Drawer, Input, Modal, Select, Space, Statistic, Table, Tag, Typography,
  message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import { explainDecision, listDecisions, searchDecisions, verifyAuditChain } from '../../api/risk'
import type { AuditChainVerifyResp, AuditRow, DecisionRow, ExplainResp } from '../../api/risk'

const VERDICT_COLOR: Record<string, string> = {
  ALLOW: 'success', REVIEW: 'warning', DENY: 'error',
}

export default function Decisions() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<DecisionRow[]>([])
  const [filterID, setFilterID] = useState('')
  const [explainID, setExplainID] = useState<string | null>(null)

  // 服务端搜索条件（merchant / customer / IP / verdict / 时间），任一非空 →
  // 走 /admin/audit/search，否则走 listDecisions 拉最近 500 条。
  const [searchMerchant, setSearchMerchant] = useState('')
  const [searchCustomer, setSearchCustomer] = useState('')
  const [searchIP, setSearchIP] = useState('')
  const [searchVerdict, setSearchVerdict] = useState<string | undefined>()
  const [searchSince, setSearchSince] = useState<dayjs.Dayjs | null>(null)
  const [searchUntil, setSearchUntil] = useState<dayjs.Dayjs | null>(null)

  const isServerSearch =
    !!searchMerchant || !!searchCustomer || !!searchIP || !!searchVerdict ||
    !!searchSince || !!searchUntil

  const load = useCallback(async () => {
    setLoading(true)
    try {
      if (isServerSearch) {
        const r = await searchDecisions({
          merchant_id: searchMerchant || undefined,
          customer_id: searchCustomer || undefined,
          ip: searchIP || undefined,
          verdict: searchVerdict || undefined,
          since: searchSince ? searchSince.toISOString() : undefined,
          until: searchUntil ? searchUntil.toISOString() : undefined,
          limit: 500,
        })
        // AuditRow → DecisionRow 投影（前端表格的字段子集）
        const items: DecisionRow[] = (r.items || []).map((a: AuditRow) => ({
          decision_id: a.decision_id,
          occurred_at: a.occurred_at,
          verdict: a.verdict as 'ALLOW' | 'REVIEW' | 'DENY',
          risk_score: a.risk_score,
          risk_level: a.risk_level,
          hit_rules: (a.hits || []).map((h) => h.rule_id),
          eval_duration_ms: a.eval_duration_ms || 0,
        }))
        setRows(items)
      } else {
        const r = await listDecisions(500)
        setRows(r.items || [])
      }
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [isServerSearch, searchMerchant, searchCustomer, searchIP, searchVerdict, searchSince, searchUntil])

  useEffect(() => { load() }, [load])

  const onClearSearch = () => {
    setSearchMerchant('')
    setSearchCustomer('')
    setSearchIP('')
    setSearchVerdict(undefined)
    setSearchSince(null)
    setSearchUntil(null)
  }

  const filtered = filterID
    ? rows.filter((r) => r.decision_id.includes(filterID))
    : rows

  return (
    <div>
      <Typography.Title level={3}>{t('decisions.title')}</Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message={t('decisions.infoAlert')}
        />
        <Space style={{ marginBottom: 12 }} wrap>
          <Input
            placeholder={t('decisions.search.merchantPlaceholder')} allowClear style={{ width: 180 }}
            value={searchMerchant} onChange={(e) => setSearchMerchant(e.target.value.trim())}
          />
          <Input
            placeholder={t('decisions.search.customerPlaceholder')} allowClear style={{ width: 180 }}
            value={searchCustomer} onChange={(e) => setSearchCustomer(e.target.value.trim())}
          />
          <Input
            placeholder={t('decisions.search.ipPlaceholder')} allowClear style={{ width: 140 }}
            value={searchIP} onChange={(e) => setSearchIP(e.target.value.trim())}
          />
          <Select
            placeholder={t('decisions.search.verdictPlaceholder')} allowClear style={{ width: 120 }}
            value={searchVerdict} onChange={setSearchVerdict}
            options={[
              { value: 'ALLOW', label: 'ALLOW' },
              { value: 'REVIEW', label: 'REVIEW' },
              { value: 'DENY', label: 'DENY' },
            ]}
          />
          <DatePicker
            placeholder={t('decisions.search.sincePlaceholder')} showTime value={searchSince} onChange={setSearchSince}
          />
          <DatePicker
            placeholder={t('decisions.search.untilPlaceholder')} showTime value={searchUntil} onChange={setSearchUntil}
          />
          {isServerSearch && (
            <Button onClick={onClearSearch}>{t('decisions.search.clearButton')}</Button>
          )}
        </Space>
        <Space style={{ marginBottom: 16 }}>
          <Input.Search
            placeholder={t('decisions.search.localFilterPlaceholder')} allowClear style={{ width: 320 }}
            onChange={(e) => setFilterID(e.target.value.trim())}
          />
          <Button onClick={load} loading={loading}>{t('common:actions.refresh')}</Button>
          <ChainVerifyButton />
          <Typography.Text type="secondary">
            {isServerSearch ? t('decisions.search.serverPrefix') : t('decisions.search.latestPrefix')}
            {t('decisions.search.hitSummary', { shown: filtered.length, total: rows.length })}
          </Typography.Text>
        </Space>
        <Table<DecisionRow>
          rowKey="decision_id" size="small" loading={loading} dataSource={filtered}
          pagination={{ pageSize: 25 }}
          columns={[
            {
              title: t('decisions.columns.decisionId'), dataIndex: 'decision_id', width: 270, ellipsis: true,
              render: (v) => <Typography.Text code copyable>{v}</Typography.Text>,
            },
            {
              title: t('decisions.columns.time'), dataIndex: 'occurred_at', width: 170,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss.SSS'),
            },
            {
              title: t('decisions.columns.verdict'), dataIndex: 'verdict', width: 90,
              render: (v: string) => <Tag color={VERDICT_COLOR[v] || 'default'}>{v}</Tag>,
            },
            {
              title: t('decisions.columns.riskScore'), dataIndex: 'risk_score', width: 80, align: 'center',
              render: (v: number) => <Tag>{v}</Tag>,
            },
            { title: t('decisions.columns.riskLevel'), dataIndex: 'risk_level', width: 90 },
            {
              title: t('decisions.columns.hitRules'), dataIndex: 'hit_rules',
              render: (vs: string[]) => (
                <Space size={4} wrap>
                  {(vs || []).map((s, i) => <Tag key={i}>{s}</Tag>)}
                </Space>
              ),
            },
            {
              title: t('decisions.columns.evalMs'), dataIndex: 'eval_duration_ms', width: 100, align: 'right',
              render: (v: number) => v?.toFixed(2),
            },
            {
              title: t('decisions.columns.actions'), width: 90,
              render: (_v, r) => (
                <Button size="small" onClick={() => setExplainID(r.decision_id)}>{t('decisions.explainButton')}</Button>
              ),
            },
          ]}
        />
      </Card>
      <ExplainDrawer
        decisionID={explainID}
        onClose={() => setExplainID(null)}
      />
    </div>
  )
}

// ── Explain Drawer：原决策 vs 当前规则集重放 ───────────────────

function ExplainDrawer({
  decisionID, onClose,
}: { decisionID: string | null; onClose: () => void }) {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [data, setData] = useState<ExplainResp | null>(null)
  const [err, setErr] = useState('')

  const load = useCallback(async (id: string) => {
    setLoading(true)
    setErr('')
    setData(null)
    try {
      setData(await explainDecision(id))
    } catch (e: unknown) {
      const m = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      setErr(m)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (decisionID) load(decisionID)
  }, [decisionID, load])

  const open = decisionID !== null
  const verdictDrift = data && data.original.verdict !== data.replayed.verdict
  const scoreDrift = data && data.original.risk_score !== data.replayed.risk_score

  return (
    <Drawer
      open={open} onClose={onClose} width={900}
      title={t('decisions.explainDrawer.title', { id: decisionID || '' })}
    >
      {loading && <Typography.Text>{t('decisions.explainDrawer.loading')}</Typography.Text>}
      {err && (
        <Alert type="error" showIcon message={t('decisions.explainDrawer.loadFailed')} description={err} />
      )}
      {data && (
        <>
          {(verdictDrift || scoreDrift) && (
            <Alert
              type="warning" showIcon style={{ marginBottom: 16 }}
              message={t('decisions.explainDrawer.driftMessage')}
              description={
                <span>
                  {t('decisions.explainDrawer.driftDescPrefix')}<Tag color={VERDICT_COLOR[data.original.verdict]}>{data.original.verdict}</Tag>
                  {t('decisions.explainDrawer.driftDescConnector')}
                  <Tag color={VERDICT_COLOR[data.replayed.verdict]}>{data.replayed.verdict}</Tag>
                  {scoreDrift && <span>{t('decisions.explainDrawer.driftScore', { orig: data.original.risk_score, replay: data.replayed.risk_score })}</span>}
                  {t('decisions.explainDrawer.driftSuffix')}
                </span>
              }
            />
          )}
          <Space size="large" style={{ marginBottom: 24 }} wrap>
            <Statistic
              title={t('decisions.explainDrawer.originalVerdict')} valueStyle={{ fontSize: 18 }}
              value={data.original.verdict}
              valueRender={() => <Tag color={VERDICT_COLOR[data.original.verdict]}>{data.original.verdict}</Tag>}
            />
            <Statistic
              title={t('decisions.explainDrawer.replayedVerdict')} valueStyle={{ fontSize: 18 }}
              value={data.replayed.verdict}
              valueRender={() => <Tag color={VERDICT_COLOR[data.replayed.verdict]}>{data.replayed.verdict}</Tag>}
            />
            <Statistic title={t('decisions.explainDrawer.originalScore')} value={data.original.risk_score} />
            <Statistic
              title={t('decisions.explainDrawer.replayedScore')} value={data.replayed.risk_score}
              valueStyle={{ color: scoreDrift ? '#cf1322' : undefined }}
            />
            <Statistic
              title={t('decisions.explainDrawer.occurredAt')} valueStyle={{ fontSize: 14 }}
              value={dayjs(data.original.occurred_at).format('YYYY-MM-DD HH:mm:ss')}
            />
          </Space>

          <Typography.Title level={5}>{t('decisions.explainDrawer.inputSnapshot')}</Typography.Title>
          <Card size="small" style={{ marginBottom: 24 }}>
            <Space direction="vertical" size={4} style={{ width: '100%' }}>
              {data.input.payment_intent_id &&
                <div>PaymentIntent: <Typography.Text code>{data.input.payment_intent_id}</Typography.Text></div>}
              {data.input.merchant_id &&
                <div>Merchant: <Typography.Text code>{data.input.merchant_id}</Typography.Text></div>}
              {data.input.customer_id &&
                <div>Customer: <Typography.Text code>{data.input.customer_id}</Typography.Text></div>}
              {data.input.amount !== undefined && data.input.currency &&
                <div>{t('decisions.explainDrawer.amountLabel')}<strong>{data.input.amount}</strong> {data.input.currency}</div>}
              {data.input.payment_method &&
                <div>{t('decisions.explainDrawer.paymentMethodLabel')}<Tag>{data.input.payment_method}</Tag></div>}
              {data.input.country &&
                <div>{t('decisions.explainDrawer.countryLabel')}<Tag>{data.input.country}</Tag></div>}
              {data.input.ip_address &&
                <div>IP: <Typography.Text code>{data.input.ip_address}</Typography.Text></div>}
              {data.input.device_id &&
                <div>Device: <Typography.Text code>{data.input.device_id}</Typography.Text></div>}
              {data.input.metadata && Object.keys(data.input.metadata).length > 0 && (
                <div>Metadata: <pre style={{ background: '#fafafa', padding: 8, borderRadius: 4 }}>
                  {JSON.stringify(data.input.metadata, null, 2)}
                </pre></div>
              )}
            </Space>
          </Card>

          <Typography.Title level={5}>{t('decisions.explainDrawer.hitCompareTitle')}</Typography.Title>
          <HitsCompare
            original={data.original.hits}
            replayed={data.replayed.hits}
          />

          {data.replayed.shadow_hits && data.replayed.shadow_hits.length > 0 && (
            <>
              <Typography.Title level={5} style={{ marginTop: 16 }}>{t('decisions.explainDrawer.shadowHitsTitle')}</Typography.Title>
              <Table size="small" pagination={false}
                rowKey={(r) => r.rule_id} dataSource={data.replayed.shadow_hits}
                columns={[
                  { title: t('decisions.explainDrawer.shadowColumns.rule'), dataIndex: 'rule_name', width: 240 },
                  { title: t('decisions.explainDrawer.shadowColumns.decision'), dataIndex: 'decision', width: 100,
                    render: (v) => <Tag color="orange">{v}</Tag> },
                  { title: t('decisions.explainDrawer.shadowColumns.detail'), dataIndex: 'detail' },
                ]}
              />
            </>
          )}
        </>
      )}
    </Drawer>
  )
}

// HitsCompare 横向对比原命中 vs 重放命中：取 union of rule_ids，标 only_orig /
// only_replay / both 三种状态。
function HitsCompare({
  original, replayed,
}: { original: import('../../api/risk').DecisionHit[]; replayed: import('../../api/risk').DecisionHit[] }) {
  const { t } = useTranslation('risk')
  const origMap = new Map(original.map((h) => [h.rule_id, h]))
  const replayMap = new Map(replayed.map((h) => [h.rule_id, h]))
  const allIDs = Array.from(new Set([...origMap.keys(), ...replayMap.keys()]))
  const rows = allIDs.map((id) => {
    const o = origMap.get(id)
    const r = replayMap.get(id)
    let status: 'both' | 'only_orig' | 'only_replay' = 'both'
    if (o && !r) status = 'only_orig'
    if (!o && r) status = 'only_replay'
    return {
      rule_id: id,
      rule_name: o?.rule_name || r?.rule_name || id,
      orig_decision: o?.decision || '',
      replay_decision: r?.decision || '',
      orig_detail: o?.detail || '',
      replay_detail: r?.detail || '',
      status,
    }
  })

  const STATUS_COLOR: Record<string, string> = {
    both: 'green', only_orig: 'red', only_replay: 'orange',
  }
  const STATUS_LABEL: Record<string, string> = {
    both: t('decisions.hitsCompare.statusLabels.both'),
    only_orig: t('decisions.hitsCompare.statusLabels.onlyOrig'),
    only_replay: t('decisions.hitsCompare.statusLabels.onlyReplay'),
  }

  return (
    <Table size="small" pagination={false} rowKey="rule_id" dataSource={rows}
      columns={[
        { title: t('decisions.hitsCompare.columns.rule'), dataIndex: 'rule_name', width: 220,
          render: (v, r) => <span><Typography.Text code>{r.rule_id}</Typography.Text>{' '}{v !== r.rule_id ? <span style={{ color: '#888' }}>{v}</span> : null}</span> },
        { title: t('decisions.hitsCompare.columns.status'), dataIndex: 'status', width: 160,
          render: (v: string) => <Tag color={STATUS_COLOR[v]}>{STATUS_LABEL[v]}</Tag> },
        { title: t('decisions.hitsCompare.columns.originalDecision'), dataIndex: 'orig_decision', width: 90,
          render: (v) => v ? <Tag color={VERDICT_COLOR[v] || 'default'}>{v}</Tag> : '-' },
        { title: t('decisions.hitsCompare.columns.replayedDecision'), dataIndex: 'replay_decision', width: 90,
          render: (v) => v ? <Tag color={VERDICT_COLOR[v] || 'default'}>{v}</Tag> : '-' },
        { title: t('decisions.hitsCompare.columns.detail'), render: (_v, r) => r.replay_detail || r.orig_detail },
      ]}
    />
  )
}

// ── 审计链完整性按钮：合规 / 取证场景一键验证 ────────────

function ChainVerifyButton() {
  const { t } = useTranslation('risk')
  const [verifying, setVerifying] = useState(false)

  const onVerify = async () => {
    setVerifying(true)
    try {
      const r = await verifyAuditChain(1000)
      Modal[r.ok ? 'success' : 'error']({
        title: r.ok ? t('decisions.chainVerify.successTitle') : t('decisions.chainVerify.failureTitle'),
        content: <ChainVerifyResult result={r} />,
        width: 600,
      })
    } catch (e: unknown) {
      const msg = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      message.error(msg)
    } finally {
      setVerifying(false)
    }
  }

  return (
    <Button onClick={onVerify} loading={verifying}>
      {t('decisions.verifyChainButton')}
    </Button>
  )
}

function ChainVerifyResult({ result }: { result: AuditChainVerifyResp }) {
  const { t } = useTranslation('risk')
  if (result.ok) {
    return (
      <div>
        <p>{t('decisions.chainVerify.successDescPart1')}<strong>{result.verified}</strong>{t('decisions.chainVerify.successDescPart2')}</p>
        <p style={{ color: '#888' }}>
          {t('decisions.chainVerify.successAlgorithm')}
        </p>
      </div>
    )
  }
  return (
    <div>
      <p>{t('decisions.chainVerify.failurePart1')}<strong>{result.verified}</strong>{t('decisions.chainVerify.failurePart2')}</p>
      <p>{t('decisions.chainVerify.failureIndex')}<Typography.Text code>{result.failed_at_index}</Typography.Text></p>
      <p>{t('decisions.chainVerify.failureField')}<Typography.Text code>{result.error_field || '-'}</Typography.Text></p>
      {result.want && (
        <p style={{ wordBreak: 'break-all' }}>
          {t('decisions.chainVerify.expected')}<Typography.Text code copyable>{result.want}</Typography.Text>
        </p>
      )}
      {result.got && (
        <p style={{ wordBreak: 'break-all' }}>
          {t('decisions.chainVerify.actual')}<Typography.Text code copyable>{result.got}</Typography.Text>
        </p>
      )}
      <p style={{ color: '#cf1322' }}>
        {t('decisions.chainVerify.diagnosis')}
      </p>
    </div>
  )
}
