/**
 * FleetRoutingTest — Fleet × Rotation 路由测试面板
 *
 * 给运维 / 演示用的两个工具：
 *   1. Resolve Fleet Sub —— 输入 LA key + flow_id，复刻 rotation_router 算法，
 *      返回路由命中的 sub-account（含 sub_idx, account_no, group, phase, balance）。
 *   2. Fleet Test Book ——  端到端 demo：src 走 fleet routing 选 sub，dst 直填,
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
import { useTranslation, Trans } from 'react-i18next'
import type { TFunction } from 'i18next'
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

function GroupTag({ group }: { group: string }) {
  const { t } = useTranslation('rotation')
  if (!group) return <Tag>{t('test.groupLegacy')}</Tag>
  return <Tag color={group === 'A' ? 'cyan' : 'purple'}>{t('test.groupLabel', { group })}</Tag>
}

// ────────────────────────────────────────────────────────────────
// Sub-account 解析结果展示
// ────────────────────────────────────────────────────────────────

function ResolutionView({ data }: { data: FleetSubResolution }) {
  const { t } = useTranslation('rotation')
  return (
    <Descriptions bordered size="small" column={1}>
      <Descriptions.Item label={t('test.resolution.laKey')}>
        <Text code>{data.logical_account_key}</Text>
      </Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.laId')}>{data.logical_account_id}</Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.flowId')}>
        <Text code>{data.flow_id}</Text>
      </Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.subIdx')}>
        <Text strong>{data.sub_idx}</Text>{' '}
        <Text type="secondary">{t('test.resolution.subIdxHelper')}</Text>
      </Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.accountNo')}>
        <Text copyable code>
          {data.account_no}
        </Text>
      </Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.accountGroup')}><GroupTag group={data.account_group} /></Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.lifecyclePhase')}>
        {phaseTag(data.lifecycle_phase_str, data.lifecycle_phase)}
      </Descriptions.Item>
      <Descriptions.Item label={t('test.resolution.balance')}>
        <Text>{displayMoney(data.balance, data.currency)}</Text>{' '}
        <Text type="secondary">{t('test.resolution.minorUnitsSuffix', { balance: data.balance, currency: data.currency })}</Text>
      </Descriptions.Item>
    </Descriptions>
  )
}

// ────────────────────────────────────────────────────────────────
// Card 1: Resolve Fleet Sub
// ────────────────────────────────────────────────────────────────

function ResolveCard() {
  const { t } = useTranslation('rotation')
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
          <span>{t('test.resolve.title')}</span>
        </Space>
      }
      extra={
        <Text type="secondary">
          {t('test.resolve.endpoint')}
        </Text>
      }
    >
      <Paragraph type="secondary" style={{ marginTop: 0 }}>
        <Trans i18nKey="test.resolve.description" t={t}>
          {'给定 LA key + flow_id，复刻 '}
          <Text code>rotation_router.go</Text>
          {' 的 fleet 路由算法，返回选中的 sub-account（用于排障 / Demo）。同一个 flow_id 永远命中同一个 sub_idx（幂等）。'}
        </Trans>
      </Paragraph>
      <Form form={form} layout="vertical" onFinish={submit}>
        <Row gutter={16}>
          <Col span={14}>
            <Form.Item
              label={t('test.resolve.logicalAccountKeyLabel')}
              name="logical_account_key"
              rules={[{ required: true, message: t('test.resolve.requiredMsg') }]}
              initialValue="channel-payable:alipay"
            >
              <Input placeholder={t('test.resolve.logicalAccountKeyPlaceholder')} />
            </Form.Item>
          </Col>
          <Col span={10}>
            <Form.Item
              label={t('test.resolve.flowIdLabel')}
              name="flow_id"
              rules={[{ required: true, message: t('test.resolve.requiredMsg') }]}
              initialValue="order-12345"
              tooltip={t('test.resolve.flowIdTooltip')}
            >
              <Input placeholder={t('test.resolve.flowIdPlaceholder')} />
            </Form.Item>
          </Col>
        </Row>
        <Button
          type="primary"
          icon={<ThunderboltOutlined />}
          htmlType="submit"
          loading={loading}
        >
          {t('test.resolve.submitButton')}
        </Button>
      </Form>

      {error && (
        <Alert
          type="error"
          showIcon
          message={t('test.resolve.errorTitle')}
          description={error}
          style={{ marginTop: 16 }}
        />
      )}
      {result && (
        <div style={{ marginTop: 16 }}>
          <Divider orientation="left" plain>
            <Text type="secondary">{t('test.resolve.resultTitle')}</Text>
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

function makeBusinessTypeOptions(t: TFunction) {
  return [
    { value: 'TRANSFER', label: t('test.businessTypes.transfer') },
    { value: 'PAYMENT', label: t('test.businessTypes.payment') },
    { value: 'REFUND', label: t('test.businessTypes.refund') },
    { value: 'WITHDRAW', label: t('test.businessTypes.withdraw') },
    { value: 'DEPOSIT', label: t('test.businessTypes.deposit') },
    { value: 'COMMISSION', label: t('test.businessTypes.commission') },
  ]
}

const CURRENCY_OPTIONS = ['CNY', 'USD', 'PHP', 'HKD', 'SGD', 'JPY', 'EUR']

function BookCard() {
  const { t } = useTranslation('rotation')
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
      message.success(t('test.book.bookSuccess'))
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
          <span>{t('test.book.title')}</span>
        </Space>
      }
      extra={<Text type="secondary">{t('test.book.endpoint')}</Text>}
    >
      <Alert
        type="warning"
        showIcon
        message={t('test.book.alertTitle')}
        description={
          <>
            <Trans i18nKey="test.book.alertDesc1" t={t}>
              {'源账户走 fleet routing（推荐填 '}
              <Text code>src_logical_account_key</Text>
              {'），目标账户直填 account_no。生产 caller 应走 gRPC '}
              <Text code>CreateTransaction</Text>
              {'，此处仅用于运维验证 fleet 链路是否打通。'}
            </Trans>
            <br />
            {t('test.book.alertDesc2')}
          </>
        }
        style={{ marginBottom: 16 }}
      />
      <Form form={form} layout="vertical" onFinish={submit}>
        <Divider orientation="left" plain>
          <Text type="secondary">{t('test.book.srcSectionTitle')}</Text>
        </Divider>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              label={t('test.book.srcLogicalAccountKeyLabel')}
              name="src_logical_account_key"
              initialValue="channel-payable:alipay"
              tooltip={t('test.book.srcLogicalAccountKeyTooltip')}
            >
              <Input placeholder={t('test.book.srcLogicalAccountKeyPlaceholder')} allowClear />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item
              label={t('test.book.srcAccountNoLabel')}
              name="src_account_no"
              tooltip={t('test.book.srcAccountNoTooltip')}
            >
              <Input placeholder={t('test.book.srcAccountNoPlaceholder')} allowClear />
            </Form.Item>
          </Col>
        </Row>

        <Divider orientation="left" plain>
          <Text type="secondary">{t('test.book.dstSectionTitle')}</Text>
        </Divider>
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item
              label={t('test.book.dstAccountNoLabel')}
              name="dst_account_no"
              rules={[{ required: true, message: t('test.book.dstAccountNoRequired') }]}
            >
              <Input placeholder={t('test.book.dstAccountNoPlaceholder')} />
            </Form.Item>
          </Col>
          <Col span={6}>
            <Form.Item
              label={t('test.book.amountLabel')}
              name="amount"
              rules={[{ required: true, message: t('test.book.amountRequired') }]}
              initialValue={100}
            >
              <InputNumber min={1} style={{ width: '100%' }} />
            </Form.Item>
          </Col>
          <Col span={6}>
            <Form.Item label={t('test.book.currencyLabel')} name="currency" initialValue="CNY">
              <Select options={CURRENCY_OPTIONS.map((c) => ({ value: c, label: c }))} />
            </Form.Item>
          </Col>
        </Row>

        <Divider orientation="left" plain>
          <Text type="secondary">{t('test.book.auditSectionTitle')}</Text>
        </Divider>
        <Row gutter={16}>
          <Col span={8}>
            <Form.Item
              label={t('test.book.flowIdLabel')}
              name="flow_id"
              rules={[{ required: true, message: t('test.book.flowIdRequired') }]}
              initialValue={`fleet-demo-${Date.now()}`}
              tooltip={t('test.book.flowIdTooltip')}
            >
              <Input />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              label={t('test.book.businessTypeLabel')}
              name="business_type"
              initialValue="TRANSFER"
              rules={[{ required: true }]}
            >
              <Select options={makeBusinessTypeOptions(t)} />
            </Form.Item>
          </Col>
          <Col span={8}>
            <Form.Item
              label={t('test.book.operatorLabel')}
              name="operator"
              rules={[{ required: true, message: t('test.book.operatorRequired') }]}
              initialValue="demo-user"
            >
              <Input placeholder={t('test.book.operatorPlaceholder')} />
            </Form.Item>
          </Col>
        </Row>

        <Button
          type="primary"
          icon={<ThunderboltOutlined />}
          htmlType="submit"
          loading={loading}
        >
          {t('test.book.submitButton')}
        </Button>
      </Form>

      {error && (
        <Alert
          type="error"
          showIcon
          message={t('test.book.errorTitle')}
          description={error}
          style={{ marginTop: 16 }}
        />
      )}
      {result && (
        <div style={{ marginTop: 16 }}>
          <Divider orientation="left" plain>
            <Text type="secondary">{t('test.book.resultTitle')}</Text>
          </Divider>
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label={t('test.book.voucherNo')}>
              <Text copyable code>
                {result.voucher_no}
              </Text>
            </Descriptions.Item>
            <Descriptions.Item label={t('test.book.transactionIds')}>
              <Space direction="vertical" size={0}>
                {result.transaction_ids.map((id) => (
                  <Text key={id} copyable code>
                    {id}
                  </Text>
                ))}
              </Space>
            </Descriptions.Item>
            <Descriptions.Item label={t('test.book.bookingTime')}>{result.booking_time}</Descriptions.Item>
          </Descriptions>
          {result.src_resolution && (
            <>
              <Divider orientation="left" plain>
                <Text type="secondary">{t('test.book.srcResolutionTitle')}</Text>
              </Divider>
              <ResolutionView data={result.src_resolution} />
              <Alert
                type="info"
                showIcon
                style={{ marginTop: 16 }}
                message={
                  <Trans
                    i18nKey="test.book.srcDebitedMessage"
                    t={t}
                    values={{ amount: displayMoney(result.src_resolution.balance, result.src_resolution.currency) }}
                  >
                    {'src account 已 Debit '}
                    <Text code>{'{{amount}}'}</Text>
                    {' 后变为当前 balance；可点 '}
                    <a href={`/rotation/${result.src_resolution.logical_account_key}`}>
                      LA 详情页
                    </a>
                    {' 看 fleet 全 sub 分布。'}
                  </Trans>
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
  const { t } = useTranslation('rotation')
  return (
    <div style={{ padding: 24 }}>
      <Title level={3}>{t('test.pageTitle')}</Title>
      <Paragraph type="secondary">
        <Trans i18nKey="test.pageDescription" t={t}>
          {'Fleet 模式下，一个 Logical Account 背后挂着 100 个 sub-account（按 user_id 0-99 分散在 100 个 shard），Booking router 用 '}
          <Text code>fnv32a(flow_id) % 100</Text>
          {' 选定哪个 sub 承接流量。本页提供两个工具 —— '}
          <b>解析</b>
          {'（看路由命中谁）和 '}
          <b>记账</b>
          {'（端到端跑一笔验证链路）。'}
        </Trans>
      </Paragraph>
      <Space direction="vertical" size="large" style={{ width: '100%' }}>
        <ResolveCard />
        <BookCard />
      </Space>
    </div>
  )
}
