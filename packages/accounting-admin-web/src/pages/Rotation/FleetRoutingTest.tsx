/**
 * FleetRoutingTest — Fleet × Rotation 路由测试面板
 *
 * 给运维 / 演示用的两个工具：
 *   1. Resolve Fleet Sub —— 输入 LA key + flow_id，复刻 rotation_router 算法，
 *      返回路由命中的 sub-account（含 sub_idx, account_no, group, phase, balance）。
 *   2. Fleet Test Book ——  端到端 demo：src 走 fleet routing 选 sub，dst 直填，
 *      调 accounting-system DoubleEntryBooking 真记账，返回 voucher_no + tx_ids。
 *
 * 设计：
 *   - 两个独立 Card，互不依赖（运维可以只用第一个看路由）
 *   - Resolve 结果 + Book 结果都展示完整 JSON 让运维可复制
 *   - Fleet Book 提交前会先 Resolve 一次给用户预览 src 命中谁
 *   - 表单数据不持久化（demo 性质，不引 localStorage）
 */
import { useState } from 'react'
import {
  Card,
  Form,
  Input,
  InputNumber,
  Button,
  Space,
  Typography,
  message,
  Tag,
  Descriptions,
  Alert,
  Divider,
  Select,
  Row,
  Col,
} from 'antd'
import { ThunderboltOutlined, FunctionOutlined } from '@ant-design/icons'
import {
  rotationResolveFleetSub,
  rotationFleetBook,
} from '../../api/accounting'
import type {
  FleetSubResolution,
  FleetTestBookResponse,
} from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

const { Title, Paragraph, Text } = Typography

// ────────────────────────────────────────────────────────────────
// 共用：phase / group 标签
// ────────────────────────────────────────────────────────────────

function phaseTag(phaseStr: string, phaseCode: number) {
  // active=1 绿，draining=2 橙，frozen=3 红，archived=4 灰，provisioned=5 蓝
  const color =
    phaseCode === 1
      ? 'green'
      : phaseCode === 2
      ? 'orange'
      : phaseCode === 3
      ? 'red'
      : phaseCode === 5
      ? 'blue'
      : 'default'
  return (
    <Tag color={color}>
      {phaseStr} ({phaseCode})
    </Tag>
  )
}

function groupTag(group: string) {
  if (!group) return <Tag>(legacy)</Tag>
  return <Tag color={group === 'A' ? 'cyan' : 'purple'}>Group {group}</Tag>
}

// ────────────────────────────────────────────────────────────────
// Sub-account 解析结果展示
// ────────────────────────────────────────────────────────────────

function ResolutionView({ data }: { data: FleetSubResolution }) {
  return (
    <Descriptions bordered size="small" column={1}>
      <Descriptions.Item label="LA key">
        <Text code>{data.logical_account_key}</Text>
      </Descriptions.Item>
      <Descriptions.Item label="LA id">{data.logical_account_id}</Descriptions.Item>
      <Descriptions.Item label="flow_id">
        <Text code>{data.flow_id}</Text>
      </Descriptions.Item>
      <Descriptions.Item label="sub_idx">
        <Text strong>{data.sub_idx}</Text>{' '}
        <Text type="secondary">= fnv32a(flow_id) % 100</Text>
      </Descriptions.Item>
      <Descriptions.Item label="account_no">
        <Text copyable code>
          {data.account_no}
        </Text>
      </Descriptions.Item>
      <Descriptions.Item label="account_group">{groupTag(data.account_group)}</Descriptions.Item>
      <Descriptions.Item label="lifecycle_phase">
        {phaseTag(data.lifecycle_phase_str, data.lifecycle_phase)}
      </Descriptions.Item>
      <Descriptions.Item label="balance">
        <Text>{displayMoney(data.balance, data.currency)}</Text>{' '}
        <Text type="secondary">({data.balance} {data.currency} minor units)</Text>
      </Descriptions.Item>
    </Descriptions>
  )
}

// ────────────────────────────────────────────────────────────────
// Card 1: Resolve Fleet Sub
// ────────────────────────────────────────────────────────────────

function ResolveCard() {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<FleetSubResolution | null>(null)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    try {
      const v = await form.validateFields()
      setLoading(true)
      setError(null)
      const r = await rotationResolveFleetSub(v.logical_account_key, v.flow_id)
      setResult(r)
    } catch (e) {
      if ((e as { errorFields?: unknown }).errorFields) return
      const msg = e instanceof Error ? e.message : String(e)
      setError(msg)
      setResult(null)
    } finally {
      setLoading(false)
    }
  }

  return (
    <Card
      title={
        <Space>
          <FunctionOutlined />
          <span>1. Fleet Routing 解析</span>
        </Space>
      }
      extra={
        <Text type="secondary">
          GET /v1/rotation/resolve-fleet-sub
        </Text>
      }
    >
      <Paragraph type="secondary" style={{ marginTop: 0 }}>
        给定 LA key + flow_id，复刻 <Text code>rotation_router.go</Text> 的 fleet 路由算法，
        返回选中的 sub-account（用于排障 / Demo）。同一个 flow_id 永远命中同一个 sub_idx（幂等）。
      </Paragraph>
      <Form form={form} layout="vertical" onFinish={submit}>
        <Row gutter={16}>
          <Col span={14}>
            <Form.Item
              label="logical_account_key"
              name="logical_account_key"
              rules={[{ required: true, message: '必填' }]}
              initialValue="channel-payable:alipay"
            >
              <Input placeholder="e.g. channel-payable:alipay" />
            </Form.Item>
          </Col>
          <Col span={10}>
            <Form.Item
              label="flow_id"
              name="flow_id"
              rules={[{ required: true, message: '必填' }]}
              initialValue="order-12345"
              tooltip="任意字符串；hash 后选 sub。同 flow_id 路由到同一 sub_idx"
            >
              <Input placeholder="e.g. order-12345" />
            </Form.Item>
          </Col>
        </Row>
        <Button
          type="primary"
          icon={<ThunderboltOutlined />}
          htmlType="submit"
          loading={loading}
        >
          解析路由
        </Button>
      </Form>

      {error && (
        <Alert
          type="error"
          showIcon
          message="路由失败"
          description={error}
          style={{ marginTop: 16 }}
        />
      )}
      {result && (
        <div style={{ marginTop: 16 }}>
          <Divider orientation="left" plain>
            <Text type="secondary">路由结果</Text>
          </Divider>
          <ResolutionView data={result} />
        </div>
      )}
    </Card>
  )
}

// ────────────────────────────────────────────────────────────────
// Card 2: Fleet Test Book — 端到端记账 demo
// ────────────────────────────────────────────────────────────────

const BUSINESS_TYPE_OPTIONS = [
  { value: 'TRANSFER', label: 'TRANSFER 转账' },
  { value: 'PAYMENT', label: 'PAYMENT 支付' },
  { value: 'REFUND', label: 'REFUND 退款' },
  { value: 'WITHDRAW', label: 'WITHDRAW 提现' },
  { value: 'DEPOSIT', label: 'DEPOSIT 充值' },
  { value: 'COMMISSION', label: 'COMMISSION 佣金' },
]

const CURRENCY_OPTIONS = ['CNY', 'USD', 'PHP', 'HKD', 'SGD', 'JPY', 'EUR']

function BookCard() {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<FleetTestBookResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    try {
      const v = await form.validateFields()
      setLoading(true)
      setError(null)
      const r = await rotationFleetBook({
        src_logical_account_key: v.src_logical_account_key || undefined,
        src_account_no: v.src_account_no || undefined,
        dst_account_no: v.dst_account_no,
        amount: v.amount,
        currency: v.currency,
        flow_id: v.flow_id,
        business_type: v.business_type,
        operator: v.operator,
      })
      setResult(r)
      message.success('记账成功')
    } catch (e) {
      if ((e as { errorFields?: unknown }).errorFields) return
      const msg = e instanceof Error ? e.message : String(e)
      setError(msg)
      setResult(null)
    } finally {
      setLoading(false)
    }
  }

  return (
    <Card
      title={
        <Space>
          <ThunderboltOutlined />
          <span>2. Fleet 端到端记账 demo</span>
        </Space>
      }
      extra={<Text type="secondary">POST /v1/rotation/fleet-book</Text>}
    >
      <Alert
        type="warning"
        showIcon
        message="演示性接口"
        description={
          <>
            源账户走 fleet routing（推荐填 <Text code>src_logical_account_key</Text>），
            目标账户直填 account_no。生产 caller 应走 gRPC <Text code>CreateTransaction</Text>，
            此处仅用于运维验证 fleet 链路是否打通。<br />
            金额从 src（Debit）流向 dst（Credit）；src 余额减、dst 余额加。
          </>
        }
        style={{ marginBottom: 16 }}
      />
      <Form form={form} layout="vertical" onFinish={submit}>
        <Divider orientation="left" plain>
          <Text type="secondary">源账户（二选一）</Text>
        </Divider>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              label="src_logical_account_key（推荐 — 走 fleet routing）"
              name="src_logical_account_key"
              initialValue="channel-payable:alipay"
              tooltip="给 LA key + flow_id 就能自动选中 fleet 里某个 sub-account"
            >
              <Input placeholder="e.g. channel-payable:alipay" allowClear />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              label="src_account_no（绕过 fleet routing）"
              name="src_account_no"
              tooltip="直接指定源账户号；填了就不走 routing"
            >
              <Input placeholder="留空则用 LA key" allowClear />
            </Form.Item>
          </Col>
        </Row>

        <Divider orientation="left" plain>
          <Text type="secondary">目标账户 + 金额</Text>
        </Divider>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              label="dst_account_no"
              name="dst_account_no"
              rules={[{ required: true, message: '必填，必须是已存在的账户' }]}
            >
              <Input placeholder="e.g. 156020100011099999" />
            </Form.Item>
          </Col>
          <Col span={6}>
            <Form.Item
              label="amount (minor units)"
              name="amount"
              rules={[{ required: true, message: '必填' }]}
              initialValue={100}
            >
              <InputNumber min={1} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={6}>
            <Form.Item label="currency" name="currency" initialValue="CNY">
              <Select options={CURRENCY_OPTIONS.map((c) => ({ value: c, label: c }))} />
            </Form.Item>
          </Col>
        </Row>

        <Divider orientation="left" plain>
          <Text type="secondary">业务 + 审计</Text>
        </Divider>
        <Row gutter={16}>
          <Col span={8}>
            <Form.Item
              label="flow_id（幂等键 + routing hash）"
              name="flow_id"
              rules={[{ required: true, message: '必填' }]}
              initialValue={`fleet-demo-${Date.now()}`}
              tooltip="同 flow_id 重复调用是幂等的；同时它的 hash 决定 src 选哪个 sub"
            >
              <Input />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              label="business_type"
              name="business_type"
              initialValue="TRANSFER"
              rules={[{ required: true }]}
            >
              <Select options={BUSINESS_TYPE_OPTIONS} />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              label="operator（审计）"
              name="operator"
              rules={[{ required: true, message: '必填' }]}
              initialValue="demo-user"
            >
              <Input placeholder="ops-alice@example.com" />
            </Form.Item>
          </Col>
        </Row>

        <Button
          type="primary"
          icon={<ThunderboltOutlined />}
          htmlType="submit"
          loading={loading}
        >
          执行记账
        </Button>
      </Form>

      {error && (
        <Alert
          type="error"
          showIcon
          message="记账失败"
          description={error}
          style={{ marginTop: 16 }}
        />
      )}
      {result && (
        <div style={{ marginTop: 16 }}>
          <Divider orientation="left" plain>
            <Text type="secondary">记账结果</Text>
          </Divider>
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label="voucher_no">
              <Text copyable code>
                {result.voucher_no}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label="transaction_ids">
              <Space direction="vertical" size={0}>
                {result.transaction_ids.map((id) => (
                  <Text key={id} copyable code>
                    {id}
                  </Text>
                ))}
              </Space>
            </Descriptions.Item>
            <Descriptions.Item label="booking_time">{result.booking_time}</Descriptions.Item>
          </Descriptions>
          {result.src_resolution && (
            <>
              <Divider orientation="left" plain>
                <Text type="secondary">Fleet routing 命中的源 sub-account</Text>
              </Divider>
              <ResolutionView data={result.src_resolution} />
              <Alert
                type="info"
                showIcon
                style={{ marginTop: 16 }}
                message={
                  <>
                    src account 已 Debit{' '}
                    <Text code>{displayMoney(result.src_resolution.balance, result.src_resolution.currency)}</Text>{' '}
                    后变为当前 balance；可点{' '}
                    <a href={`/rotation/${result.src_resolution.logical_account_key}`}>
                      LA 详情页
                    </a>{' '}
                    看 fleet 全 sub 分布。
                  </>
                }
              />
            </>
          )}
        </div>
      )}
    </Card>
  )
}

// ────────────────────────────────────────────────────────────────
// Page entry
// ────────────────────────────────────────────────────────────────

export default function FleetRoutingTest() {
  return (
    <div style={{ padding: 24 }}>
      <Title level={3}>Fleet × Rotation 测试面板</Title>
      <Paragraph type="secondary">
        Fleet 模式下，一个 Logical Account 背后挂着 100 个 sub-account（按 user_id 0-99
        分散在 100 个 shard），Booking router 用{' '}
        <Text code>fnv32a(flow_id) % 100</Text> 选定哪个 sub 承接流量。本页提供两个工具
        ——
        <b>解析</b>（看路由命中谁）和 <b>记账</b>（端到端跑一笔验证链路）。
      </Paragraph>
      <Space direction="vertical" size="large" style={{ width: '100%' }}>
        <ResolveCard />
        <BookCard />
      </Space>
    </div>
  )
}
