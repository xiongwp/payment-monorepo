import { useState } from 'react'
import { Alert, Button, Card, Form, Input, InputNumber, Select, Space, Typography, message } from 'antd'
import { probeRoute } from '../../api'
import type { ProbeRouteResponse } from '../../api/types'

export default function ChannelProbe() {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<ProbeRouteResponse | null>(null)

  const onRun = async (v: any) => {
    setLoading(true)
    setResult(null)
    try {
      const r = await probeRoute({
        country: v.country,
        payment_method: v.payment_method,
        amount: v.amount,
        currency: v.currency,
        merchant: v.merchant,
      })
      setResult(r)
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div>
      <Typography.Title level={3}>路由探测</Typography.Title>
      <Alert
        message="会向 payment-core 发送一次带 extra.probe=true 的 Charge；确保生产环境的 adapter 识别该标记、不会产生真实扣款。"
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
      />
      <Card>
        <Form
          form={form}
          layout="vertical"
          onFinish={onRun}
          initialValues={{ country: 'PH', currency: 'PHP', amount: 10000 }}
        >
          <Space wrap>
            <Form.Item name="country" label="国家" rules={[{ required: true }]}>
              <Select
                style={{ width: 120 }}
                options={[
                  { value: 'PH', label: 'PH' },
                  { value: 'ID', label: 'ID' },
                  { value: 'VN', label: 'VN' },
                ]}
              />
            </Form.Item>
            <Form.Item name="payment_method" label="支付方式" rules={[{ required: true }]}>
              <Select
                style={{ width: 180 }}
                options={[
                  { value: 'GCASH', label: 'GCASH' },
                  { value: 'MAYA', label: 'MAYA' },
                  { value: 'GRABPAY', label: 'GRABPAY' },
                  { value: 'COINS_PH', label: 'COINS_PH' },
                  { value: 'INSTAPAY', label: 'INSTAPAY' },
                  { value: 'PESONET', label: 'PESONET' },
                ]}
              />
            </Form.Item>
            <Form.Item name="amount" label="金额 (分)" rules={[{ required: true }]}>
              <InputNumber min={1} style={{ width: 160 }} />
            </Form.Item>
            <Form.Item name="currency" label="币种">
              <Input style={{ width: 80 }} />
            </Form.Item>
            <Form.Item name="merchant" label="商户">
              <Input style={{ width: 160 }} />
            </Form.Item>
            <Form.Item label=" ">
              <Button type="primary" htmlType="submit" loading={loading}>
                探测
              </Button>
            </Form.Item>
          </Space>
        </Form>
      </Card>

      {result && (
        <Card style={{ marginTop: 16 }} title="结果">
          <pre style={{ margin: 0 }}>{JSON.stringify(result, null, 2)}</pre>
        </Card>
      )}
    </div>
  )
}
