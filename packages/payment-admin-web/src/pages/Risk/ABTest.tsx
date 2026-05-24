// ML A/B 显著性检验页 ── /risk/abtest
//
// 两块：
//   1. Challenger 管理：注册 / promote / drop 当前在跑的 challenger 模型
//      （从 cmd/retrain 训出来 → 把 weights 复制粘贴进表单）
//   2. 显著性报告：每条 challenger 跟 champion 的 AUC + 95% CI + 推荐
//      promote / hold / drop
//
// 数据来源：risk-manage ABTracker ring buffer（默认 4096 条三元组）+
// feedback.Recorder（outcome label）。
import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Drawer, Empty, Form, Input, InputNumber, Modal, Popconfirm,
  Space, Statistic, Switch, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { useTranslation } from 'react-i18next'
import {
  clearMLOverride, dropChallenger, getMLOverride, listChallengers, mlscoreABTest,
  promoteChallenger, registerChallenger, setMLOverride,
} from '../../api/risk'
import type {
  ABRecommendation, ChallengerListResp, ChallengerRegisterReq, ChallengerReport, FeatureWeights,
  MLOverrideStatus,
} from '../../api/risk'
import dayjs from 'dayjs'

const REC_COLOR: Record<ABRecommendation, string> = {
  promote: 'success',
  drop: 'error',
  hold: 'warning',
}

export default function ABTest() {
  const { t } = useTranslation('risk')
  return (
    <div>
      <Typography.Title level={3}>{t('abtest.title')}</Typography.Title>
      <Tabs
        defaultActiveKey="manage"
        items={[
          { key: 'manage', label: t('abtest.tabs.manage'), children: <ChallengerManage /> },
          { key: 'report', label: t('abtest.tabs.report'), children: <ABReport /> },
          { key: 'override', label: t('abtest.tabs.override'), children: <MLOverridePanel /> },
        ]}
      />
    </div>
  )
}

// ── Challenger 管理面板 ─────────────────────────────────────────

const FEATURE_NAMES: (keyof FeatureWeights)[] = [
  'HighAmount', 'IPProxy', 'IPVPN', 'IPDataCenter', 'IPCountryMismatch',
  'NoFingerprint', 'HeadlessRenderer', 'LowConcurrency', 'RapidCheckout',
  'NoMouseEntropy', 'BotTypingRhythm', 'NoKeystrokes', 'HighRiskCountry',
]

const ZERO_WEIGHTS: FeatureWeights = FEATURE_NAMES.reduce((acc, n) => {
  acc[n] = 0
  return acc
}, {} as FeatureWeights)

function ChallengerManage() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [data, setData] = useState<ChallengerListResp>({ scoring_disabled: false })
  const [registerOpen, setRegisterOpen] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setData(await listChallengers())
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  useEffect(() => { load() }, [load])

  const onPromote = (name: string) => {
    Modal.confirm({
      title: t('abtest.manage.promoteConfirm', { name }),
      content: t('abtest.manage.promoteContent', { champion: data.champion }),
      onOk: async () => {
        try {
          const r = await promoteChallenger(name)
          message.success(t('abtest.manage.promoteSuccess', { champion: r.champion, demoted: r.demoted }))
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onDrop = async (name: string) => {
    try {
      await dropChallenger(name)
      message.success(t('abtest.manage.dropSuccess', { name }))
      load()
    } catch (e) { message.error(String(e)) }
  }

  if (data.scoring_disabled) {
    return (
      <Card>
        <Alert
          type="warning" showIcon
          message={t('abtest.manage.scoringDisabledTitle')}
          description={t('abtest.manage.scoringDisabledDesc')}
        />
      </Card>
    )
  }

  return (
    <Card>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={
          <span>
            {t('abtest.manage.currentChampionPrefix')}<Typography.Text code strong>{data.champion || '-'}</Typography.Text>
            <span style={{ marginLeft: 16 }}>
              {t('abtest.manage.challengersPrefix')}<strong>{data.challengers?.length || 0}</strong>
            </span>
          </span>
        }
        description={t('abtest.manage.infoDesc')}
      />
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" onClick={() => setRegisterOpen(true)}>{t('abtest.manage.registerButton')}</Button>
        <Button onClick={load} loading={loading}>{t('common:actions.refresh')}</Button>
      </Space>
      <Table
        rowKey="name" size="small" loading={loading}
        dataSource={data.challengers || []}
        pagination={false}
        locale={{ emptyText: t('abtest.manage.emptyText') }}
        columns={[
          { title: t('abtest.manage.columns.name'), dataIndex: 'name', width: 220,
            render: (v) => <Typography.Text code strong>{v}</Typography.Text> },
          {
            title: t('abtest.manage.columns.actions'), width: 220,
            render: (_v, rec: { name: string }) => (
              <Space>
                <Button size="small" onClick={() => onPromote(rec.name)}>{t('abtest.manage.promoteButton')}</Button>
                <Popconfirm title={t('abtest.manage.dropConfirm', { name: rec.name })} onConfirm={() => onDrop(rec.name)}>
                  <Button size="small" danger>{t('abtest.manage.dropButton')}</Button>
                </Popconfirm>
              </Space>
            ),
          },
        ]}
      />
      <ChallengerRegisterDrawer
        open={registerOpen}
        onClose={() => setRegisterOpen(false)}
        onSaved={() => { setRegisterOpen(false); load() }}
      />
    </Card>
  )
}

function ChallengerRegisterDrawer({
  open, onClose, onSaved,
}: { open: boolean; onClose: () => void; onSaved: () => void }) {
  const { t } = useTranslation('risk')
  const [form] = Form.useForm<ChallengerRegisterReq>()
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')
  const [pasteJson, setPasteJson] = useState('')

  useEffect(() => {
    if (open) {
      form.resetFields()
      form.setFieldsValue({
        name: '',
        model_ver: '',
        intercept: -2.5,
        weights: ZERO_WEIGHTS,
        platt_a: 0,
        platt_b: 0,
      })
      setErr('')
      setPasteJson('')
    }
  }, [open, form])

  // 从 cmd/retrain 输出的 LogisticConfig JSON 直接 paste 自动填表
  const onPasteJSON = () => {
    try {
      const j = JSON.parse(pasteJson)
      form.setFieldsValue({
        model_ver: j.ModelVer || '',
        intercept: j.Intercept || 0,
        weights: { ...ZERO_WEIGHTS, ...(j.Weights || {}) },
        platt_a: j.PlattA || 0,
        platt_b: j.PlattB || 0,
      })
      message.success(t('abtest.register.filledFromJson'))
    } catch (e) {
      setErr(t('abtest.register.parseFailed', { error: String(e) }))
    }
  }

  const onSubmit = async () => {
    let v: ChallengerRegisterReq
    try { v = await form.validateFields() } catch { return }
    setSubmitting(true)
    setErr('')
    try {
      await registerChallenger(v)
      message.success(t('abtest.register.registered', { name: v.name }))
      onSaved()
    } catch (e: unknown) {
      const m = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      setErr(m)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Drawer
      open={open} onClose={onClose} width={640}
      title={t('abtest.register.title')}
      extra={
        <Space>
          <Button onClick={onClose}>{t('common:actions.cancel')}</Button>
          <Button type="primary" loading={submitting} onClick={onSubmit}>{t('abtest.register.registerButton')}</Button>
        </Space>
      }
    >
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('abtest.register.infoAlert')}
      />
      {err && <Alert type="error" showIcon closable message={t('abtest.register.errorTitle')} description={err}
        style={{ marginBottom: 16 }} onClose={() => setErr('')} />}
      <Form.Item label={t('abtest.register.pasteJsonLabel')}>
        <Input.TextArea
          value={pasteJson}
          onChange={(e) => setPasteJson(e.target.value)}
          autoSize={{ minRows: 4, maxRows: 8 }}
          placeholder='{"ModelVer":"...","Intercept":-2.5,"Weights":{...},"PlattA":1.2,"PlattB":-0.3}'
          style={{ fontFamily: 'ui-monospace, monospace' }}
        />
        <Button onClick={onPasteJSON} style={{ marginTop: 8 }} disabled={!pasteJson}>{t('abtest.register.fillFromJson')}</Button>
      </Form.Item>
      <Form layout="vertical" form={form}>
        <Form.Item name="name" label={t('abtest.register.fields.name')} rules={[{ required: true }]}>
          <Input placeholder="e.g. logistic-v2" />
        </Form.Item>
        <Form.Item name="model_ver" label={t('abtest.register.fields.modelVer')}>
          <Input placeholder="e.g. v2.0-2026-04" />
        </Form.Item>
        <Form.Item name="intercept" label={t('abtest.register.fields.intercept')}>
          <InputNumber step={0.1} style={{ width: 200 }} />
        </Form.Item>
        <Typography.Text strong>{t('abtest.register.fields.featureWeights')}</Typography.Text>
        <div style={{
          display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8, marginTop: 8, marginBottom: 16,
        }}>
          {FEATURE_NAMES.map((n) => (
            <Form.Item key={n} name={['weights', n]} label={n} style={{ marginBottom: 0 }}>
              <InputNumber step={0.1} style={{ width: '100%' }} />
            </Form.Item>
          ))}
        </div>
        <Space>
          <Form.Item name="platt_a" label={t('abtest.register.fields.plattA')}>
            <InputNumber step={0.1} style={{ width: 160 }} />
          </Form.Item>
          <Form.Item name="platt_b" label={t('abtest.register.fields.plattB')}>
            <InputNumber step={0.1} style={{ width: 160 }} />
          </Form.Item>
        </Space>
      </Form>
    </Drawer>
  )
}

// ── 显著性报告面板（原 ABTest 内容） ───────────────────────────

function ABReport() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [report, setReport] = useState<ChallengerReport[]>([])
  const [minLabeled, setMinLabeled] = useState(30)
  const [bootstrap, setBootstrap] = useState(1000)

  const REC_LABEL: Record<ABRecommendation, string> = {
    promote: t('abtest.recLabels.promote'),
    drop: t('abtest.recLabels.drop'),
    hold: t('abtest.recLabels.hold'),
  }

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await mlscoreABTest({ min_labeled: minLabeled, bootstrap })
      setReport(r.report || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [minLabeled, bootstrap])

  useEffect(() => { load() }, [load])

  return (
    <Card>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('abtest.report.infoMessage')}
        description={t('abtest.report.infoDesc')}
      />
      <Space style={{ marginBottom: 16 }} size="large">
        <Space>
          <Typography.Text strong>{t('abtest.report.minLabeledLabel')}</Typography.Text>
          <InputNumber min={5} max={10000} value={minLabeled} onChange={(v) => setMinLabeled(v || 30)} />
        </Space>
        <Space>
          <Typography.Text strong>{t('abtest.report.bootstrapLabel')}</Typography.Text>
          <InputNumber min={0} max={10000} step={100} value={bootstrap} onChange={(v) => setBootstrap(v || 0)} />
        </Space>
        <Button onClick={load} loading={loading}>{t('common:actions.refresh')}</Button>
      </Space>
      {report.length === 0 ? (
        <Empty description={t('abtest.report.empty')} />
      ) : (
        <Table<ChallengerReport>
          rowKey="name" size="middle" loading={loading} dataSource={report}
          pagination={false}
          columns={[
            { title: t('abtest.report.columns.challenger'), dataIndex: 'name', width: 180,
              render: (v) => <Typography.Text code strong>{v}</Typography.Text> },
            { title: t('abtest.report.columns.labeledSamples'), dataIndex: 'labeled_samples', width: 110, align: 'right' as const },
            { title: t('abtest.report.columns.championAuc'), dataIndex: 'champion_auc', width: 130, align: 'right' as const,
              render: (v: number) => v.toFixed(4) },
            { title: t('abtest.report.columns.challengerAuc'), dataIndex: 'challenger_auc', width: 130, align: 'right' as const,
              render: (v: number) => <strong>{v.toFixed(4)}</strong> },
            {
              title: t('abtest.report.columns.aucDiff'), dataIndex: 'auc_diff', width: 110, align: 'right' as const,
              render: (v: number) => {
                const color = v > 0 ? '#52c41a' : v < 0 ? '#cf1322' : undefined
                return <span style={{ color }}>{v >= 0 ? '+' : ''}{v.toFixed(4)}</span>
              },
            },
            {
              title: t('abtest.report.columns.ci'), width: 200,
              render: (_v, r) => {
                if (r.ci_low === 0 && r.ci_high === 0) {
                  return <Typography.Text type="secondary">{t('abtest.report.columns.ciNotComputed')}</Typography.Text>
                }
                return <span>[{r.ci_low.toFixed(4)}, {r.ci_high.toFixed(4)}]</span>
              },
            },
            {
              title: t('abtest.report.columns.recommend'), dataIndex: 'recommendation', width: 180,
              render: (v: ABRecommendation, r) => (
                <span>
                  <Tag color={REC_COLOR[v]}>{REC_LABEL[v]}</Tag>
                  <div style={{ fontSize: 12, color: '#888', marginTop: 4 }}>{r.recommend_reason}</div>
                </span>
              ),
            },
          ]}
        />
      )}
      {report.length > 0 && (
        <Space size="large" style={{ marginTop: 24 }} wrap>
          <Statistic title={t('abtest.report.stats.totalChallengers')} value={report.length} />
          <Statistic title={t('abtest.report.stats.canPromote')}
            value={report.filter((r) => r.recommendation === 'promote').length}
            valueStyle={{ color: '#52c41a' }} />
          <Statistic title={t('abtest.report.stats.shouldDrop')}
            value={report.filter((r) => r.recommendation === 'drop').length}
            valueStyle={{ color: '#cf1322' }} />
          <Statistic title={t('abtest.report.stats.keepHold')}
            value={report.filter((r) => r.recommendation === 'hold').length}
            valueStyle={{ color: '#faad14' }} />
        </Space>
      )}
    </Card>
  )
}

// ── ML 降级开关 panel：紧急关 ML / 强制分数 ─────────────────

function MLOverridePanel() {
  const { t } = useTranslation('risk')
  const [loading, setLoading] = useState(false)
  const [status, setStatus] = useState<MLOverrideStatus | null>(null)
  const [disabled, setDisabled] = useState(false)
  const [forceScore, setForceScore] = useState(0)
  const [reason, setReason] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await getMLOverride()
      setStatus(r)
      setDisabled(r.disabled)
      setForceScore(r.force_score)
      setReason(r.reason)
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  useEffect(() => { load() }, [load])

  const onApply = async () => {
    if (!reason || reason.length < 4) {
      message.warning(t('abtest.override.reasonRequired'))
      return
    }
    Modal.confirm({
      title: t('abtest.override.applyConfirmTitle'),
      content: (
        <div>
          {disabled
            ? <div><strong>{t('abtest.override.applyConfirmDisabled')}</strong>{t('abtest.override.applyConfirmDisabledSuffix')}</div>
            : forceScore !== 0
              ? <div>{t('abtest.override.applyConfirmForceScore')}<strong>force_score={forceScore}</strong>{t('abtest.override.applyConfirmForceScoreSuffix')}</div>
              : <div>{t('abtest.override.applyConfirmNoop')}</div>}
          <div style={{ marginTop: 8, color: '#888' }}>{t('abtest.override.applyConfirmReason', { reason })}</div>
        </div>
      ),
      okButtonProps: { danger: disabled || forceScore !== 0 },
      onOk: async () => {
        try {
          await setMLOverride({ disabled, force_score: forceScore, reason })
          message.success(t('abtest.override.applied'))
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onClear = async () => {
    Modal.confirm({
      title: t('abtest.override.clearConfirmTitle'),
      content: t('abtest.override.clearConfirmContent'),
      onOk: async () => {
        try {
          await clearMLOverride()
          message.success(t('abtest.override.cleared'))
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  return (
    <Card>
      <Alert
        type={status?.is_active ? 'error' : 'info'}
        showIcon style={{ marginBottom: 16 }}
        message={status?.is_active ? t('abtest.override.activeTitle') : t('abtest.override.normalTitle')}
        description={
          status && (
            <Space direction="vertical" size={2}>
              <span>disabled = <strong>{String(status.disabled)}</strong></span>
              <span>force_score = <strong>{status.force_score}</strong></span>
              <span>reason: {status.reason || '-'}</span>
              {status.set_at && (
                <span>{t('abtest.override.setByLine', {
                  actor: status.set_by || t('abtest.override.unknownActor'),
                  time: dayjs(status.set_at).format('YYYY-MM-DD HH:mm:ss'),
                })}</span>
              )}
            </Space>
          )
        }
        action={
          status?.is_active && (
            <Button danger size="small" onClick={onClear}>{t('abtest.override.clearButton')}</Button>
          )
        }
      />
      <Alert
        type="warning" showIcon style={{ marginBottom: 16 }}
        message={t('abtest.override.warningTitle')}
        description={t('abtest.override.warningDesc')}
      />
      <Space direction="vertical" size="middle" style={{ width: '100%' }}>
        <Space>
          <Switch
            checked={disabled} onChange={setDisabled}
            checkedChildren={t('abtest.override.switchDisabled')} unCheckedChildren={t('abtest.override.switchEnabled')}
          />
          <Typography.Text type={disabled ? 'danger' : 'secondary'}>
            {disabled ? t('abtest.override.willSkip') : t('abtest.override.runningNormal')}
          </Typography.Text>
        </Space>
        <Space>
          <Typography.Text strong>{t('abtest.override.forceScoreLabel')}</Typography.Text>
          <InputNumber
            min={0} max={1} step={0.01} value={forceScore}
            onChange={(v) => setForceScore(v ?? 0)}
            style={{ width: 160 }}
          />
          <Typography.Text type="secondary">
            {t('abtest.override.forceScoreHint')}
          </Typography.Text>
        </Space>
        <div>
          <Typography.Text strong>{t('abtest.override.reasonLabel')}</Typography.Text>
          <Input.TextArea
            value={reason} onChange={(e) => setReason(e.target.value)}
            rows={2} placeholder={t('abtest.override.reasonPlaceholder')}
            style={{ marginTop: 4 }}
          />
        </div>
        <Space>
          <Button type="primary" danger={disabled || forceScore !== 0} onClick={onApply}>
            {t('abtest.override.applyButton')}
          </Button>
          <Button onClick={load} loading={loading}>{t('common:actions.refresh')}</Button>
        </Space>
      </Space>
    </Card>
  )
}
