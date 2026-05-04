import { useEffect, useMemo, useState } from 'react'
import {
  Card, Form, InputNumber, Select, Button, Input, message,
  Descriptions, Alert, Typography, Tabs,
} from 'antd'
import type { Account, BusinessTypeInfo } from '../../types/accounting'
import {
  PLATFORM_ACCOUNT_TYPES,
  ACCOUNT_TYPE_LABEL,
  CATEGORY_BY_ACCOUNT_TYPE,
  CATEGORY_LABEL,
  createPlatformAccount,
  createPlatformAccountFleet,
  listBusinessTypes,
  isPlatformAccountType,
} from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

// ─── 基于已登记 business_type 批量建 100 账户 ─────────────────────────────────

function CreateChannelFleetPanel() {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [registry, setRegistry] = useState<BusinessTypeInfo[]>([])
  const [resultMsg, setResultMsg] = useState<string | null>(null)

  const loadRegistry = () => {
    // 只筛平台内部账户类型对应的 business_type；账户管理页的用户/商户 business_type 不出现在此
    listBusinessTypes()
      .then((rows) => setRegistry(rows.filter(r => isPlatformAccountType(r.account_type))))
      .catch(() => setRegistry([]))
  }
  useEffect(loadRegistry, [])

  // 选中的 business_type → 对应 registry 行（用来回填 account_type + derived category）
  const selectedBt = Form.useWatch('channel_business_type', form) as number | undefined
  const selectedRow = useMemo(
    () => registry.find(r => r.business_type === selectedBt),
    [registry, selectedBt],
  )

  const onFinish = async (values: {
    channel_business_type: number
    currency?: string
  }) => {
    if (!selectedRow) {
      message.warning('请先选择一个已登记的 business_type')
      return
    }
    setLoading(true)
    setResultMsg(null)
    try {
      const resp = await createPlatformAccountFleet({
        account_type: selectedRow.account_type,
        channel_business_type: values.channel_business_type,
        currency: values.currency || 'PHP',
      })
      setResultMsg(`已创建 ${resp.created}/100 个分片账户 · business_type=${resp.channel_business_type} · ${resp.business_type_code}`)
      if (resp.created === 100) {
        message.success(`账户创建完成`)
      } else {
        message.warning(`部分成功 ${resp.created}/100，可用相同参数重跑补齐`)
      }
      loadRegistry()
    } catch (e) {
      message.error(e instanceof Error ? e.message : '创建失败')
    } finally {
      setLoading(false)
    }
  }

  // 下拉候选：仅 registry 里已登记的 business_type（无"自动分配"）
  const btOptions = registry
    .filter(r => r.enabled === 1)
    .map(r => ({
      value: r.business_type,
      label: `${r.business_type} · ${r.business_type_code} [${ACCOUNT_TYPE_LABEL[r.account_type] ?? '?'}]`,
    }))

  return (
    <Card>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="基于已登记 business_type 创建 100 个分片账户"
        description={
          <>
            <div>先到"业务类型"页登记一条 business_type，再到这里选它来创建 100 个分片账户（user_id = 0..99）。</div>
            <div style={{ marginTop: 6 }}>
              本操作只建账户，<b>不再注册 registry</b>；所以下拉里如果没看到你要的 business_type，先去登记。
              相同参数重跑幂等：只补齐缺失分片。
            </div>
          </>
        }
      />
      <Form form={form} layout="vertical" onFinish={onFinish} initialValues={{ currency: 'PHP' }}>
        <Form.Item
          label="选择已登记的 Business Type"
          name="channel_business_type"
          rules={[{ required: true, message: '请选择 business_type' }]}
        >
          <Select
            placeholder={registry.length ? '选择已在 registry 登记的 business_type' : '请先到"业务类型"页注册'}
            options={btOptions}
            showSearch
            optionFilterProp="label"
          />
        </Form.Item>

        {selectedRow && (
          <Descriptions bordered size="small" column={2} style={{ marginBottom: 12 }}>
            <Descriptions.Item label="Account Type">
              {ACCOUNT_TYPE_LABEL[selectedRow.account_type] ?? '?'} ({selectedRow.account_type})
            </Descriptions.Item>
            <Descriptions.Item label="Category (derived)">
              {CATEGORY_LABEL[CATEGORY_BY_ACCOUNT_TYPE[selectedRow.account_type]] ?? '?'}
            </Descriptions.Item>
            <Descriptions.Item label="Code"><Text code>{selectedRow.business_type_code}</Text></Descriptions.Item>
            <Descriptions.Item label="描述">{selectedRow.description || '-'}</Descriptions.Item>
          </Descriptions>
        )}

        <Form.Item label="币种" name="currency">
          <Input placeholder="PHP" />
        </Form.Item>
        <Form.Item>
          <Button type="primary" htmlType="submit" loading={loading} disabled={!selectedRow}>
            创建 100 个账户
          </Button>
        </Form.Item>
      </Form>
      {resultMsg && <Alert type="success" showIcon message={resultMsg} />}
    </Card>
  )
}

// ─── 单点创建面板 ─────────────────────────────────────────────────────────────

function CreateSingleAccountPanel() {
  const [form] = Form.useForm()
  const [loading, setLoading] = useState(false)
  const [created, setCreated] = useState<Account | null>(null)

  const onFinish = async (values: { reserved_id: number; account_type: number; currency: string }) => {
    setLoading(true)
    setCreated(null)
    try {
      const acc = await createPlatformAccount({
        reserved_id: values.reserved_id,
        account_type: values.account_type,
        currency: values.currency || 'PHP',
      })
      setCreated(acc)
      message.success(`账户已创建: ${acc.account_no}`)
    } catch (e) {
      message.error(e instanceof Error ? e.message : '创建失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <Card>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="单点创建（少用）"
        description='只为某一分片补一个账户。大多数情况应该用"渠道注册"批量建 100 个。'
      />
      <Form form={form} layout="vertical" onFinish={onFinish} initialValues={{ currency: 'PHP' }}>
        <Form.Item label="Reserved ID (1-10000)" name="reserved_id" rules={[{ required: true, type: 'number', min: 1, max: 10000 }]}>
          <InputNumber min={1} max={10000} style={{ width: '100%' }} placeholder="该账户在保留段的 owner_id" />
        </Form.Item>
        <Form.Item label="账户类型" name="account_type" rules={[{ required: true }]}>
          <Select
            placeholder="选择平台账户类型"
            options={PLATFORM_ACCOUNT_TYPES.map(t => ({ value: t.value, label: t.label }))}
          />
        </Form.Item>
        <Form.Item label="币种" name="currency">
          <Input placeholder="PHP" />
        </Form.Item>
        <Form.Item>
          <Button type="primary" htmlType="submit" loading={loading}>创建账户</Button>
        </Form.Item>
      </Form>
      {created && (
        <Descriptions title="创建结果" bordered column={1} size="small">
          <Descriptions.Item label="账户号">{created.account_no}</Descriptions.Item>
          <Descriptions.Item label="User ID">{created.user_id}</Descriptions.Item>
          <Descriptions.Item label="Account Type">{created.account_type}</Descriptions.Item>
          <Descriptions.Item label="Business Type">{created.account_business_type}</Descriptions.Item>
          <Descriptions.Item label="Category">{created.account_category}</Descriptions.Item>
          <Descriptions.Item label="Currency">{created.currency}</Descriptions.Item>
        </Descriptions>
      )}
    </Card>
  )
}

// ─── 页面主容器 ────────────────────────────────────────────────────────────────

export default function PlatformAccounts() {
  return (
    <div>
      <Title level={3}>系统账户管理</Title>
      <Paragraph type="secondary">
        管理平台 / 中间 / 手续费 / 权益 / 中转等系统内部账户。owner_id 预留 [1, 10000]；用户 base 1e8、商户 base 9e8。
        每个渠道对应 100 个分片账户（user_id = 0..99），business_type 唯一区分渠道。
        业务类型清单与审计请到"业务类型"页面。
      </Paragraph>

      <Tabs
        defaultActiveKey="fleet"
        items={[
          {
            key: 'fleet',
            label: '渠道注册（100 账户）',
            children: <CreateChannelFleetPanel />,
          },
          {
            key: 'single',
            label: '单点创建',
            children: <CreateSingleAccountPanel />,
          },
        ]}
      />
    </div>
  )
}
