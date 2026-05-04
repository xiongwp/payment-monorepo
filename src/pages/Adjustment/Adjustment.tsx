import { Form, Input, InputNumber, Radio, Button, Card, message, Descriptions } from 'antd'
import { useState } from 'react'
import { adjustBalance } from '../../api/accounting'
import type { AdjustBalanceRequest, AdjustBalanceResponse } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

// AdjustBalanceResponse does not carry a currency field, and the UI has no
// per-account currency context at this point (the form is keyed by
// account_no). Fall back to PHP which is the system-wide default; if a future
// backend version starts returning currency, prefer that.
const FALLBACK_CURRENCY = 'PHP'

export default function Adjustment() {
  const [form] = Form.useForm<AdjustBalanceRequest>()
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<AdjustBalanceResponse | null>(null)

  const handleSubmit = async (values: AdjustBalanceRequest) => {
    setLoading(true)
    setResult(null)
    try {
      const res = await adjustBalance({
        ...values,
        amount: String(values.amount),
        operator: values.operator || 'admin',
      })
      setResult(res)
      message.success('调账成功')
      form.resetFields()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? '调账失败'
      message.error(msg)
    } finally {
      setLoading(false)
    }
  }

  return (
    <div>
      <Card title="调账申请" style={{ marginBottom: 16 }}>
        <Form
          form={form}
          layout="vertical"
          onFinish={handleSubmit}
          style={{ maxWidth: 600 }}
          initialValues={{ adjustment_type: 'MANUAL', is_increase: true }}
        >
          <Form.Item name="account_no" label="账户号" rules={[{ required: true, message: '请输入账户号' }]}>
            <Input placeholder="请输入账户号" />
          </Form.Item>

          <Form.Item name="adjustment_type" label="调账类型" rules={[{ required: true }]}>
            <Radio.Group>
              <Radio value="CORRECTION">余额修正</Radio>
              <Radio value="COMPENSATE">补偿调账</Radio>
              <Radio value="MANUAL">手工调账</Radio>
            </Radio.Group>
          </Form.Item>

          <Form.Item name="amount" label="调账金额" rules={[{ required: true, message: '请输入调账金额' }]}>
            <InputNumber min={0} precision={2} style={{ width: '100%' }} placeholder="0.00" />
          </Form.Item>

          <Form.Item name="is_increase" label="调账方向" rules={[{ required: true }]}>
            <Radio.Group>
              <Radio value={true}>增加余额</Radio>
              <Radio value={false}>减少余额</Radio>
            </Radio.Group>
          </Form.Item>

          <Form.Item name="reason" label="调账原因" rules={[{ required: true, message: '请输入调账原因' }]}>
            <Input.TextArea rows={4} placeholder="请输入调账原因" />
          </Form.Item>

          <Form.Item name="approval_no" label="审批单号" rules={[{ required: true, message: '请输入审批单号' }]}>
            <Input placeholder="请输入审批单号" />
          </Form.Item>

          <Form.Item name="operator" label="操作员">
            <Input placeholder="可选，默认 admin" />
          </Form.Item>

          <Form.Item>
            <Button type="primary" htmlType="submit" loading={loading}>
              提交调账申请
            </Button>
          </Form.Item>
        </Form>
      </Card>

      {result && (
        <Card title="调账结果">
          <Descriptions column={2} bordered size="small">
            <Descriptions.Item label="交易ID">{result.transaction_id}</Descriptions.Item>
            <Descriptions.Item label="凭证号">{result.voucher_no}</Descriptions.Item>
            <Descriptions.Item label="调账前余额">{displayMoney(result.balance_before, FALLBACK_CURRENCY)}</Descriptions.Item>
            <Descriptions.Item label="调账后余额">{displayMoney(result.balance_after, FALLBACK_CURRENCY)}</Descriptions.Item>
          </Descriptions>
        </Card>
      )}
    </div>
  )
}
