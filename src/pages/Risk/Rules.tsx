// 规则编辑 UI ── /risk/rules
//
// 工作流：
//   1) 列表展示当前 engine 在跑的规则集（id / type / mode / weight / rollout / enabled）
//   2) 点 "编辑" 弹 Drawer，运营可改 name / decision / enabled / mode / weight /
//      rollout / config（raw JSON 编辑器）
//   3) 提交时调 BFF /api/risk/rules/update —— BFF 透传 risk-manage 的 schema
//      校验错误（400 + body.error），UI 展示给运营，避免规则配错落到 engine。
//   4) "新建" / "复制" 走同样的 update 端点（id 不存在 → create）。
//   5) "删除" 调 /api/risk/rules/delete，需要填 reason 落审计。
//   6) "模式" 列直接调 /api/risk/rules/mode 切 enforce ↔ shadow。
//   7) 顶部 Tab 第二页：规则审计日志（rule_audit）— 谁、什么时候、把哪条规则改成了什么。
//
// 注意：本 UI 的修改是 runtime-only；下次 reload yaml 配置会盖掉。要持久化必须改
// configmap 然后 POST /admin/rules/reload。这个 trade-off 写在 Alert 里告知运营。
import { useCallback, useEffect, useState } from 'react'
import { useAuth } from '../../stores/auth'
import {
  Alert, Button, Card, Drawer, Form, Input, InputNumber, Modal, Popconfirm,
  Select, Space, Switch, Table, Tabs, Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import {
  deleteRule, exportRulesYAML, importRulesYAML, listRuleAudit, listRules, ruleInsights,
  ruleOverlap, setRuleMode, simulateRule, updateRule,
} from '../../api/risk'
import type {
  RuleAuditEntry, RuleDef, RuleDiffEntry, RuleImportResult, RuleInsight, RuleOverlapPair,
  RuleSimulateResult,
} from '../../api/risk'

const MODE_COLOR: Record<string, string> = {
  enforce: 'green',
  shadow: 'orange',
}

const ACTION_COLOR: Record<string, string> = {
  create: 'blue',
  update: 'cyan',
  delete: 'red',
  mode_change: 'orange',
  reload: 'purple',
}

interface DrawerState {
  open: boolean
  rule: RuleDef | null
  isNew: boolean
}

export default function Rules() {
  const [loading, setLoading] = useState(false)
  const [rules, setRules] = useState<RuleDef[]>([])
  const [drawer, setDrawer] = useState<DrawerState>({ open: false, rule: null, isNew: false })
  const [importOpen, setImportOpen] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listRules()
      setRules(r.items || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  // 触发浏览器下载当前规则集 YAML
  const onExportYAML = async () => {
    try {
      const yaml = await exportRulesYAML()
      const blob = new Blob([yaml], { type: 'text/yaml' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `risk-rules-${new Date().toISOString().slice(0, 10)}.yaml`
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) { message.error(String(e)) }
  }

  useEffect(() => { load() }, [load])

  const onToggleMode = async (rule: RuleDef, shadow: boolean) => {
    try {
      await setRuleMode({ id: rule.id, shadow })
      message.success(`规则 ${rule.id} → ${shadow ? 'shadow' : 'enforce'}`)
      load()
    } catch (e) { message.error(String(e)) }
  }

  const onDelete = async (rule: RuleDef) => {
    let reason = ''
    Modal.confirm({
      title: `删除规则 ${rule.id}？`,
      content: (
        <div>
          <Alert
            type="warning" showIcon style={{ marginBottom: 12 }}
            message="软停：仅从当前 engine 移除。下次 reload yaml 还会长出来。"
          />
          <Input.TextArea
            rows={3} placeholder="说明删除原因（落审计日志）"
            onChange={(e) => { reason = e.target.value }}
          />
        </div>
      ),
      okText: '确认删除', okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await deleteRule({ id: rule.id, reason })
          message.success('已删除')
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  return (
    <div>
      <Typography.Title level={3}>风控规则管理</Typography.Title>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message="本页操作即时生效（heap-resident）。要持久化请同步改 ConfigMap 后 POST /admin/rules/reload，否则下次重启 / 重载会丢失。"
      />
      <Tabs
        items={[
          {
            key: 'rules', label: '规则列表',
            children: (
              <Card>
                <Space style={{ marginBottom: 16 }}>
                  <Button type="primary" onClick={() => setDrawer({
                    open: true, isNew: true,
                    rule: {
                      id: '', name: '', type: '', decision: 'DENY', enabled: true,
                      mode: 'shadow', weight: 0, config_json: '{}',
                      rollout: { enable_pct: 0, bucket_seed: '' },
                    },
                  })}>新建规则</Button>
                  <Button onClick={load}>刷新</Button>
                  <Button onClick={onExportYAML}>导出 YAML</Button>
                  <Button onClick={() => setImportOpen(true)}>导入 YAML</Button>
                  <Typography.Text type="secondary">{rules.length} 条规则</Typography.Text>
                </Space>
                <Table<RuleDef>
                  rowKey="id" size="small" loading={loading} dataSource={rules}
                  pagination={{ pageSize: 25 }}
                  columns={[
                    { title: 'ID', dataIndex: 'id', width: 220, ellipsis: true,
                      render: (v) => <Typography.Text code>{v}</Typography.Text> },
                    { title: '名称', dataIndex: 'name', width: 200 },
                    { title: 'Type', dataIndex: 'type', width: 140,
                      render: (v) => <Tag>{v}</Tag> },
                    { title: 'Decision', dataIndex: 'decision', width: 110,
                      render: (v: string) => <Tag color={v === 'DENY' ? 'error' : 'warning'}>{v}</Tag> },
                    { title: '权重', dataIndex: 'weight', width: 70, align: 'right' as const },
                    {
                      title: '模式', dataIndex: 'mode', width: 130,
                      render: (mode: string, rec: RuleDef) => (
                        <Space>
                          <Tag color={MODE_COLOR[mode] || 'default'}>{mode || 'enforce'}</Tag>
                          <Switch
                            size="small" checkedChildren="shadow" unCheckedChildren="enforce"
                            checked={mode === 'shadow'}
                            onChange={(checked) => onToggleMode(rec, checked)}
                          />
                        </Space>
                      ),
                    },
                    {
                      title: '灰度 %', width: 90, align: 'right' as const,
                      render: (_v, rec) => rec.rollout?.enable_pct ?? 100,
                    },
                    {
                      title: '启用', dataIndex: 'enabled', width: 70,
                      render: (v: boolean) => v ? <Tag color="success">on</Tag> : <Tag>off</Tag>,
                    },
                    {
                      title: '操作', width: 160,
                      render: (_v, rec) => (
                        <Space>
                          <Button size="small" onClick={() => setDrawer({
                            open: true, isNew: false, rule: { ...rec },
                          })}>编辑</Button>
                          <Popconfirm title="确认删除？" onConfirm={() => onDelete(rec)}>
                            <Button size="small" danger>删除</Button>
                          </Popconfirm>
                        </Space>
                      ),
                    },
                  ]}
                />
              </Card>
            ),
          },
          {
            key: 'insights', label: '规则 KPI',
            children: <RuleInsightsPanel />,
          },
          {
            key: 'overlap', label: '冗余 / 冲突',
            children: <RuleOverlapPanel />,
          },
          {
            key: 'audit', label: '变更审计',
            children: <RuleAuditPanel />,
          },
        ]}
      />
      <RuleEditDrawer
        state={drawer}
        onClose={() => setDrawer({ open: false, rule: null, isNew: false })}
        onSaved={() => { setDrawer({ open: false, rule: null, isNew: false }); load() }}
      />
      <RuleImportDrawer
        open={importOpen}
        onClose={() => setImportOpen(false)}
        onApplied={() => { setImportOpen(false); load() }}
      />
    </div>
  )
}

// ── 编辑 Drawer ───────────────────────────────────────────────

function RuleEditDrawer({
  state, onClose, onSaved,
}: { state: DrawerState; onClose: () => void; onSaved: () => void }) {
  const [form] = Form.useForm<RuleDef>()
  const [submitting, setSubmitting] = useState(false)
  const [simulating, setSimulating] = useState(false)
  const [simResult, setSimResult] = useState<RuleSimulateResult | null>(null)
  const [validateErr, setValidateErr] = useState<string>('')
  const canDanger = useAuth((s) => s.hasRole('danger'))

  useEffect(() => {
    if (state.open && state.rule) {
      form.setFieldsValue(state.rule)
      setValidateErr('')
      setSimResult(null)
    }
  }, [state, form])

  // 把当前表单值拼成 RuleDef 并跑前端 sanity check（合法 JSON）。
  // 失败时把错误塞进 validateErr 并返回 null，让 caller 直接 return。
  const collectAndValidate = async (): Promise<RuleDef | null> => {
    let v: RuleDef
    try {
      v = await form.validateFields()
    } catch { return null }
    if (v.config_json) {
      try { JSON.parse(v.config_json) }
      catch (e) {
        setValidateErr(`config_json 不是合法 JSON: ${e}`)
        return null
      }
    }
    return v
  }

  const onSimulate = async () => {
    const v = await collectAndValidate()
    if (!v) return
    setSimulating(true)
    setValidateErr('')
    setSimResult(null)
    try {
      const r = await simulateRule(v)
      setSimResult(r)
    } catch (e: unknown) {
      const msg = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      setValidateErr(msg)
    } finally {
      setSimulating(false)
    }
  }

  const onSubmit = async () => {
    const v = await collectAndValidate()
    if (!v) return

    setSubmitting(true)
    setValidateErr('')
    try {
      const r = await updateRule(v)
      message.success(`规则 ${v.id} → ${r.action === 'create' ? '已创建' : '已更新'}`)
      onSaved()
    } catch (e: unknown) {
      // BFF 透传了 risk-manage 的 400 响应：{"error": "..."}
      // 把它显示在表单下方而不是 toast，方便运营看清楚到底哪个字段不合法。
      const msg = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      setValidateErr(msg)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Drawer
      open={state.open} onClose={onClose} width={640}
      title={state.isNew ? '新建规则' : `编辑规则 ${state.rule?.id || ''}`}
      extra={
        <Space>
          <Button onClick={onClose}>取消</Button>
          <Button loading={simulating} onClick={onSimulate}>试运行</Button>
          <Button
            type="primary"
            loading={submitting}
            onClick={onSubmit}
            disabled={!canDanger}
            title={!canDanger ? '需要 danger 角色才能改风控规则' : ''}
          >
            保存
          </Button>
        </Space>
      }
    >
      {validateErr && (
        <Alert
          type="error" showIcon closable
          message="校验失败" description={validateErr}
          style={{ marginBottom: 16 }}
          onClose={() => setValidateErr('')}
        />
      )}
      {simResult && (
        <Alert
          type={simResult.hits === 0 ? 'warning' : 'info'}
          showIcon closable
          style={{ marginBottom: 16 }}
          onClose={() => setSimResult(null)}
          message={
            <span>
              试运行结果：重放 <strong>{simResult.sample}</strong> 笔历史决策
              ，候选规则命中 <strong>{simResult.hits}</strong> 笔
              （命中率 <strong>{(simResult.hit_rate * 100).toFixed(2)}%</strong>）
            </span>
          }
          description={
            <div>
              <div>
                新增 BLOCK <strong>{simResult.would_newly_block}</strong> 笔
                ；与现行 BLOCK 一致 <strong>{simResult.would_keep_block}</strong> 笔
              </div>
              <div>
                有 outcome 反馈 <strong>{simResult.hits_with_outcome}</strong>
                ；其中真欺诈 <strong>{simResult.hits_true_fraud}</strong>
                {simResult.estimated_precision > 0 && (
                  <span>
                    ；估算 precision <Tag color={
                      simResult.estimated_precision < 0.3 ? 'red'
                        : simResult.estimated_precision < 0.6 ? 'orange' : 'green'
                    }>{(simResult.estimated_precision * 100).toFixed(1)}%</Tag>
                  </span>
                )}
              </div>
              {simResult.notes && simResult.notes.length > 0 && (
                <ul style={{ marginTop: 8, marginBottom: 0, paddingLeft: 20 }}>
                  {simResult.notes.map((n, i) => <li key={i}>{n}</li>)}
                </ul>
              )}
            </div>
          }
        />
      )}
      <Form<RuleDef> form={form} layout="vertical">
        <Form.Item name="id" label="ID" rules={[{ required: true, message: '必填' }]}>
          <Input disabled={!state.isNew} placeholder="e.g. r_velocity_5m" />
        </Form.Item>
        <Form.Item name="name" label="名称" rules={[{ required: true }]}>
          <Input placeholder="给运营看的描述" />
        </Form.Item>
        <Form.Item name="type" label="类型" rules={[{ required: true }]}>
          <Input placeholder="e.g. velocity / amount / blacklist / mlscore" />
        </Form.Item>
        <Form.Item name="decision" label="决策">
          <Select options={[
            { value: 'DENY', label: 'DENY' },
            { value: 'REVIEW', label: 'REVIEW' },
          ]} />
        </Form.Item>
        <Space size="large">
          <Form.Item name="enabled" label="启用" valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="mode" label="模式">
            <Select style={{ width: 140 }} options={[
              { value: 'enforce', label: 'enforce' },
              { value: 'shadow', label: 'shadow' },
            ]} />
          </Form.Item>
          <Form.Item name="weight" label="权重 (0=按 decision 默认)">
            <InputNumber min={0} max={100} />
          </Form.Item>
        </Space>
        <Form.Item label="灰度">
          <Space>
            <Form.Item name={['rollout', 'enable_pct']} noStyle>
              <InputNumber min={0} max={100} addonAfter="%" />
            </Form.Item>
            <Form.Item name={['rollout', 'bucket_seed']} noStyle>
              <Input placeholder="bucket_seed (空 = 用 rule_id)" style={{ width: 220 }} />
            </Form.Item>
          </Space>
        </Form.Item>
        <Form.Item
          name="config_json" label="config (JSON)"
          extra="规则 type-specific 配置；提交时后端 BuildRule 会跑 schema 校验。"
        >
          <Input.TextArea rows={8} style={{ fontFamily: 'ui-monospace, monospace' }} />
        </Form.Item>
      </Form>
    </Drawer>
  )
}

// ── 审计日志 panel ────────────────────────────────────────────

function RuleAuditPanel() {
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<RuleAuditEntry[]>([])
  const [ruleID, setRuleID] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await listRuleAudit({ rule_id: ruleID || undefined, limit: 200 })
      setRows(r.items || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [ruleID])

  useEffect(() => { load() }, [load])

  return (
    <Card>
      <Space style={{ marginBottom: 16 }}>
        <Input.Search
          placeholder="按 rule_id 过滤" allowClear style={{ width: 280 }}
          onSearch={(v) => setRuleID(v.trim())}
        />
        <Button onClick={load}>刷新</Button>
        <Typography.Text type="secondary">{rows.length} 条变更</Typography.Text>
      </Space>
      <Table<RuleAuditEntry>
        rowKey={(r) => `${r.occurred_at}-${r.rule_id || ''}-${r.action}`}
        size="small" loading={loading} dataSource={rows}
        pagination={{ pageSize: 25 }}
        expandable={{
          expandedRowRender: (r) => (
            <pre style={{
              background: '#fafafa', padding: 12, borderRadius: 4, maxHeight: 300, overflow: 'auto',
            }}>
              {JSON.stringify({ before: r.before, after: r.after, metadata: r.metadata }, null, 2)}
            </pre>
          ),
        }}
        columns={[
          { title: '时间', dataIndex: 'occurred_at', width: 170,
            render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
          { title: '动作', dataIndex: 'action', width: 110,
            render: (v: string) => <Tag color={ACTION_COLOR[v] || 'default'}>{v}</Tag> },
          { title: '操作人', dataIndex: 'actor', width: 140 },
          { title: 'Rule ID', dataIndex: 'rule_id', width: 200, ellipsis: true,
            render: (v) => v ? <Typography.Text code>{v}</Typography.Text> : '-' },
          { title: '原因', dataIndex: 'reason', ellipsis: true },
        ]}
      />
    </Card>
  )
}

// ── 规则 KPI panel：silence / precision / ROI 排行 ──────────

function RuleInsightsPanel() {
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<RuleInsight[]>([])
  const [windowDays, setWindowDays] = useState(7)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await ruleInsights()
      setRows(r.rules || [])
      setWindowDays(r.silence_window_days)
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  useEffect(() => { load() }, [load])

  const silentCount = rows.filter((r) => r.is_silent).length
  const lowPrecCount = rows.filter((r) => r.label_count >= 5 && r.precision < 0.3).length

  return (
    <Card>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={`silence 窗口 ${windowDays} 天；ROI = hits × precision；precision 至少 5 条 outcome 反馈才计算（避免噪音）。`}
      />
      <Space style={{ marginBottom: 16 }} size="large">
        <Typography.Text>
          总规则 <strong>{rows.length}</strong>
        </Typography.Text>
        <Typography.Text type={silentCount > 0 ? 'danger' : 'success'}>
          silent ({windowDays}d 0 hit) <strong>{silentCount}</strong>
        </Typography.Text>
        <Typography.Text type={lowPrecCount > 0 ? 'warning' : 'secondary'}>
          低 precision (&lt;0.3) <strong>{lowPrecCount}</strong>
        </Typography.Text>
        <Button onClick={load}>刷新</Button>
      </Space>
      <Table<RuleInsight>
        rowKey="rule_id" size="small" loading={loading} dataSource={rows}
        pagination={{ pageSize: 50 }}
        columns={[
          {
            title: '规则 ID', dataIndex: 'rule_id', width: 220, ellipsis: true,
            render: (v) => <Typography.Text code>{v}</Typography.Text>,
          },
          {
            title: '状态', dataIndex: 'is_silent', width: 90,
            render: (silent: boolean, rec) => silent
              ? <Tag color="error">silent</Tag>
              : (rec.hits_in_window > 0 ? <Tag color="success">active</Tag> : <Tag>idle</Tag>),
          },
          {
            title: '最近命中', dataIndex: 'last_hit_at', width: 180,
            render: (v: string, rec) => rec.days_since_hit < 0
              ? <Typography.Text type="secondary">从未</Typography.Text>
              : <span>{dayjs(v).format('MM-DD HH:mm')} <Typography.Text type="secondary">（{rec.days_since_hit.toFixed(1)}d 前）</Typography.Text></span>,
          },
          { title: '命中次数', dataIndex: 'hits_in_window', width: 100, align: 'right' as const },
          {
            title: 'Outcome', width: 130, align: 'right' as const,
            render: (_v, rec) => rec.label_count > 0
              ? <span>{rec.fraud_count}/{rec.label_count} fraud</span>
              : <Typography.Text type="secondary">-</Typography.Text>,
          },
          {
            title: 'Precision', dataIndex: 'precision', width: 110, align: 'right' as const,
            render: (v: number, rec) => {
              if (rec.label_count < 5) return <Typography.Text type="secondary">N&lt;5</Typography.Text>
              const color = v < 0.3 ? 'red' : v < 0.6 ? 'orange' : 'green'
              return <Tag color={color}>{(v * 100).toFixed(1)}%</Tag>
            },
          },
          {
            title: 'ROI', dataIndex: 'roi', width: 100, align: 'right' as const,
            render: (v: number) => v > 0
              ? <strong>{v.toFixed(1)}</strong>
              : <Typography.Text type="secondary">-</Typography.Text>,
          },
        ]}
      />
    </Card>
  )
}

// ── 导入 YAML drawer：dry-run 看 diff，确认后真正落地 ────────

const DIFF_COLOR: Record<string, string> = {
  create: 'green',
  update: 'blue',
  delete: 'red',
  unchanged: 'default',
}

function RuleImportDrawer({
  open, onClose, onApplied,
}: { open: boolean; onClose: () => void; onApplied: () => void }) {
  const [yaml, setYaml] = useState('')
  const [dryRunResult, setDryRunResult] = useState<RuleImportResult | null>(null)
  const [applying, setApplying] = useState(false)
  const [err, setErr] = useState('')

  const onDryRun = async () => {
    setErr('')
    setDryRunResult(null)
    try {
      const r = await importRulesYAML(yaml, true)
      setDryRunResult(r)
    } catch (e: unknown) {
      const m = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      setErr(m)
    }
  }

  const onApply = async () => {
    if (!dryRunResult) return
    setApplying(true)
    try {
      await importRulesYAML(yaml, false)
      message.success(`已应用 ${dryRunResult.imported} 条规则`)
      setYaml('')
      setDryRunResult(null)
      onApplied()
    } catch (e: unknown) {
      const m = (e as { response?: { data?: { error?: string } } })?.response?.data?.error
        || String(e)
      message.error(m)
    } finally {
      setApplying(false)
    }
  }

  // 改 YAML 时重置 dry-run 结果（避免拿过期的 diff 应用）
  useEffect(() => { setDryRunResult(null); setErr('') }, [yaml])

  const counts = dryRunResult?.diff.reduce((acc, d) => {
    acc[d.action] = (acc[d.action] || 0) + 1
    return acc
  }, {} as Record<string, number>) || {}

  return (
    <Drawer
      open={open} onClose={onClose} width={760}
      title="导入规则 YAML"
      extra={
        <Space>
          <Button onClick={onClose}>取消</Button>
          <Button onClick={onDryRun} disabled={!yaml}>预览 diff</Button>
          <Button
            type="primary" danger
            disabled={!dryRunResult || dryRunResult.diff.every((d) => d.action === 'unchanged')}
            loading={applying}
            onClick={onApply}
          >
            确认应用
          </Button>
        </Space>
      }
    >
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message="先粘贴 YAML 点 “预览 diff” 校验 + 看变更，确认无误再 “确认应用”。所有规则过 schema 校验全部通过才落地，单条失败 → 整批拒绝。"
      />
      {err && (
        <Alert type="error" showIcon message="校验失败" description={err}
          style={{ marginBottom: 16 }} closable onClose={() => setErr('')} />
      )}
      <Input.TextArea
        value={yaml}
        onChange={(e) => setYaml(e.target.value)}
        placeholder="rules:&#10;  - id: r1&#10;    type: amount_limit&#10;    enabled: true&#10;    config: '{...}'"
        autoSize={{ minRows: 10, maxRows: 20 }}
        style={{ fontFamily: 'ui-monospace, monospace', marginBottom: 16 }}
      />
      {dryRunResult && (
        <>
          <Space style={{ marginBottom: 12 }} size="large">
            <Typography.Text strong>导入 {dryRunResult.imported} 条</Typography.Text>
            {(['create', 'update', 'delete', 'unchanged'] as const).map((a) => (
              <Typography.Text key={a}>
                {a}: <Tag color={DIFF_COLOR[a]}>{counts[a] || 0}</Tag>
              </Typography.Text>
            ))}
          </Space>
          <Table<RuleDiffEntry>
            rowKey={(r) => r.rule_id + r.action}
            size="small"
            dataSource={dryRunResult.diff}
            pagination={false}
            columns={[
              {
                title: 'Rule ID', dataIndex: 'rule_id', width: 200, ellipsis: true,
                render: (v) => <Typography.Text code>{v}</Typography.Text>,
              },
              {
                title: '动作', dataIndex: 'action', width: 100,
                render: (v: string) => <Tag color={DIFF_COLOR[v]}>{v}</Tag>,
              },
              {
                title: '说明', render: (_v, r) => {
                  if (r.action === 'create') return '新规则将被添加'
                  if (r.action === 'delete') return '现有规则将被移除'
                  if (r.action === 'unchanged') return '<Typography.Text type="secondary">无变化</Typography.Text>'
                  return '已存在的规则将被更新'
                },
              },
            ]}
          />
        </>
      )}
    </Drawer>
  )
}

// ── 规则冗余 / 冲突 panel：co-fire 矩阵 ────────────────────

function RuleOverlapPanel() {
  const [loading, setLoading] = useState(false)
  const [pairs, setPairs] = useState<RuleOverlapPair[]>([])
  const [minBoth, setMinBoth] = useState(5)
  const [sample, setSample] = useState(0)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await ruleOverlap({ min_both: minBoth })
      setPairs(r.pairs || [])
      setSample(r.sample)
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [minBoth])

  useEffect(() => { load() }, [load])

  const conflicts = pairs.filter((p) => p.is_conflict).length
  const redundant = pairs.filter((p) => p.a_implies_b > 0.95 || p.b_implies_a > 0.95).length

  return (
    <Card>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={`基于最近 ${sample} 条决策的命中数据。Jaccard > 0.8 = 高度重叠；蕴含率 > 95% = 一条规则是另一条的子集（可能冗余）；verdict 不一致 = 设计冲突。`}
      />
      <Space style={{ marginBottom: 16 }} size="large">
        <Space>
          <Typography.Text strong>最小共同命中</Typography.Text>
          <InputNumber min={1} max={100} value={minBoth} onChange={(v) => setMinBoth(v || 5)} />
        </Space>
        <Typography.Text type={conflicts > 0 ? 'danger' : 'success'}>
          冲突 <strong>{conflicts}</strong>
        </Typography.Text>
        <Typography.Text type={redundant > 0 ? 'warning' : 'secondary'}>
          疑似冗余 <strong>{redundant}</strong>
        </Typography.Text>
        <Typography.Text type="secondary">总对 {pairs.length}</Typography.Text>
        <Button onClick={load} loading={loading}>刷新</Button>
      </Space>
      <Table<RuleOverlapPair>
        rowKey={(r) => `${r.rule_a}::${r.rule_b}`}
        size="small" loading={loading} dataSource={pairs}
        pagination={{ pageSize: 50 }}
        columns={[
          { title: '规则 A', dataIndex: 'rule_a', width: 180, ellipsis: true,
            render: (v, r) => (
              <span>
                <Typography.Text code>{v}</Typography.Text>
                {r.verdict_a && <Tag style={{ marginLeft: 4 }}>{r.verdict_a}</Tag>}
              </span>
            )},
          { title: '规则 B', dataIndex: 'rule_b', width: 180, ellipsis: true,
            render: (v, r) => (
              <span>
                <Typography.Text code>{v}</Typography.Text>
                {r.verdict_b && <Tag style={{ marginLeft: 4 }}>{r.verdict_b}</Tag>}
              </span>
            )},
          { title: 'A 命中', dataIndex: 'hits_a', width: 80, align: 'right' as const },
          { title: 'B 命中', dataIndex: 'hits_b', width: 80, align: 'right' as const },
          { title: '同时', dataIndex: 'both', width: 70, align: 'right' as const },
          {
            title: 'Jaccard', dataIndex: 'jaccard', width: 100, align: 'right' as const,
            sorter: (a, b) => a.jaccard - b.jaccard,
            render: (v: number) => {
              const color = v >= 0.8 ? 'red' : v >= 0.5 ? 'orange' : undefined
              return color
                ? <Tag color={color}>{(v * 100).toFixed(1)}%</Tag>
                : (v * 100).toFixed(1) + '%'
            },
          },
          {
            title: 'A→B', dataIndex: 'a_implies_b', width: 90, align: 'right' as const,
            render: (v: number) => v >= 0.95
              ? <Tag color="orange">{(v * 100).toFixed(1)}%</Tag>
              : (v * 100).toFixed(1) + '%',
          },
          {
            title: 'B→A', dataIndex: 'b_implies_a', width: 90, align: 'right' as const,
            render: (v: number) => v >= 0.95
              ? <Tag color="orange">{(v * 100).toFixed(1)}%</Tag>
              : (v * 100).toFixed(1) + '%',
          },
          {
            title: '诊断', width: 200,
            render: (_v, r) => {
              const tags = []
              if (r.is_conflict) {
                tags.push(<Tag key="c" color="red">verdict 冲突</Tag>)
              }
              if (r.a_implies_b >= 0.95 && r.b_implies_a >= 0.95) {
                tags.push(<Tag key="dup" color="purple">两规则等价</Tag>)
              } else if (r.a_implies_b >= 0.95) {
                tags.push(<Tag key="ab" color="orange">A 蕴含 B</Tag>)
              } else if (r.b_implies_a >= 0.95) {
                tags.push(<Tag key="ba" color="orange">B 蕴含 A</Tag>)
              } else if (r.jaccard >= 0.8) {
                tags.push(<Tag key="j" color="orange">高度重叠</Tag>)
              }
              return tags.length > 0 ? <Space size={4}>{tags}</Space>
                : <Typography.Text type="secondary">-</Typography.Text>
            },
          },
        ]}
      />
    </Card>
  )
}
