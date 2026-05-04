import { useState } from 'react'
import { Button, Card, Form, Input, Select, Typography, message } from 'antd'
import { testWebhook } from '../../api'
import type { WebhookTestResponse } from '../../api/types'

export default function WebhookTest() {
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
      message.error('headers 必须是合法 JSON')
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
      <Typography.Title level={3}>Webhook 调试</Typography.Title>
      <Card>
        <Form form={form} layout="vertical" onFinish={onRun}>
          <Form.Item
            name="adapter"
            label="Adapter"
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
            label="Headers (JSON)"
            initialValue='{"X-Signature": "abc"}'
          >
            <Input.TextArea rows={3} />
          </Form.Item>
          <Form.Item name="body" label="Body (raw)" rules={[{ required: true }]}>
            <Input.TextArea rows={10} placeholder='{"pi": "pi_xxx", "event": "charge.succeeded"}' />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" loading={loading}>
              解析
            </Button>
          </Form.Item>
        </Form>
      </Card>

      {result && (
        <Card style={{ marginTop: 16 }} title="解析结果">
          <pre style={{ margin: 0 }}>{JSON.stringify(result, null, 2)}</pre>
        </Card>
      )}
    </div>
  )
}
