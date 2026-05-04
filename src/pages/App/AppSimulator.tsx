import { useState } from 'react'
import {
  Alert,
  Button,
  Card,
  Col,
  Descriptions,
  Form,
  Input,
  InputNumber,
  Row,
  Space,
  Steps,
  Tag,
  Typography,
  message,
} from 'antd'
import dayjs from 'dayjs'
import { appConfirm, appCreateIntent, appRetrieveIntent } from '../../api'
import type { AppConfirmResponse, AppNextAction } from '../../api'
import type { PaymentIntentDetail } from '../../api/types'
import { display } from '../../utils/money'

// 仿 Stripe 收银台的三步流：
//   1. 商户创建订单（CreatePaymentIntent）
//   2. 用户选渠道（Confirm）→ 拿到 next_action
//   3. 用户完成动作（App 跳转 / 扫码 / OTP…）→ 轮询订单状态
//
// 没真实浏览器跳转；第 3 步只展示 next_action 细节 + 提供 "刷新状态" 按钮。

const METHODS = ['GCASH', 'MAYA', 'GRABPAY', 'COINS_PH', 'INSTAPAY', 'PESONET']

export default function AppSimulator() {
  const [form] = Form.useForm()
  const [step, setStep] = useState(0)

  const [pi, setPI] = useState<PaymentIntentDetail | null>(null)
  const [confirmResp, setConfirmResp] = useState<AppConfirmResponse | null>(null)
  const [creating, setCreating] = useState(false)
  const [confirming, setConfirming] = useState(false)
  const [polling, setPolling] = useState(false)

  const onCreate = async (v: any) => {
    setCreating(true)
    try {
      const created = await appCreateIntent({
        mch_id: v.mch_id,
        mch_order_no: v.mch_order_no || `mch_${Date.now()}`,
        amount: v.amount,
        currency: v.currency || 'PHP',
        description: v.description || '',
        idempotency_key: v.idempotency_key || `idem_${Date.now()}`,
        return_url: v.return_url || 'https://cashier.example.com/ret',
        // payment-core 路由按 metadata.country 选 adapter；填错或不填会 no_match
        metadata: { country: (v.country || 'PH').toUpperCase() },
      })
      setPI(created)
      setStep(1)
    } catch (e) {
      message.error(String(e))
    } finally {
      setCreating(false)
    }
  }

  const onConfirm = async (payment_method: string) => {
    if (!pi) return
    setConfirming(true)
    try {
      const r = await appConfirm({ id: pi.id, payment_method })
      setConfirmResp(r)
      setPI(r.payment_intent)
      setStep(2)
    } catch (e) {
      message.error(String(e))
    } finally {
      setConfirming(false)
    }
  }

  const onPoll = async () => {
    if (!pi) return
    setPolling(true)
    try {
      const latest = await appRetrieveIntent(pi.id)
      setPI(latest)
    } catch (e) {
      message.error(String(e))
    } finally {
      setPolling(false)
    }
  }

  const reset = () => {
    form.resetFields()
    setPI(null)
    setConfirmResp(null)
    setStep(0)
  }

  return (
    <div>
      <Typography.Title level={3}>模拟商户 App（下单 → 支付）</Typography.Title>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="这不是真实收银台"
        description="下面三步完整触发 order-core → payment-core → payment-channel 的 gRPC 链路；外部渠道（GCash/Maya/...）的 HTTP API 由 payment-channel 的 scripted adapter 在容器里 mock。"
      />

      <Steps
        current={step}
        style={{ marginBottom: 24 }}
        items={[
          { title: '商户创建订单' },
          { title: '用户选渠道' },
          { title: '完成支付' },
        ]}
      />

      {step === 0 && (
        <Card title="① CreatePaymentIntent">
          <Form
            form={form}
            layout="vertical"
            onFinish={onCreate}
            initialValues={{ currency: 'PHP', country: 'PH', amount: 10000, mch_id: 'demo_merchant' }}
          >
            <Row gutter={16}>
              <Col span={6}>
                <Form.Item name="mch_id" label="商户 ID" rules={[{ required: true }]}>
                  <Input placeholder="demo_merchant" />
                </Form.Item>
              </Col>
              <Col span={6}>
                <Form.Item name="mch_order_no" label="商户订单号">
                  <Input placeholder="留空会生成 mch_{timestamp}" />
                </Form.Item>
              </Col>
              <Col span={4}>
                <Form.Item name="amount" label="金额 (storage)" rules={[{ required: true }]}>
                  <InputNumber min={1} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              <Col span={4}>
                <Form.Item name="currency" label="币种">
                  <Input />
                </Form.Item>
              </Col>
              <Col span={4}>
                <Form.Item name="country" label="国家（路由用）" rules={[{ required: true }]}>
                  <Input placeholder="PH" />
                </Form.Item>
              </Col>
            </Row>
            <Form.Item name="description" label="描述">
              <Input placeholder="示例：购买 10 个苹果" />
            </Form.Item>
            <Form.Item name="return_url" label="return_url（用户完成支付后跳转）">
              <Input placeholder="https://cashier.example.com/ret" />
            </Form.Item>
            <Form.Item name="idempotency_key" label="幂等键（留空自动生成）">
              <Input placeholder="idem_xxx" />
            </Form.Item>
            <Button type="primary" htmlType="submit" loading={creating}>
              创建订单
            </Button>
          </Form>
        </Card>
      )}

      {step >= 1 && pi && (
        <Card title="订单" style={{ marginBottom: 16 }}>
          <Descriptions column={3} size="small">
            <Descriptions.Item label="PI ID">{pi.id}</Descriptions.Item>
            <Descriptions.Item label="状态">
              <Tag>{pi.status.replace('PAYMENT_INTENT_STATUS_', '')}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="金额">
              {display(pi.amount, pi.currency)}
            </Descriptions.Item>
            <Descriptions.Item label="商户">
              {pi.mch_id} / {pi.mch_order_no}
            </Descriptions.Item>
            <Descriptions.Item label="分片键">{pi.business_id}</Descriptions.Item>
            <Descriptions.Item label="创建时间">
              {pi.created ? dayjs(pi.created * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
            </Descriptions.Item>
          </Descriptions>
        </Card>
      )}

      {step === 1 && (
        <Card title="② Confirm：选支付方式">
          <Space wrap>
            {METHODS.map((m) => (
              <Button key={m} onClick={() => onConfirm(m)} loading={confirming}>
                {m}
              </Button>
            ))}
          </Space>
        </Card>
      )}

      {step === 2 && confirmResp && (
        <Card
          title="③ 完成支付"
          extra={
            <Space>
              <Button onClick={onPoll} loading={polling}>
                刷新订单状态
              </Button>
              <Button onClick={reset}>再来一单</Button>
            </Space>
          }
        >
          {confirmResp.charge && (
            <Descriptions column={2} size="small" style={{ marginBottom: 16 }}>
              <Descriptions.Item label="Charge ID">{confirmResp.charge.id}</Descriptions.Item>
              <Descriptions.Item label="支付方式">{confirmResp.charge.payment_method}</Descriptions.Item>
              <Descriptions.Item label="金额">
                {display(confirmResp.charge.amount, confirmResp.charge.currency)}
              </Descriptions.Item>
              <Descriptions.Item label="状态">
                {confirmResp.charge.status.replace('CHARGE_STATUS_', '')}
              </Descriptions.Item>
            </Descriptions>
          )}

          {confirmResp.next_action && <NextActionCard na={confirmResp.next_action} />}

          {!confirmResp.next_action && (
            <Alert
              type="success"
              showIcon
              message="无需用户动作，订单已终态"
              description={`PI.status = ${pi?.status}`}
            />
          )}
        </Card>
      )}
    </div>
  )
}

function NextActionCard({ na }: { na: AppNextAction }) {
  const redirectURL = na.payload?.['redirect_url']
  return (
    <div>
      <Alert
        type="warning"
        showIcon
        message={`需要用户完成 ${na.action_type}`}
        description={
          redirectURL ? (
            <div>
              收银台应跳转：
              <a href={redirectURL} target="_blank" rel="noreferrer">
                {redirectURL}
              </a>
            </div>
          ) : (
            '具体动作字段见下方 payload'
          )
        }
        style={{ marginBottom: 12 }}
      />
      <Descriptions column={2} size="small" bordered>
        <Descriptions.Item label="Action ID">{na.id}</Descriptions.Item>
        <Descriptions.Item label="类型">{na.action_type}</Descriptions.Item>
        <Descriptions.Item label="状态">{na.status}</Descriptions.Item>
        <Descriptions.Item label="Charge">{na.charge_id}</Descriptions.Item>
        <Descriptions.Item label="过期时间" span={2}>
          {na.expires_at ? dayjs(na.expires_at * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
        </Descriptions.Item>
        <Descriptions.Item label="payload" span={2}>
          <pre style={{ margin: 0 }}>{JSON.stringify(na.payload, null, 2)}</pre>
        </Descriptions.Item>
      </Descriptions>
    </div>
  )
}
