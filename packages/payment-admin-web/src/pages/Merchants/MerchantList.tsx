import { useEffect, useState } from 'react'
import {
  Alert, Button, Card, Form, Input, Modal, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { createMerchant, listMerchants } from '../../api'
import type { Merchant, CreateMerchantResponse } from '../../api'

const KYC_COLORS: Record<string, string> = {
  pending: 'default',
  submitted: 'blue',
  reviewing: 'geekblue',
  needs_more_info: 'gold',
  approved: 'green',
  rejected: 'red',
  suspended: 'orange',
  terminated: 'red',
}

const STATUS_COLORS: Record<string, string> = {
  pending: 'default',
  active: 'green',
  suspended: 'orange',
  terminated: 'red',
}

export default function MerchantList() {
  const { t } = useTranslation('merchant')
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<Merchant[]>([])
  const [filters, setFilters] = useState<{ status?: string; kyc_status?: string }>({})
  const [createOpen, setCreateOpen] = useState(false)
  const [createForm] = Form.useForm()
  const [secretsDialog, setSecretsDialog] = useState<CreateMerchantResponse | null>(null)

  const load = async () => {
    setLoading(true)
    try {
      const r = await listMerchants({ ...filters, limit: 100 })
      setRows(r.items || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [filters.status, filters.kyc_status]) // eslint-disable-line

  const onCreate = async (v: Partial<Merchant>) => {
    try {
      const r = await createMerchant({
        ...v,
        country: v.country || 'PH',
        settle_currency: v.settle_currency || 'PHP',
        business_type: v.business_type || 'individual',
      })
      setSecretsDialog(r)
      setCreateOpen(false)
      createForm.resetFields()
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  return (
    <div>
      <Typography.Title level={3}>{t('list.title')}</Typography.Title>
      <Card>
        <Space wrap style={{ marginBottom: 16 }}>
          <Select
            placeholder={t('list.filters.statusPlaceholder')} allowClear style={{ width: 140 }}
            onChange={(v) => setFilters((f) => ({ ...f, status: v }))}
            options={[
              { value: 'pending', label: 'pending' },
              { value: 'active', label: 'active' },
              { value: 'suspended', label: 'suspended' },
              { value: 'terminated', label: 'terminated' },
            ]}
          />
          <Select
            placeholder={t('list.filters.kycStatusPlaceholder')} allowClear style={{ width: 180 }}
            onChange={(v) => setFilters((f) => ({ ...f, kyc_status: v }))}
            options={[
              { value: 'pending', label: 'pending' },
              { value: 'submitted', label: 'submitted' },
              { value: 'reviewing', label: 'reviewing' },
              { value: 'needs_more_info', label: 'needs_more_info' },
              { value: 'approved', label: 'approved' },
              { value: 'rejected', label: 'rejected' },
              { value: 'suspended', label: 'suspended' },
              { value: 'terminated', label: 'terminated' },
            ]}
          />
          <Button onClick={load}>{t('common:actions.refresh')}</Button>
          <Button type="primary" onClick={() => setCreateOpen(true)}>{t('list.actions.create')}</Button>
        </Space>
        <Table<Merchant>
          rowKey="id"
          loading={loading}
          dataSource={rows}
          size="small"
          pagination={{ pageSize: 20 }}
          columns={[
            { title: t('list.columns.id'), dataIndex: 'id', width: 140, render: (v) => <Link to={`/merchants/${v}`}>{v}</Link> },
            { title: t('list.columns.name'), dataIndex: 'name', ellipsis: true },
            { title: t('list.columns.email'), dataIndex: 'contact_email', ellipsis: true },
            { title: t('list.columns.country'), dataIndex: 'country', width: 70 },
            {
              title: t('list.columns.kyc'), dataIndex: 'kyc_status', width: 140,
              render: (v: string) => <Tag color={KYC_COLORS[v] || 'default'}>{v}</Tag>,
            },
            {
              title: t('list.columns.status'), dataIndex: 'status', width: 100,
              render: (v: string) => <Tag color={STATUS_COLORS[v] || 'default'}>{v}</Tag>,
            },
            {
              title: t('list.columns.riskTier'), dataIndex: 'risk_tier', width: 100,
              render: (v: string) => <Tag>{v}</Tag>,
            },
            {
              title: t('list.columns.createdAt'), dataIndex: 'created_ms', width: 170,
              render: (v: number) => new Date(v).toLocaleString(),
            },
          ]}
        />
      </Card>

      <Modal
        title={t('list.createModal.title')}
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={() => createForm.submit()}
        okText={t('list.createModal.okText')}
        width={640}
      >
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message={t('list.createModal.alert')}
        />
        <Form
          form={createForm} layout="vertical" onFinish={onCreate}
          initialValues={{ country: 'PH', settle_currency: 'PHP', business_type: 'individual' }}
        >
          <Form.Item name="name" label={t('list.createModal.fields.name')} rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="legal_name" label={t('list.createModal.fields.legalName')}>
            <Input />
          </Form.Item>
          <Form.Item name="contact_email" label={t('list.createModal.fields.contactEmail')} rules={[{ required: true, type: 'email' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="contact_phone" label={t('list.createModal.fields.contactPhone')}><Input /></Form.Item>
          <Form.Item name="country" label={t('list.createModal.fields.country')}>
            <Select options={[{ value: 'PH', label: 'Philippines' }]} />
          </Form.Item>
          <Form.Item name="business_type" label={t('list.createModal.fields.businessType')}>
            <Select options={[
              { value: 'individual', label: 'individual' },
              { value: 'corporate', label: 'corporate' },
              { value: 'non_profit', label: 'non_profit' },
              { value: 'government', label: 'government' },
            ]} />
          </Form.Item>
          <Form.Item name="tax_id" label={t('list.createModal.fields.taxId')}><Input /></Form.Item>
          <Form.Item name="mcc" label={t('list.createModal.fields.mcc')}><Input maxLength={4} /></Form.Item>
          <Form.Item name="webhook_url" label={t('list.createModal.fields.webhookUrl')}>
            <Input placeholder={t('list.createModal.fields.webhookUrlPlaceholder')} />
          </Form.Item>
          <Form.Item name="settle_method" label={t('list.createModal.fields.settleMethod')}>
            <Select allowClear options={[
              { value: 'bank_transfer', label: 'bank_transfer' },
              { value: 'wallet_topup', label: 'wallet_topup' },
              { value: 'manual', label: 'manual' },
            ]} />
          </Form.Item>
          <Form.Item name="settle_account" label={t('list.createModal.fields.settleAccount')}><Input /></Form.Item>
          <Form.Item name="settle_bank" label={t('list.createModal.fields.settleBank')}><Input /></Form.Item>
          <Form.Item name="settle_holder" label={t('list.createModal.fields.settleHolder')}><Input /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('list.secretsModal.title')}
        open={!!secretsDialog}
        onCancel={() => setSecretsDialog(null)}
        onOk={() => setSecretsDialog(null)}
        okText={t('list.secretsModal.okText')}
        cancelButtonProps={{ style: { display: 'none' } }}
        width={720}
      >
        {secretsDialog && (
          <div>
            <Alert
              type="warning" showIcon style={{ marginBottom: 16 }}
              message={t('list.secretsModal.alert')}
            />
            <SecretRow label={t('list.secretsModal.labels.merchantId')} value={secretsDialog.merchant?.id} />
            <SecretRow label={t('list.secretsModal.labels.liveSecretKey')} value={secretsDialog.live_secret_key} />
            <SecretRow label={t('list.secretsModal.labels.testSecretKey')} value={secretsDialog.test_secret_key} />
            <SecretRow label={t('list.secretsModal.labels.webhookSecret')} value={secretsDialog.webhook_secret} />
          </div>
        )}
      </Modal>
    </div>
  )
}

function SecretRow({ label, value }: { label: string; value?: string }) {
  return (
    <div style={{ marginBottom: 12 }}>
      <Typography.Text strong>{label}</Typography.Text>
      <Input.TextArea value={value || ''} readOnly autoSize style={{ fontFamily: 'monospace', marginTop: 4 }} />
    </div>
  )
}
