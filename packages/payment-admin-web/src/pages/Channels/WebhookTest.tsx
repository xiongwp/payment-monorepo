import { useState } from 'react'
import { Button, Card, Form, Input, Select, Typography, message } from 'antd'
import { useTranslation } from 'react-i18next'
import { testWebhook } from '../../api'
import type { WebhookTestResponse } from '../../api/types'

export default function WebhookTest() {
  const { t } = useTranslation('channel')
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<WebhookTestResponse | null>(null)

  const onRun = async (v: any) => {
    setLoading(true)
    setResult(null)
    let headers: Record<string, string> = {}
    try {
      if (v.headers) headers = JSON.parse(v.headers)
    } catch {
      message.error(t('webhookTest.errors.invalidHeadersJson'))
      setLoading(false)
      return
    }
    try {
      const r = await testWebhook({
        adapter: v.adapter,
        headers,
        body: v.body || '',
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
      <Typography.Title level={3}>{t('webhookTest.title')}</Typography.Title>
      <Card>
        <Form form={form} layout="vertical" onFinish={onRun}>
          <Form.Item
            name="adapter"
            label={t('webhookTest.form.adapter')}
            rules={[{ required: true }]}
            initialValue="gcash"
          >
            <Select
              options={[
                { value: 'gcash', label: 'gcash' },
                { value: 'maya', label: 'maya' },
                { value: 'grabpay', label: 'grabpay' },
                { value: 'coinsph', label: 'coinsph' },
                { value: 'instapay', label: 'instapay' },
                { value: 'pesonet', label: 'pesonet' },
              ]}
              style={{ width: 200 }}
            />
          </Form.Item>
          <Form.Item
            name="headers"
            label={t('webhookTest.form.headers')}
            initialValue='{"X-Signature": "abc"}'
          >
            <Input.TextArea rows={3} />
          </Form.Item>
          <Form.Item name="body" label={t('webhookTest.form.body')} rules={[{ required: true }]}>
            <Input.TextArea rows={10} placeholder={t('webhookTest.payloadPlaceholder')} />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" loading={loading}>
              {t('webhookTest.actions.parse')}
            </Button>
          </Form.Item>
        </Form>
      </Card>

      {result && (
        <Card style={{ marginTop: 16 }} title={t('webhookTest.result')}>
          <pre style={{ margin: 0 }}>{JSON.stringify(result, null, 2)}</pre>
        </Card>
      )}
    </div>
  )
}
