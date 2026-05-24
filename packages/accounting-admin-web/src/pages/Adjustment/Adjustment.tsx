import { Form, Input, InputNumber, Radio, Button, Card, message, Descriptions } from 'antd'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { adjustBalance } from '../../api/accounting'
import type { AdjustBalanceRequest, AdjustBalanceResponse } from '../../types/accounting'
import { display as displayMoney } from '../../utils/money'

// AdjustBalanceResponse does not carry a currency field, and the UI has no
// per-account currency context at this point (the form is keyed by
// account_no). Fall back to PHP which is the system-wide default; if a future
// backend version starts returning currency, prefer that.
const FALLBACK_CURRENCY = 'PHP'

export default function Adjustment() {
  const { t } = useTranslation('config')
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
      message.success(t('adjustment.messages.success'))
      form.resetFields()
    } catch (err: unknown) {
      const msg = (err as { message?: string })?.message ?? t('adjustment.messages.failed')
      message.error(msg)
    } finally {
      setLoading(false)
    }
  }

  return (
    <div>
      <Card title={t('adjustment.formTitle')} style={{ marginBottom: 16 }}>
        <Form
          form={form}
          layout="vertical"
          onFinish={handleSubmit}
          style={{ maxWidth: 600 }}
          initialValues={{ adjustment_type: 'MANUAL', is_increase: true }}
        >
          <Form.Item name="account_no" label={t('adjustment.form.accountNoLabel')} rules={[{ required: true, message: t('adjustment.form.accountNoRequired') }]}>
            <Input placeholder={t('adjustment.form.accountNoPlaceholder')} />
          </Form.Item>

          <Form.Item name="adjustment_type" label={t('adjustment.form.typeLabel')} rules={[{ required: true }]}>
            <Radio.Group>
              <Radio value="CORRECTION">{t('adjustment.form.typeCorrection')}</Radio>
              <Radio value="COMPENSATE">{t('adjustment.form.typeCompensate')}</Radio>
              <Radio value="MANUAL">{t('adjustment.form.typeManual')}</Radio>
            </Radio.Group>
          </Form.Item>

          <Form.Item name="amount" label={t('adjustment.form.amountLabel')} rules={[{ required: true, message: t('adjustment.form.amountRequired') }]}>
            <InputNumber min={0} precision={2} style={{ width: '100%' }} placeholder={t('adjustment.form.amountPlaceholder')} />
          </Form.Item>

          <Form.Item name="is_increase" label={t('adjustment.form.directionLabel')} rules={[{ required: true }]}>
            <Radio.Group>
              <Radio value={true}>{t('adjustment.form.directionIncrease')}</Radio>
              <Radio value={false}>{t('adjustment.form.directionDecrease')}</Radio>
            </Radio.Group>
          </Form.Item>

          <Form.Item name="reason" label={t('adjustment.form.reasonLabel')} rules={[{ required: true, message: t('adjustment.form.reasonRequired') }]}>
            <Input.TextArea rows={4} placeholder={t('adjustment.form.reasonPlaceholder')} />
          </Form.Item>

          <Form.Item name="approval_no" label={t('adjustment.form.approvalLabel')} rules={[{ required: true, message: t('adjustment.form.approvalRequired') }]}>
            <Input placeholder={t('adjustment.form.approvalPlaceholder')} />
          </Form.Item>

          <Form.Item name="operator" label={t('adjustment.form.operatorLabel')}>
            <Input placeholder={t('adjustment.form.operatorPlaceholder')} />
          </Form.Item>

          <Form.Item>
            <Button type="primary" htmlType="submit" loading={loading}>
              {t('adjustment.submitButton')}
            </Button>
          </Form.Item>
        </Form>
      </Card>

      {result && (
        <Card title={t('adjustment.resultTitle')}>
          <Descriptions column={2} bordered size="small">
            <Descriptions.Item label={t('adjustment.result.transactionId')}>{result.transaction_id}</Descriptions.Item>
            <Descriptions.Item label={t('adjustment.result.voucherNo')}>{result.voucher_no}</Descriptions.Item>
            <Descriptions.Item label={t('adjustment.result.balanceBefore')}>{displayMoney(result.balance_before, FALLBACK_CURRENCY)}</Descriptions.Item>
            <Descriptions.Item label={t('adjustment.result.balanceAfter')}>{displayMoney(result.balance_after, FALLBACK_CURRENCY)}</Descriptions.Item>
          </Descriptions>
        </Card>
      )}
    </div>
  )
}
