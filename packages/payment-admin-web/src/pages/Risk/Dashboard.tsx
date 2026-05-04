import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Col, Form, Input, Row, Space, Statistic, Switch, Table,
  Tag, Typography, message,
} from 'antd'
import dayjs from 'dayjs'
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
      message.success(`规则 ${r.id} 切到 ${r.mode}`)
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
      <Typography.Title level={3}>风控运营工作台</Typography.Title>
      <Alert
        type="info" showIcon style={{ marginBottom: 16 }}
        message="数据来源：risk-manage 进程内 ring buffer（最近 500 条决策 / 200 条 outcome）。生产应配 ClickHouse + Grafana 看长期 SLA。"
      />

      {summary && (
        <Card title="系统状态" size="small" style={{ marginBottom: 16 }}>
          <Row gutter={[16, 16]}>
            <Col xs={12} sm={6} md={4}>
              <Statistic title="规则数" value={summary.rule_count} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title="待领取" value={summary.queue.pending} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title="审核中" value={summary.queue.in_review}
                valueStyle={{ color: '#1677ff' }}
              />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title="已升级" value={summary.queue.escalated}
                valueStyle={{ color: '#cf1322' }}
              />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title="超期未决"
                value={summary.queue.overdue}
                valueStyle={{ color: summary.queue.overdue > 0 ? '#cf1322' : undefined }}
              />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Space direction="vertical" size={2}>
                <Typography.Text type="secondary">ML champion</Typography.Text>
                <Tag color="green">{summary.champion_model || 'noop'}</Tag>
                {summary.challenger_models?.length ? (
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                    challengers: {summary.challenger_models.join(', ')}
                  </Typography.Text>
                ) : null}
              </Space>
            </Col>
          </Row>
        </Card>
      )}

      {recall && recall.labeled_samples > 0 && (
        <Card
          title={`系统召回 / 准确率（最近 ${recall.window_days} 天，${recall.labeled_samples} 条已 label 样本）`}
          size="small" style={{ marginBottom: 16 }}
        >
          <Row gutter={[16, 16]}>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title="Precision"
                value={(recall.precision_at_block * 100).toFixed(1)} suffix="%"
                valueStyle={{ color: '#3f8600' }}
              />
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                BLOCK 中真欺诈
              </Typography.Text>
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title="Recall"
                value={(recall.recall_at_block * 100).toFixed(1)} suffix="%"
                valueStyle={{ color: recall.recall_at_block < 0.7 ? '#cf1322' : '#3f8600' }}
              />
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                真欺诈被抓到
              </Typography.Text>
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic
                title="FPR"
                value={(recall.false_positive_rate * 100).toFixed(2)} suffix="%"
                valueStyle={{ color: recall.false_positive_rate > 0.05 ? '#cf1322' : undefined }}
              />
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                正常被误杀
              </Typography.Text>
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title="F1" value={recall.f1_score.toFixed(3)} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title="实际欺诈" value={recall.actual_fraud} />
            </Col>
            <Col xs={12} sm={6} md={4}>
              <Statistic title="实际正常" value={recall.actual_legit} />
            </Col>
          </Row>
        </Card>
      )}

      <Card title="规则预测试（mode 切换）" size="small" style={{ marginBottom: 16 }}>
        <Alert
          type="info" showIcon style={{ marginBottom: 12 }}
          message="新规则上线 SOP：先 mode=shadow 跑 N 天观察 shadow_hits → 对照真值 → 切 enforce。本切换不重启。"
        />
        <Form
          form={ruleForm} layout="inline" onFinish={onToggleRule}
          initialValues={{ shadow: true }}
        >
          <Form.Item label="规则 ID" name="id" rules={[{ required: true }]}>
            <Input placeholder="reg_ip_10min_5" style={{ width: 240 }} />
          </Form.Item>
          <Form.Item label="Shadow" name="shadow" valuePropName="checked">
            <Switch checkedChildren="shadow" unCheckedChildren="enforce" />
          </Form.Item>
          <Form.Item>
            <Space>
              <Button type="primary" htmlType="submit">应用</Button>
              <Typography.Text type="secondary">
                shadow=on 表示规则只观察不影响 verdict；off 表示进入决策路径
              </Typography.Text>
            </Space>
          </Form.Item>
        </Form>
      </Card>

      <Row gutter={[16, 16]}>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}><Statistic title="待审核" value={pending.length} suffix="条" /></Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="ALLOW 比例" value={allowRate} suffix="%" valueStyle={{ color: '#3f8600' }} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="REVIEW 比例" value={reviewRate} suffix="%" valueStyle={{ color: '#faad14' }} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="DENY 比例" value={denyRate} suffix="%" valueStyle={{ color: '#cf1322' }} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="反馈样本" value={outcomes.length} suffix="条" />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="标记欺诈" value={fraudCount} suffix={`/ ${outcomes.length}`} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="标记正常" value={legitCount} suffix={`/ ${outcomes.length}`} />
          </Card>
        </Col>
        <Col xs={24} sm={12} md={6}>
          <Card loading={loading}>
            <Statistic title="欺诈率" value={fraudRate} suffix="%" valueStyle={{ color: '#cf1322' }} />
          </Card>
        </Col>
      </Row>

      <Card title="最近 DENY 决策" style={{ marginTop: 16 }} loading={loading}>
        <Table<DecisionRow>
          rowKey="decision_id" size="small" pagination={false} dataSource={recentDeny}
          locale={{ emptyText: '没有 DENY 决策（说明大盘风险偏低）' }}
          columns={[
            {
              title: '决策 ID', dataIndex: 'decision_id', width: 240, ellipsis: true,
              render: (v) => <Typography.Text code>{v}</Typography.Text>,
            },
            {
              title: '时间', dataIndex: 'occurred_at', width: 170,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss'),
            },
            {
              title: 'Verdict', dataIndex: 'verdict', width: 80,
              render: (v: string) => <Tag color={VERDICT_COLOR[v] || 'default'}>{v}</Tag>,
            },
            { title: '风险分', dataIndex: 'risk_score', width: 80, align: 'center' },
            {
              title: '命中规则', dataIndex: 'hit_rules',
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
