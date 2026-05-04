import { useEffect, useState } from 'react'
import {
  Alert, Button, Card, Form, Input, Modal, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import { Link } from 'react-router-dom'
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
      <Typography.Title level={3}>商户管理</Typography.Title>
      <Card>
        <Space wrap style={{ marginBottom: 16 }}>
          <Select
            placeholder="业务状态" allowClear style={{ width: 140 }}
            onChange={(v) => setFilters((f) => ({ ...f, status: v }))}
            options={[
              { value: 'pending', label: 'pending' },
              { value: 'active', label: 'active' },
              { value: 'suspended', label: 'suspended' },
              { value: 'terminated', label: 'terminated' },
            ]}
          />
          <Select
            placeholder="KYC 状态" allowClear style={{ width: 180 }}
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
          <Button onClick={load}>刷新</Button>
          <Button type="primary" onClick={() => setCreateOpen(true)}>新建商户</Button>
        </Space>
        <Table<Merchant>
          rowKey="id"
          loading={loading}
          dataSource={rows}
          size="small"
          pagination={{ pageSize: 20 }}
          columns={[
            { title: 'ID', dataIndex: 'id', width: 140, render: (v) => <Link to={`/merchants/${v}`}>{v}</Link> },
            { title: '名称', dataIndex: 'name', ellipsis: true },
            { title: '邮箱', dataIndex: 'contact_email', ellipsis: true },
            { title: '国家', dataIndex: 'country', width: 70 },
            {
              title: 'KYC', dataIndex: 'kyc_status', width: 140,
              render: (v: string) => <Tag color={KYC_COLORS[v] || 'default'}>{v}</Tag>,
            },
            {
              title: '状态', dataIndex: 'status', width: 100,
              render: (v: string) => <Tag color={STATUS_COLORS[v] || 'default'}>{v}</Tag>,
            },
            {
              title: '风控', dataIndex: 'risk_tier', width: 100,
              render: (v: string) => <Tag>{v}</Tag>,
            },
            {
              title: '创建时间', dataIndex: 'created_ms', width: 170,
              render: (v: number) => new Date(v).toLocaleString(),
            },
          ]}
        />
      </Card>

      <Modal
        title="新建商户"
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={() => createForm.submit()}
        okText="创建"
        width={640}
      >
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message="创建成功后会返回 live/test API key 和 webhook secret 明文；仅显示一次，请立即妥善保存。"
        />
        <Form
          form={createForm} layout="vertical" onFinish={onCreate}
          initialValues={{ country: 'PH', settle_currency: 'PHP', business_type: 'individual' }}
        >
          <Form.Item name="name" label="名称" rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="legal_name" label="注册主体全称">
            <Input />
          </Form.Item>
          <Form.Item name="contact_email" label="联系邮箱" rules={[{ required: true, type: 'email' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="contact_phone" label="联系电话"><Input /></Form.Item>
          <Form.Item name="country" label="国家">
            <Select options={[{ value: 'PH', label: 'Philippines' }]} />
          </Form.Item>
          <Form.Item name="business_type" label="主体类型">
            <Select options={[
              { value: 'individual', label: 'individual' },
              { value: 'corporate', label: 'corporate' },
              { value: 'non_profit', label: 'non_profit' },
              { value: 'government', label: 'government' },
            ]} />
          </Form.Item>
          <Form.Item name="tax_id" label="TIN / SEC / DTI"><Input /></Form.Item>
          <Form.Item name="mcc" label="MCC (ISO 18245)"><Input maxLength={4} /></Form.Item>
          <Form.Item name="webhook_url" label="Webhook URL">
            <Input placeholder="https://merchant.example.com/webhooks/payments" />
          </Form.Item>
          <Form.Item name="settle_method" label="结算方式">
            <Select allowClear options={[
              { value: 'bank_transfer', label: 'bank_transfer' },
              { value: 'wallet_topup', label: 'wallet_topup' },
              { value: 'manual', label: 'manual' },
            ]} />
          </Form.Item>
          <Form.Item name="settle_account" label="结算账户"><Input /></Form.Item>
          <Form.Item name="settle_bank" label="结算银行"><Input /></Form.Item>
          <Form.Item name="settle_holder" label="户名"><Input /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title="商户已创建 — 请妥善保存以下凭据"
        open={!!secretsDialog}
        onCancel={() => setSecretsDialog(null)}
        onOk={() => setSecretsDialog(null)}
        okText="我已保存"
        cancelButtonProps={{ style: { display: 'none' } }}
        width={720}
      >
        {secretsDialog && (
          <div>
            <Alert
              type="warning" showIcon style={{ marginBottom: 16 }}
              message="以下凭据只显示一次。关闭后将无法再次查看；如需重置请使用 rotate-key。"
            />
            <SecretRow label="商户 ID" value={secretsDialog.merchant?.id} />
            <SecretRow label="Live Secret Key" value={secretsDialog.live_secret_key} />
            <SecretRow label="Test Secret Key" value={secretsDialog.test_secret_key} />
            <SecretRow label="Webhook Secret" value={secretsDialog.webhook_secret} />
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
