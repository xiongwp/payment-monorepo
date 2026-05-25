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
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('risk')
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
      message.success(t('rules.modeSwitched', { id: rule.id, mode: shadow ? 'shadow' : 'enforce' }))
      load()
    } catch (e) { message.error(String(e)) }
  }

  const onDelete = async (rule: RuleDef) => {
    let reason = ''
    Modal.confirm({
      title: t('rules.deleteModal.title', { id: rule.id }),
      content: (
        <div>
          <Alert
            type="warning" showIcon style={{ marginBottom: 12 }}
            message={t('rules.deleteModal.warning')}
          />
          <Input.TextArea
            rows={3} placeholder={t('rules.deleteModal.reasonPlaceholder')}
            onChange={(e) => { reason = e.target.value }}
          />
        </div>
      ),
      okText: t('rules.deleteModal.okText'), okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await deleteRule({ id: rule.id, reason })
          message.success(t('rules.deleted'))
          load()
        } catch (e) { message.error(String(e)) }
      },
    })
  }

  return (
    <div>
      <Typography.Title level={3}>{t('rules.title')}</Typography.Title>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('rules.topAlert')}
      />
      <Tabs
        items={[
          {
            key: 'rules', label: t('rules.tabs.rules'),
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
                  })}>{t('rules.createButton')}</Button>
                  <Button onClick={load}>{t('common:actions.refresh')}</Button>
                  <Button onClick={onExportYAML}>{t('rules.exportYaml')}</Button>
                  <Button onClick={() => setImportOpen(true)}>{t('rules.importYaml')}</Button>
                  <Typography.Text type="secondary">{t('rules.ruleCountSuffix', { count: rules.length })}</Typography.Text>
                </Space>
                <Table<RuleDef>
                  rowKey="id" size="small" loading={loading} dataSource={rules}
                  pagination={{ pageSize: 25 }}
                  columns={[
                    { title: t('rules.columns.id'), dataIndex: 'id', width: 220, ellipsis: true,
                      render: (v) => <Typography.Text code>{v}</Typography.Text> },
                    { title: t('rules.columns.name'), dataIndex: 'name', width: 200 },
                    { title: t('rules.columns.type'), dataIndex: 'type', width: 140,
                      render: (v) => <Tag>{v}</Tag> },
                    { title: t('rules.columns.decision'), dataIndex: 'decision', width: 110,
                      render: (v: string) => <Tag color={v === 'DENY' ? 'error' : 'warning'}>{v}</Tag> },
                    { title: t('rules.columns.weight'), dataIndex: 'weight', width: 70, align: 'right' as const },
                    {
                      title: t('rules.columns.mode'), dataIndex: 'mode', width: 130,
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
                      title: t('rules.columns.rollout'), width: 90, align: 'right' as const,
                      render: (_v, rec) => rec.rollout?.enable_pct ?? 100,
                    },
                    {
                      title: t('rules.columns.enabled'), dataIndex: 'enabled', width: 70,
                      render: (v: boolean) => v ? <Tag color="success">{t('rules.tag.on')}</Tag> : <Tag>{t('rules.tag.off')}</Tag>,
                    },
                    {
                      title: t('rules.columns.actions'), width: 160,
                      render: (_v, rec) => (
                        <Space>
                          <Button size="small" onClick={() => setDrawer({
                            open: true, isNew: false, rule: { ...rec },
                          })}>{t('common:actions.edit')}</Button>
                          <Popconfirm title={t('rules.popconfirmDelete')} onConfirm={() => onDelete(rec)}>
                            <Button size="small" danger>{t('common:actions.delete')}</Button>
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
            key: 'insights', label: t('rules.tabs.insights'),
            children: <RuleInsightsPanel />,
          },
          {
            key: 'overlap', label: t('rules.tabs.overlap'),
            children: <RuleOverlapPanel />,
          },
          {
            key: 'audit', label: t('rules.tabs.audit'),
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
  const { t } = useTranslation('risk')
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
        setValidateErr(t('rules.editDrawer.configInvalid', { error: String(e) }))
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
      message.success(t('rules.editDrawer.saveSuccess', {
        id: v.id,
        action: r.action === 'create' ? t('rules.editDrawer.createAction') : t('rules.editDrawer.updateAction'),
      }))
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
      title={state.isNew ? t('rules.editDrawer.createTitle') : t('rules.editDrawer.editTitle', { id: state.rule?.id || '' })}
      extra={
        <Space>
          <Button onClick={onClose}>{t('common:actions.cancel')}</Button>
          <Button loading={simulating} onClick={onSimulate}>{t('rules.editDrawer.simulateButton')}</Button>
          <Button
            type="primary"
            loading={submitting}
            onClick={onSubmit}
            disabled={!canDanger}
            title={!canDanger ? t('rules.editDrawer.saveDisabledTitle') : ''}
          >
            {t('rules.editDrawer.saveButton')}
          </Button>
        </Space>
      }
    >
      {validateErr && (
        <Alert
          type="error" showIcon closable
          message={t('rules.editDrawer.validateErrorTitle')} description={validateErr}
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
              {t('rules.editDrawer.simulate.messagePart1')}<strong>{simResult.sample}</strong>
              {t('rules.editDrawer.simulate.messagePart2')}<strong>{simResult.hits}</strong>
              {t('rules.editDrawer.simulate.messagePart3')}<strong>{(simResult.hit_rate * 100).toFixed(2)}%</strong>
              {t('rules.editDrawer.simulate.messagePart4')}
            </span>
          }
          description={
            <div>
              <div>
                {t('rules.editDrawer.simulate.newBlockPrefix')}<strong>{simResult.would_newly_block}</strong>
                {t('rules.editDrawer.simulate.newBlockMid')}<strong>{simResult.would_keep_block}</strong>
                {t('rules.editDrawer.simulate.newBlockSuffix')}
              </div>
              <div>
                {t('rules.editDrawer.simulate.outcomePrefix')}<strong>{simResult.hits_with_outcome}</strong>
                {t('rules.editDrawer.simulate.outcomeMid')}<strong>{simResult.hits_true_fraud}</strong>
                {simResult.estimated_precision > 0 && (
                  <span>
                    {t('rules.editDrawer.simulate.precisionPrefix')}<Tag color={
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
        <Form.Item name="id" label={t('rules.editDrawer.fields.id')} rules={[{ required: true, message: t('rules.editDrawer.fields.idRequired') }]}>
          <Input disabled={!state.isNew} placeholder="e.g. r_velocity_5m" />
        </Form.Item>
        <Form.Item name="name" label={t('rules.editDrawer.fields.name')} rules={[{ required: true }]}>
          <Input placeholder={t('rules.editDrawer.fields.namePlaceholder')} />
        </Form.Item>
        <Form.Item name="type" label={t('rules.editDrawer.fields.type')} rules={[{ required: true }]}>
          <Input placeholder={t('rules.editDrawer.fields.typePlaceholder')} />
        </Form.Item>
        <Form.Item name="decision" label={t('rules.editDrawer.fields.decision')}>
          <Select options={[
            { value: 'DENY', label: 'DENY' },
            { value: 'REVIEW', label: 'REVIEW' },
          ]} />
        </Form.Item>
        <Space size="large">
          <Form.Item name="enabled" label={t('rules.editDrawer.fields.enabled')} valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="mode" label={t('rules.editDrawer.fields.mode')}>
            <Select style={{ width: 140 }} options={[
              { value: 'enforce', label: 'enforce' },
              { value: 'shadow', label: 'shadow' },
            ]} />
          </Form.Item>
          <Form.Item name="weight" label={t('rules.editDrawer.fields.weight')}>
            <InputNumber min={0} max={100} />
          </Form.Item>
        </Space>
        <Form.Item label={t('rules.editDrawer.fields.rollout')}>
          <Space>
            <Form.Item name={['rollout', 'enable_pct']} noStyle>
              <InputNumber min={0} max={100} addonAfter="%" />
            </Form.Item>
            <Form.Item name={['rollout', 'bucket_seed']} noStyle>
              <Input placeholder={t('rules.editDrawer.fields.bucketSeedPlaceholder')} style={{ width: 220 }} />
            </Form.Item>
          </Space>
        </Form.Item>
        <Form.Item
          name="config_json" label={t('rules.editDrawer.fields.configLabel')}
          extra={t('rules.editDrawer.fields.configExtra')}
        >
          <Input.TextArea rows={8} style={{ fontFamily: 'ui-monospace, monospace' }} />
        </Form.Item>
      </Form>
    </Drawer>
  )
}

// ── 审计日志 panel ────────────────────────────────────────────

function RuleAuditPanel() {
  const { t } = useTranslation('risk')
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
          placeholder={t('rules.audit.filterPlaceholder')} allowClear style={{ width: 280 }}
          onSearch={(v) => setRuleID(v.trim())}
        />
        <Button onClick={load}>{t('common:actions.refresh')}</Button>
        <Typography.Text type="secondary">{t('rules.audit.totalSuffix', { count: rows.length })}</Typography.Text>
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
          { title: t('rules.audit.columns.time'), dataIndex: 'occurred_at', width: 170,
            render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
          { title: t('rules.audit.columns.action'), dataIndex: 'action', width: 110,
            render: (v: string) => <Tag color={ACTION_COLOR[v] || 'default'}>{v}</Tag> },
          { title: t('rules.audit.columns.actor'), dataIndex: 'actor', width: 140 },
          { title: t('rules.audit.columns.ruleId'), dataIndex: 'rule_id', width: 200, ellipsis: true,
            render: (v) => v ? <Typography.Text code>{v}</Typography.Text> : '-' },
          { title: t('rules.audit.columns.reason'), dataIndex: 'reason', ellipsis: true },
        ]}
      />
    </Card>
  )
}

// ── 规则 KPI panel：silence / precision / ROI 排行 ──────────

function RuleInsightsPanel() {
  const { t } = useTranslation('risk')
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
        message={t('rules.insights.alert', { days: windowDays })}
      />
      <Space style={{ marginBottom: 16 }} size="large">
        <Typography.Text>
          {t('rules.insights.totalRules')}<strong>{rows.length}</strong>
        </Typography.Text>
        <Typography.Text type={silentCount > 0 ? 'danger' : 'success'}>
          {t('rules.insights.silentLabel', { days: windowDays })}<strong>{silentCount}</strong>
        </Typography.Text>
        <Typography.Text type={lowPrecCount > 0 ? 'warning' : 'secondary'}>
          {t('rules.insights.lowPrecisionLabel')}<strong>{lowPrecCount}</strong>
        </Typography.Text>
        <Button onClick={load}>{t('common:actions.refresh')}</Button>
      </Space>
      <Table<RuleInsight>
        rowKey="rule_id" size="small" loading={loading} dataSource={rows}
        pagination={{ pageSize: 50 }}
        columns={[
          {
            title: t('rules.insights.columns.ruleId'), dataIndex: 'rule_id', width: 220, ellipsis: true,
            render: (v) => <Typography.Text code>{v}</Typography.Text>,
          },
          {
            title: t('rules.insights.columns.status'), dataIndex: 'is_silent', width: 90,
            render: (silent: boolean, rec) => silent
              ? <Tag color="error">{t('rules.insights.statusTags.silent')}</Tag>
              : (rec.hits_in_window > 0 ? <Tag color="success">{t('rules.insights.statusTags.active')}</Tag> : <Tag>{t('rules.insights.statusTags.idle')}</Tag>),
          },
          {
            title: t('rules.insights.columns.lastHit'), dataIndex: 'last_hit_at', width: 180,
            render: (v: string, rec) => rec.days_since_hit < 0
              ? <Typography.Text type="secondary">{t('rules.insights.neverHit')}</Typography.Text>
              : <span>{dayjs(v).format('MM-DD HH:mm')} <Typography.Text type="secondary">{t('rules.insights.daysAgo', { days: rec.days_since_hit.toFixed(1) })}</Typography.Text></span>,
          },
          { title: t('rules.insights.columns.hits'), dataIndex: 'hits_in_window', width: 100, align: 'right' as const },
          {
            title: t('rules.insights.columns.outcome'), width: 130, align: 'right' as const,
            render: (_v, rec) => rec.label_count > 0
              ? <span>{t('rules.insights.fraudCount', { fraud: rec.fraud_count, total: rec.label_count })}</span>
              : <Typography.Text type="secondary">-</Typography.Text>,
          },
          {
            title: t('rules.insights.columns.precision'), dataIndex: 'precision', width: 110, align: 'right' as const,
            render: (v: number, rec) => {
              if (rec.label_count < 5) return <Typography.Text type="secondary">{t('rules.insights.nLessThan5')}</Typography.Text>
              const color = v < 0.3 ? 'red' : v < 0.6 ? 'orange' : 'green'
              return <Tag color={color}>{(v * 100).toFixed(1)}%</Tag>
            },
          },
          {
            title: t('rules.insights.columns.roi'), dataIndex: 'roi', width: 100, align: 'right' as const,
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
  const { t } = useTranslation('risk')
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
      message.success(t('rules.importDrawer.appliedSuccess', { count: dryRunResult.imported }))
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
      title={t('rules.importDrawer.title')}
      extra={
        <Space>
          <Button onClick={onClose}>{t('common:actions.cancel')}</Button>
          <Button onClick={onDryRun} disabled={!yaml}>{t('rules.importDrawer.previewButton')}</Button>
          <Button
            type="primary" danger
            disabled={!dryRunResult || dryRunResult.diff.every((d) => d.action === 'unchanged')}
            loading={applying}
            onClick={onApply}
          >
            {t('rules.importDrawer.applyButton')}
          </Button>
        </Space>
      }
    >
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('rules.importDrawer.infoAlert')}
      />
      {err && (
        <Alert type="error" showIcon message={t('rules.importDrawer.validateFailed')} description={err}
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
            <Typography.Text strong>{t('rules.importDrawer.importedPrefix', { count: dryRunResult.imported })}</Typography.Text>
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
                title: t('rules.importDrawer.columns.ruleId'), dataIndex: 'rule_id', width: 200, ellipsis: true,
                render: (v) => <Typography.Text code>{v}</Typography.Text>,
              },
              {
                title: t('rules.importDrawer.columns.action'), dataIndex: 'action', width: 100,
                render: (v: string) => <Tag color={DIFF_COLOR[v]}>{v}</Tag>,
              },
              {
                title: t('rules.importDrawer.columns.description'), render: (_v, r) => {
                  if (r.action === 'create') return t('rules.importDrawer.actionDesc.create')
                  if (r.action === 'delete') return t('rules.importDrawer.actionDesc.delete')
                  if (r.action === 'unchanged') return t('rules.importDrawer.actionDesc.unchanged')
                  return t('rules.importDrawer.actionDesc.update')
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
  const { t } = useTranslation('risk')
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
        message={t('rules.overlap.alert', { sample })}
      />
      <Space style={{ marginBottom: 16 }} size="large">
        <Space>
          <Typography.Text strong>{t('rules.overlap.minBothLabel')}</Typography.Text>
          <InputNumber min={1} max={100} value={minBoth} onChange={(v) => setMinBoth(v || 5)} />
        </Space>
        <Typography.Text type={conflicts > 0 ? 'danger' : 'success'}>
          {t('rules.overlap.conflictLabel')}<strong>{conflicts}</strong>
        </Typography.Text>
        <Typography.Text type={redundant > 0 ? 'warning' : 'secondary'}>
          {t('rules.overlap.redundantLabel')}<strong>{redundant}</strong>
        </Typography.Text>
        <Typography.Text type="secondary">{t('rules.overlap.totalPairs', { count: pairs.length })}</Typography.Text>
        <Button onClick={load} loading={loading}>{t('common:actions.refresh')}</Button>
      </Space>
      <Table<RuleOverlapPair>
        rowKey={(r) => `${r.rule_a}::${r.rule_b}`}
        size="small" loading={loading} dataSource={pairs}
        pagination={{ pageSize: 50 }}
        columns={[
          { title: t('rules.overlap.columns.ruleA'), dataIndex: 'rule_a', width: 180, ellipsis: true,
            render: (v, r) => (
              <span>
                <Typography.Text code>{v}</Typography.Text>
                {r.verdict_a && <Tag style={{ marginLeft: 4 }}>{r.verdict_a}</Tag>}
              </span>
            )},
          { title: t('rules.overlap.columns.ruleB'), dataIndex: 'rule_b', width: 180, ellipsis: true,
            render: (v, r) => (
              <span>
                <Typography.Text code>{v}</Typography.Text>
                {r.verdict_b && <Tag style={{ marginLeft: 4 }}>{r.verdict_b}</Tag>}
              </span>
            )},
          { title: t('rules.overlap.columns.hitsA'), dataIndex: 'hits_a', width: 80, align: 'right' as const },
          { title: t('rules.overlap.columns.hitsB'), dataIndex: 'hits_b', width: 80, align: 'right' as const },
          { title: t('rules.overlap.columns.both'), dataIndex: 'both', width: 70, align: 'right' as const },
          {
            title: t('rules.overlap.columns.jaccard'), dataIndex: 'jaccard', width: 100, align: 'right' as const,
            sorter: (a, b) => a.jaccard - b.jaccard,
            render: (v: number) => {
              const color = v >= 0.8 ? 'red' : v >= 0.5 ? 'orange' : undefined
              return color
                ? <Tag color={color}>{(v * 100).toFixed(1)}%</Tag>
                : (v * 100).toFixed(1) + '%'
            },
          },
          {
            title: t('rules.overlap.columns.aImpliesB'), dataIndex: 'a_implies_b', width: 90, align: 'right' as const,
            render: (v: number) => v >= 0.95
              ? <Tag color="orange">{(v * 100).toFixed(1)}%</Tag>
              : (v * 100).toFixed(1) + '%',
          },
          {
            title: t('rules.overlap.columns.bImpliesA'), dataIndex: 'b_implies_a', width: 90, align: 'right' as const,
            render: (v: number) => v >= 0.95
              ? <Tag color="orange">{(v * 100).toFixed(1)}%</Tag>
              : (v * 100).toFixed(1) + '%',
          },
          {
            title: t('rules.overlap.columns.diagnosis'), width: 200,
            render: (_v, r) => {
              const tags = []
              if (r.is_conflict) {
                tags.push(<Tag key="c" color="red">{t('rules.overlap.diagnosisTags.verdictConflict')}</Tag>)
              }
              if (r.a_implies_b >= 0.95 && r.b_implies_a >= 0.95) {
                tags.push(<Tag key="dup" color="purple">{t('rules.overlap.diagnosisTags.equivalent')}</Tag>)
              } else if (r.a_implies_b >= 0.95) {
                tags.push(<Tag key="ab" color="orange">{t('rules.overlap.diagnosisTags.aImpliesB')}</Tag>)
              } else if (r.b_implies_a >= 0.95) {
                tags.push(<Tag key="ba" color="orange">{t('rules.overlap.diagnosisTags.bImpliesA')}</Tag>)
              } else if (r.jaccard >= 0.8) {
                tags.push(<Tag key="j" color="orange">{t('rules.overlap.diagnosisTags.highOverlap')}</Tag>)
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
