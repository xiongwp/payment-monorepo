import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Col, Form, Input, Row, Space, Statistic, Switch, Table,
  Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { useTranslation } from 'react-i18next'
import {
  dashboardSummary, listDecisions, recentOutcomes, listReviews, recallStats, setRuleMode,
} from '../../api/risk'
import type {
  DashboardSummary, DecisionRow, Outcome, RecallStats, ReviewItem,
} from '../../api/risk'

const VERDICT_COLOR: Record<string, string> = {
  ALLOW: 'success', REVIEW: 'warning', DENY: 'error',
}

export default function Dashboard() {
  const { t } = useTranslation('risk')
  const [summary, setSummary] = useState<DashboardSummary | null>(null)
  const [recall, setRecall] = useState<RecallStats | null>(null)
  const [decisions, setDecisions] = useState<DecisionRow[]>([])
  const [outcomes, setOutcomes] = useState<Outcome[]>([])
  const [pending, setPending] = useState<ReviewItem[]>([])
  const [loading, setLoading] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [s, rec, d, o, p] = await Promise.all([
        dashboardSummary().catch(() => null), // summary 失败不阻塞其它面板
        recallStats().catch(() => null),
        listDecisions(500),
        recentOutcomes(200),
        listReviews({ status: 'pending', limit: 200 }),
      ])
      setSummary(s)
      setRecall(rec)
      setDecisions(d.items || [])
      setOutcomes(o.items || [])
      setPending(p.items || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  // 规则预测试（enforce ↔ shadow 切换）
  const [ruleForm] = Form.useForm<{ id: string; shadow: boolean }>()
  const onToggleRule = async (v: { id: string; shadow: boolean }) => {
    try {
      const r = await setRuleMode({ id: v.id, shadow: v.shadow })
      message.success(t('dashboard.ruleSwitched', { id: r.id, mode: r.mode }))
    } catch (e) { message.error(String(e)) }
  }

  useEffect(() => { load() }, [load])

  // 简单聚合（最近 500 条决策的 verdict 分布）
  const verdictDist = decisions.reduce((acc, d) => {
    acc[d.verdict] = (acc[d.verdict] || 0) + 1
    return acc
  }, {} as Record<string, number>)
  const total = decisions.length || 1
  const allowRate  = ((verdictDist.ALLOW  || 0) / total * 100).toFixed(1)
  const reviewRate = ((verdictDist.REVIEW || 0) / total * 100).toFixed(1)
  const denyRate   = ((verdictDist.DENY   || 0) / total * 100).toFixed(1)

  // outcome 标签统计
  const fraudCount  = outcomes.filter((o) => o.is_fraud).length
  const legitCount  = outcomes.length - fraudCount
  const fraudRate   = outcomes.length ? (fraudCount / outcomes.length * 100).toFixed(1) : '0.0'

  // 最近 5 条 deny 决策
  const recentDeny = decisions.filter((d) => d.verdict === 'DENY').slice(0, 5)

  return (
    <div>
      <Typography.Title level={3}>{t('dashboard.title')}</Typography.Title>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message={t('dashboard.dataSourceAlert')}
      />

      {summary && (
        <Card title={t('dashboard.systemStatus')} size="small" style={{ marginBottom: 16 }}>
          <Row gutter={[16, 16]}>
            <Col xs={12} sm={6} md={4}>
              <Statistic title={t('dashboard.ruleCount')} value={summary.rule_count} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title={t('dashboard.queuePending')} value={summary.queue.pending} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title={t('dashboard.queueInReview')} value={summary.queue.in_review}
                valueStyle={{ color: '#1677ff' }}
              />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title={t('dashboard.queueEscalated')} value={summary.queue.escalated}
                valueStyle={{ color: '#cf1322' }}
              />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title={t('dashboard.queueOverdue')}
                value={summary.queue.overdue}
                valueStyle={{ color: summary.queue.overdue > 0 ? '#cf1322' : undefined }}
              />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Space direction="vertical" size={2}>
                <Typography.Text type="secondary">{t('dashboard.championLabel')}</Typography.Text>
                <Tag color="green">{summary.champion_model || 'noop'}</Tag>
                {summary.challenger_models?.length ? (
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                    {t('dashboard.challengersPrefix', { names: summary.challenger_models.join(', ') })}
                  </Typography.Text>
                ) : null}
              </Space>
            </Col>
          </Row>
        </Card>
      )}

      {recall && recall.labeled_samples > 0 && (
        <Card
          title={t('dashboard.recallTitle', { days: recall.window_days, samples: recall.labeled_samples })}
          size="small" style={{ marginBottom: 16 }}
        >
          <Row gutter={[16, 16]}>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title={t('dashboard.precision')}
                value={(recall.precision_at_block * 100).toFixed(1)} suffix="%"
                valueStyle={{ color: '#3f8600' }}
              />
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                {t('dashboard.precisionHint')}
              </Typography.Text>
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title={t('dashboard.recall')}
                value={(recall.recall_at_block * 100).toFixed(1)} suffix="%"
                valueStyle={{ color: recall.recall_at_block < 0.7 ? '#cf1322' : '#3f8600' }}
              />
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                {t('dashboard.recallHint')}
              </Typography.Text>
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title={t('dashboard.fpr')}
                value={(recall.false_positive_rate * 100).toFixed(2)} suffix="%"
                valueStyle={{ color: recall.false_positive_rate > 0.05 ? '#cf1322' : undefined }}
              />
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                {t('dashboard.fprHint')}
              </Typography.Text>
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title={t('dashboard.f1')} value={recall.f1_score.toFixed(3)} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title={t('dashboard.actualFraud')} value={recall.actual_fraud} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title={t('dashboard.actualLegit')} value={recall.actual_legit} />
            </Col>
          </Row>
        </Card>
      )}

      <Card title={t('dashboard.rulePretestTitle')} size="small" style={{ marginBottom: 16 }}>
        <Alert
          type="info" showIcon style={{ marginBottom: 12 }}
          message={t('dashboard.rulePretestAlert')}
        />
        <Form
          form={ruleForm} layout="inline" onFinish={onToggleRule}
          initialValues={{ shadow: true }}
        >
          <Form.Item label={t('dashboard.ruleIdLabel')} name="id" rules={[{ required: true }]}>
            <Input placeholder="reg_ip_10min_5" style={{ width: 240 }} />
          </Form.Item>
          <Form.Item label={t('dashboard.shadowLabel')} name="shadow" valuePropName="checked">
            <Switch checkedChildren="shadow" unCheckedChildren="enforce" />
          </Form.Item>
          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit">{t('dashboard.applyButton')}</Button>
              <Typography.Text type="secondary">
                {t('dashboard.shadowHint')}
              </Typography.Text>
            </Space>
          </Form.Item>
        </Form>
      </Card>

      <Row gutter={[16, 16]}>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}><Statistic title={t('dashboard.pendingReview')} value={pending.length} suffix={t('dashboard.unitItem')} /></Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.allowRate')} value={allowRate} suffix="%" valueStyle={{ color: '#3f8600' }} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.reviewRate')} value={reviewRate} suffix="%" valueStyle={{ color: '#faad14' }} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.denyRate')} value={denyRate} suffix="%" valueStyle={{ color: '#cf1322' }} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.feedbackSamples')} value={outcomes.length} suffix={t('dashboard.unitItem')} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.markedFraud')} value={fraudCount} suffix={`/ ${outcomes.length}`} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.markedLegit')} value={legitCount} suffix={`/ ${outcomes.length}`} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title={t('dashboard.fraudRate')} value={fraudRate} suffix="%" valueStyle={{ color: '#cf1322' }} />
          </Card>
        </Col>
      </Row>

      <Card title={t('dashboard.recentDenyTitle')} style={{ marginTop: 16 }} loading={loading}>
        <Table<DecisionRow>
          rowKey="decision_id" size="small" pagination={false} dataSource={recentDeny}
          locale={{ emptyText: t('dashboard.recentDenyEmpty') }}
          columns={[
            {
              title: t('common.decisionId'), dataIndex: 'decision_id', width: 240, ellipsis: true,
              render: (v) => <Typography.Text code>{v}</Typography.Text>,
            },
            {
              title: t('common.time'), dataIndex: 'occurred_at', width: 170,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss'),
            },
            {
              title: t('common.verdict'), dataIndex: 'verdict', width: 80,
              render: (v: string) => <Tag color={VERDICT_COLOR[v] || 'default'}>{v}</Tag>,
            },
            { title: t('common.riskScore'), dataIndex: 'risk_score', width: 80, align: 'center' },
            {
              title: t('common.hitRules'), dataIndex: 'hit_rules',
              render: (vs: string[]) => (
                <>{(vs || []).map((s, i) => <Tag key={i}>{s}</Tag>)}</>
              ),
            },
          ]}
        />
      </Card>
    </div>
  )
}
