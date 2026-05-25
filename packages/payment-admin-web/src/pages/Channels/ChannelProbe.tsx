import { useState } from 'react'
import { Alert, Button, Card, Form, Input, InputNumber, Select, Space, Typography, message } from 'antd'
import { useTranslation } from 'react-i18next'
import { probeRoute } from '../../api'
import type { ProbeRouteResponse } from '../../api/types'

export default function ChannelProbe() {
  const { t } = useTranslation('channel')
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
      <Typography.Title level={3}>{t('probe.title')}</Typography.Title>
      <Alert
        message={t('probe.warning')}
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
            <Form.Item name="country" label={t('probe.form.country')} rules={[{ required: true }]}>
              <Select
                style={{ width: 120 }}
                options={[
                  { value: 'PH', label: 'PH' },
                  { value: 'ID', label: 'ID' },
                  { value: 'VN', label: 'VN' },
                ]}
              />
            </Form.Item>
            <Form.Item name="payment_method" label={t('probe.form.paymentMethod')} rules={[{ required: true }]}>
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
            <Form.Item name="amount" label={t('probe.form.amount')} rules={[{ required: true }]}>
              <InputNumber min={1} style={{ width: 160 }} />
            </Form.Item>
            <Form.Item name="currency" label={t('probe.form.currency')}>
              <Input style={{ width: 80 }} />
            </Form.Item>
            <Form.Item name="merchant" label={t('probe.form.merchant')}>
              <Input style={{ width: 160 }} />
            </Form.Item>
            <Form.Item label=" ">
              <Button type="primary" htmlType="submit" loading={loading}>
                {t('probe.actions.run')}
              </Button>
            </Form.Item>
          </Space>
        </Form>
      </Card>

      {result && (
        <Card style={{ marginTop: 16 }} title={t('probe.result')}>
          <pre style={{ margin: 0 }}>{JSON.stringify(result, null, 2)}</pre>
        </Card>
      )}
    </div>
  )
}
