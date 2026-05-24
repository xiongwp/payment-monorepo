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
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('app')
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
      <Typography.Title level={3}>{t('simulator.title')}</Typography.Title>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={t('simulator.notice.message')}
        description={t('simulator.notice.description')}
      />

      <Steps
        current={step}
        style={{ marginBottom: 24 }}
        items={[
          { title: t('simulator.steps.create') },
          { title: t('simulator.steps.selectMethod') },
          { title: t('simulator.steps.complete') },
        ]}
      />

      {step === 0 && (
        <Card title={t('simulator.step1.title')}>
          <Form
            form={form}
            layout="vertical"
            onFinish={onCreate}
            initialValues={{ currency: 'PHP', country: 'PH', amount: 10000, mch_id: 'demo_merchant' }}
          >
            <Row gutter={16}>
              <Col span={6}>
                <Form.Item name="mch_id" label={t('simulator.step1.form.mchId')} rules={[{ required: true }]}>
                  <Input placeholder="demo_merchant" />
                </Form.Item>
              </Col>
              <Col span={6}>
                <Form.Item name="mch_order_no" label={t('simulator.step1.form.mchOrderNo')}>
                  <Input placeholder={t('simulator.step1.form.mchOrderNoPlaceholder')} />
                </Form.Item>
              </Col>
              <Col span={4}>
                <Form.Item name="amount" label={t('simulator.step1.form.amount')} rules={[{ required: true }]}>
                  <InputNumber min={1} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              <Col span={4}>
                <Form.Item name="currency" label={t('simulator.step1.form.currency')}>
                  <Input />
                </Form.Item>
              </Col>
              <Col span={4}>
                <Form.Item name="country" label={t('simulator.step1.form.country')} rules={[{ required: true }]}>
                  <Input placeholder="PH" />
                </Form.Item>
              </Col>
            </Row>
            <Form.Item name="description" label={t('simulator.step1.form.description')}>
              <Input placeholder={t('simulator.step1.form.descriptionPlaceholder')} />
            </Form.Item>
            <Form.Item name="return_url" label={t('simulator.step1.form.returnUrl')}>
              <Input placeholder="https://cashier.example.com/ret" />
            </Form.Item>
            <Form.Item name="idempotency_key" label={t('simulator.step1.form.idempotencyKey')}>
              <Input placeholder="idem_xxx" />
            </Form.Item>
            <Button type="primary" htmlType="submit" loading={creating}>
              {t('simulator.step1.form.submit')}
            </Button>
          </Form>
        </Card>
      )}

      {step >= 1 && pi && (
        <Card title={t('simulator.intent.title')} style={{ marginBottom: 16 }}>
          <Descriptions column={3} size="small">
            <Descriptions.Item label={t('simulator.intent.piId')}>{pi.id}</Descriptions.Item>
            <Descriptions.Item label={t('simulator.intent.status')}>
              <Tag>{pi.status.replace('PAYMENT_INTENT_STATUS_', '')}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label={t('simulator.intent.amount')}>
              {display(pi.amount, pi.currency)}
            </Descriptions.Item>
            <Descriptions.Item label={t('simulator.intent.merchant')}>
              {pi.mch_id} / {pi.mch_order_no}
            </Descriptions.Item>
            <Descriptions.Item label={t('simulator.intent.shardKey')}>{pi.business_id}</Descriptions.Item>
            <Descriptions.Item label={t('simulator.intent.createdAt')}>
              {pi.created ? dayjs(pi.created * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
            </Descriptions.Item>
          </Descriptions>
        </Card>
      )}

      {step === 1 && (
        <Card title={t('simulator.step2.title')}>
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
          title={t('simulator.step3.title')}
          extra={
            <Space>
              <Button onClick={onPoll} loading={polling}>
                {t('simulator.step3.refresh')}
              </Button>
              <Button onClick={reset}>{t('simulator.step3.again')}</Button>
            </Space>
          }
        >
          {confirmResp.charge && (
            <Descriptions column={2} size="small" style={{ marginBottom: 16 }}>
              <Descriptions.Item label={t('simulator.step3.chargeId')}>{confirmResp.charge.id}</Descriptions.Item>
              <Descriptions.Item label={t('simulator.step3.paymentMethod')}>{confirmResp.charge.payment_method}</Descriptions.Item>
              <Descriptions.Item label={t('simulator.step3.chargeAmount')}>
                {display(confirmResp.charge.amount, confirmResp.charge.currency)}
              </Descriptions.Item>
              <Descriptions.Item label={t('simulator.step3.chargeStatus')}>
                {confirmResp.charge.status.replace('CHARGE_STATUS_', '')}
              </Descriptions.Item>
            </Descriptions>
          )}

          {confirmResp.next_action && <NextActionCard na={confirmResp.next_action} />}

          {!confirmResp.next_action && (
            <Alert
              type="success"
              showIcon
              message={t('simulator.step3.noActionNeeded.message')}
              description={t('simulator.step3.noActionNeeded.description', { status: pi?.status })}
            />
          )}
        </Card>
      )}
    </div>
  )
}

function NextActionCard({ na }: { na: AppNextAction }) {
  const { t } = useTranslation('app')
  const redirectURL = na.payload?.['redirect_url']
  return (
    <div>
      <Alert
        type="warning"
        showIcon
        message={t('simulator.nextAction.warningMessage', { actionType: na.action_type })}
        description={
          redirectURL ? (
            <div>
              {t('simulator.nextAction.redirectHint')}
              <a href={redirectURL} target="_blank" rel="noreferrer">
                {redirectURL}
              </a>
            </div>
          ) : (
            t('simulator.nextAction.payloadFallback')
          )
        }
        style={{ marginBottom: 12 }}
      />
      <Descriptions column={2} size="small" bordered>
        <Descriptions.Item label={t('simulator.nextAction.actionId')}>{na.id}</Descriptions.Item>
        <Descriptions.Item label={t('simulator.nextAction.type')}>{na.action_type}</Descriptions.Item>
        <Descriptions.Item label={t('simulator.nextAction.status')}>{na.status}</Descriptions.Item>
        <Descriptions.Item label={t('simulator.nextAction.charge')}>{na.charge_id}</Descriptions.Item>
        <Descriptions.Item label={t('simulator.nextAction.expiresAt')} span={2}>
          {na.expires_at ? dayjs(na.expires_at * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'}
        </Descriptions.Item>
        <Descriptions.Item label={t('simulator.nextAction.payload')} span={2}>
          <pre style={{ margin: 0 }}>{JSON.stringify(na.payload, null, 2)}</pre>
        </Descriptions.Item>
      </Descriptions>
    </div>
  )
}
