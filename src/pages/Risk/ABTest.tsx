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

const REC_LABEL: Record<ABRecommendation, string> = {
  promote: '升级 (promote)',
  drop: '下线 (drop)',
  hold: '继续观察 (hold)',
}

export default function ABTest() {
  return (
    <div>
      <Typography.Title level={3}>ML A/B 显著性检验</Typography.Title>
      <Tabs
        defaultActiveKey="manage"
        items={[
          { key: 'manage', label: '模型管理 (challenger)', children: <ChallengerManage /> },
          { key: 'report', label: '显著性报告', children: <ABReport /> },
          { key: 'override', label: '紧急降级 / 强制分数', children: <MLOverridePanel /> },
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
      title: `升级 ${name} 为 champion？`,
      content: `当前 champion ${data.champion} 会自动降级为 challenger 留观。`,
      onOk: async () => {
        try {
          const r = await promoteChallenger(name)
          message.success(`已升级：${r.champion}（原 champion ${r.demoted} 已降级为 challenger）`)
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onDrop = async (name: string) => {
    try {
      await dropChallenger(name)
      message.success(`已下线 ${name}`)
      load()
    } catch (e) { message.error(String(e)) }
  }

  if (data.scoring_disabled) {
    return (
      <Card>
        <Alert
          type="warning" showIcon
          message="ML scoring 已禁用"
          description="服务启动时 mlscore.enabled=false，无法注册 challenger。改 config 后重启。"
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
            当前 Champion: <Typography.Text code strong>{data.champion || '-'}</Typography.Text>
            <span style={{ marginLeft: 16 }}>
              Challengers: <strong>{data.challengers?.length || 0}</strong>
            </span>
          </span>
        }
        description="Challenger 跟 champion 并行打分但不影响主决策；显著优于 champion 时升级。从 cmd/retrain 训出来后填到 “注册 challenger” 表单。"
      />
      <Space style={{ marginBottom: 16 }}>
        <Button type="primary" onClick={() => setRegisterOpen(true)}>注册 challenger</Button>
        <Button onClick={load} loading={loading}>刷新</Button>
      </Space>
      <Table
        rowKey="name" size="small" loading={loading}
        dataSource={data.challengers || []}
        pagination={false}
        locale={{ emptyText: '没有 challenger。点 "注册 challenger" 添加。' }}
        columns={[
          { title: 'Name', dataIndex: 'name', width: 220,
            render: (v) => <Typography.Text code strong>{v}</Typography.Text> },
          {
            title: '操作', width: 220,
            render: (_v, rec: { name: string }) => (
              <Space>
                <Button size="small" onClick={() => onPromote(rec.name)}>升级为 champion</Button>
                <Popconfirm title={`下线 ${rec.name}？`} onConfirm={() => onDrop(rec.name)}>
                  <Button size="small" danger>下线</Button>
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
      message.success('已从 JSON 填入字段')
    } catch (e) {
      setErr(`paste JSON 解析失败: ${e}`)
    }
  }

  const onSubmit = async () => {
    let v: ChallengerRegisterReq
    try { v = await form.validateFields() } catch { return }
    setSubmitting(true)
    setErr('')
    try {
      await registerChallenger(v)
      message.success(`challenger ${v.name} 已注册`)
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
      title="注册 Challenger 模型"
      extra={
        <Space>
          <Button onClick={onClose}>取消</Button>
          <Button type="primary" loading={submitting} onClick={onSubmit}>注册</Button>
        </Space>
      }
    >
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message="把 cmd/retrain 输出的 LogisticConfig JSON 整段粘贴到下方，再点 “从 JSON 填表” 自动填入 intercept / weights / Platt 校准参数。"
      />
      {err && <Alert type="error" showIcon closable message="错误" description={err}
        style={{ marginBottom: 16 }} onClose={() => setErr('')} />}
      <Form.Item label="LogisticConfig JSON 粘贴">
        <Input.TextArea
          value={pasteJson}
          onChange={(e) => setPasteJson(e.target.value)}
          autoSize={{ minRows: 4, maxRows: 8 }}
          placeholder='{"ModelVer":"...","Intercept":-2.5,"Weights":{...},"PlattA":1.2,"PlattB":-0.3}'
          style={{ fontFamily: 'ui-monospace, monospace' }}
        />
        <Button onClick={onPasteJSON} style={{ marginTop: 8 }} disabled={!pasteJson}>从 JSON 填表</Button>
      </Form.Item>
      <Form layout="vertical" form={form}>
        <Form.Item name="name" label="Challenger Name" rules={[{ required: true }]}>
          <Input placeholder="e.g. logistic-v2" />
        </Form.Item>
        <Form.Item name="model_ver" label="Model Version">
          <Input placeholder="e.g. v2.0-2026-04" />
        </Form.Item>
        <Form.Item name="intercept" label="Intercept">
          <InputNumber step={0.1} style={{ width: 200 }} />
        </Form.Item>
        <Typography.Text strong>FeatureWeights</Typography.Text>
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
          <Form.Item name="platt_a" label="Platt A (校准)">
            <InputNumber step={0.1} style={{ width: 160 }} />
          </Form.Item>
          <Form.Item name="platt_b" label="Platt B (校准)">
            <InputNumber step={0.1} style={{ width: 160 }} />
          </Form.Item>
        </Space>
      </Form>
    </Drawer>
  )
}

// ── 显著性报告面板（原 ABTest 内容） ───────────────────────────

function ABReport() {
  const [loading, setLoading] = useState(false)
  const [report, setReport] = useState<ChallengerReport[]>([])
  const [minLabeled, setMinLabeled] = useState(30)
  const [bootstrap, setBootstrap] = useState(1000)

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
        message="基于 ABTracker ring buffer（最近 4096 条决策）+ outcome 反馈。需要 outcome 标签 ≥ min_labeled 才输出某 challenger。"
        description="bootstrap CI 全 > 0 → 升级；全 < 0 → 下线；跨 0 → 继续观察直到样本足够。"
      />
      <Space style={{ marginBottom: 16 }} size="large">
        <Space>
          <Typography.Text strong>最小样本</Typography.Text>
          <InputNumber min={5} max={10000} value={minLabeled} onChange={(v) => setMinLabeled(v || 30)} />
        </Space>
        <Space>
          <Typography.Text strong>Bootstrap 次数</Typography.Text>
          <InputNumber min={0} max={10000} step={100} value={bootstrap} onChange={(v) => setBootstrap(v || 0)} />
        </Space>
        <Button onClick={load} loading={loading}>刷新</Button>
      </Space>
      {report.length === 0 ? (
        <Empty description="无 challenger 数据。先去 “模型管理” 注册 challenger，等积累足够 outcome 反馈后回来查看显著性。" />
      ) : (
        <Table<ChallengerReport>
          rowKey="name" size="middle" loading={loading} dataSource={report}
          pagination={false}
          columns={[
            { title: 'Challenger', dataIndex: 'name', width: 180,
              render: (v) => <Typography.Text code strong>{v}</Typography.Text> },
            { title: 'Labeled 样本', dataIndex: 'labeled_samples', width: 110, align: 'right' as const },
            { title: 'Champion AUC', dataIndex: 'champion_auc', width: 130, align: 'right' as const,
              render: (v: number) => v.toFixed(4) },
            { title: 'Challenger AUC', dataIndex: 'challenger_auc', width: 130, align: 'right' as const,
              render: (v: number) => <strong>{v.toFixed(4)}</strong> },
            {
              title: 'AUC Diff', dataIndex: 'auc_diff', width: 110, align: 'right' as const,
              render: (v: number) => {
                const color = v > 0 ? '#52c41a' : v < 0 ? '#cf1322' : undefined
                return <span style={{ color }}>{v >= 0 ? '+' : ''}{v.toFixed(4)}</span>
              },
            },
            {
              title: '95% CI on diff', width: 200,
              render: (_v, r) => {
                if (r.ci_low === 0 && r.ci_high === 0) {
                  return <Typography.Text type="secondary">未计算</Typography.Text>
                }
                return <span>[{r.ci_low.toFixed(4)}, {r.ci_high.toFixed(4)}]</span>
              },
            },
            {
              title: '推荐', dataIndex: 'recommendation', width: 180,
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
          <Statistic title="Challenger 总数" value={report.length} />
          <Statistic title="可升级 (promote)"
            value={report.filter((r) => r.recommendation === 'promote').length}
            valueStyle={{ color: '#52c41a' }} />
          <Statistic title="该下线 (drop)"
            value={report.filter((r) => r.recommendation === 'drop').length}
            valueStyle={{ color: '#cf1322' }} />
          <Statistic title="继续观察 (hold)"
            value={report.filter((r) => r.recommendation === 'hold').length}
            valueStyle={{ color: '#faad14' }} />
        </Space>
      )}
    </Card>
  )
}

// ── ML 降级开关 panel：紧急关 ML / 强制分数 ─────────────────

function MLOverridePanel() {
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
      message.warning('请填写降级原因（≥ 4 字符），会落审计')
      return
    }
    Modal.confirm({
      title: '确认应用 ML 降级？',
      content: (
        <div>
          {disabled
            ? <div><strong>整段 ML 推理将被跳过</strong>，所有交易的 MLScore 用 force_score 或 0。</div>
            : forceScore !== 0
              ? <div>ML 推理结果将被 <strong>force_score={forceScore}</strong> 覆盖。</div>
              : <div>这条配置不会改变行为（disabled=false 且 force_score=0）。</div>}
          <div style={{ marginTop: 8, color: '#888' }}>原因：{reason}</div>
        </div>
      ),
      okButtonProps: { danger: disabled || forceScore !== 0 },
      onOk: async () => {
        try {
          await setMLOverride({ disabled, force_score: forceScore, reason })
          message.success('已应用')
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  const onClear = async () => {
    Modal.confirm({
      title: '清空 ML 降级？',
      content: '将恢复正常 ML 推理路径。',
      onOk: async () => {
        try {
          await clearMLOverride()
          message.success('已清空')
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
        message={status?.is_active ? '⚠️ ML 降级当前已激活' : '正常运行'}
        description={
          status && (
            <Space direction="vertical" size={2}>
              <span>disabled = <strong>{String(status.disabled)}</strong></span>
              <span>force_score = <strong>{status.force_score}</strong></span>
              <span>reason: {status.reason || '-'}</span>
              {status.set_at && (
                <span>由 {status.set_by || 'unknown'} 在 {dayjs(status.set_at).format('YYYY-MM-DD HH:mm:ss')} 设置</span>
              )}
            </Space>
          )
        }
        action={
          status?.is_active && (
            <Button danger size="small" onClick={onClear}>清空 (恢复)</Button>
          )
        }
      />
      <Alert
        type="warning" showIcon style={{ marginBottom: 16 }}
        message="紧急工具：仅用于 ML 推理质量突降 / 误伤大批合法交易 / 调试 ml_threshold 规则。"
        description="disabled=true 跳过整段 ML 推理。force_score 非 0 时即使 ML 跑成功也用强制值覆盖。所有变更立即生效（不重启）+ 落 audit。"
      />
      <Space direction="vertical" size="middle" style={{ width: '100%' }}>
        <Space>
          <Switch
            checked={disabled} onChange={setDisabled}
            checkedChildren="禁用 ML" unCheckedChildren="启用 ML"
          />
          <Typography.Text type={disabled ? 'danger' : 'secondary'}>
            {disabled ? 'ML 推理将被跳过' : 'ML 推理正常运行'}
          </Typography.Text>
        </Space>
        <Space>
          <Typography.Text strong>强制 score</Typography.Text>
          <InputNumber
            min={0} max={1} step={0.01} value={forceScore}
            onChange={(v) => setForceScore(v ?? 0)}
            style={{ width: 160 }}
          />
          <Typography.Text type="secondary">
            0 = 不强制；非 0 时即使 ML 成功也覆盖
          </Typography.Text>
        </Space>
        <div>
          <Typography.Text strong>原因 (落审计)</Typography.Text>
          <Input.TextArea
            value={reason} onChange={(e) => setReason(e.target.value)}
            rows={2} placeholder="e.g. drift 告警 / 误伤升级 / 调试 ml_threshold"
            style={{ marginTop: 4 }}
          />
        </div>
        <Space>
          <Button type="primary" danger={disabled || forceScore !== 0} onClick={onApply}>
            应用降级
          </Button>
          <Button onClick={load} loading={loading}>刷新</Button>
        </Space>
      </Space>
    </Card>
  )
}
