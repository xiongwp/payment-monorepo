import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('account')
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
      message.warning(t('platformAccounts.fleet.messages.needSelect'))
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
      setResultMsg(t('platformAccounts.fleet.messages.resultFleet', {
        created: resp.created,
        businessType: resp.channel_business_type,
        code: resp.business_type_code,
      }))
      if (resp.created === 100) {
        message.success(t('platformAccounts.fleet.messages.allDone'))
      } else {
        message.warning(t('platformAccounts.fleet.messages.partialDone', { created: resp.created }))
      }
      loadRegistry()
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('platformAccounts.fleet.messages.createFailed'))
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
        message={t('platformAccounts.fleet.alertTitle')}
        description={
          <>
            <div>{t('platformAccounts.fleet.alertLine1')}</div>
            <div style={{ marginTop: 6 }}>
              {t('platformAccounts.fleet.alertLine2Prefix')}<b>{t('platformAccounts.fleet.alertLine2Bold')}</b>{t('platformAccounts.fleet.alertLine2Suffix')}
            </div>
          </>
        }
      />
      <Form form={form} layout="vertical" onFinish={onFinish} initialValues={{ currency: 'PHP' }}>
        <Form.Item
          label={t('platformAccounts.fleet.selectLabel')}
          name="channel_business_type"
          rules={[{ required: true, message: t('platformAccounts.fleet.selectRequired') }]}
        >
          <Select
            placeholder={registry.length ? t('platformAccounts.fleet.selectPlaceholderHas') : t('platformAccounts.fleet.selectPlaceholderEmpty')}
            options={btOptions}
            showSearch
            optionFilterProp="label"
          />
        </Form.Item>

        {selectedRow && (
          <Descriptions bordered size="small" column={2} style={{ marginBottom: 12 }}>
            <Descriptions.Item label={t('platformAccounts.fleet.descriptions.accountType')}>
              {ACCOUNT_TYPE_LABEL[selectedRow.account_type] ?? '?'} ({selectedRow.account_type})
            </Descriptions.Item>
            <Descriptions.Item label={t('platformAccounts.fleet.descriptions.category')}>
              {CATEGORY_LABEL[CATEGORY_BY_ACCOUNT_TYPE[selectedRow.account_type]] ?? '?'}
            </Descriptions.Item>
            <Descriptions.Item label={t('platformAccounts.fleet.descriptions.code')}><Text code>{selectedRow.business_type_code}</Text></Descriptions.Item>
            <Descriptions.Item label={t('platformAccounts.fleet.descriptions.description')}>{selectedRow.description || '-'}</Descriptions.Item>
          </Descriptions>
        )}

        <Form.Item label={t('platformAccounts.fleet.currencyLabel')} name="currency">
          <Input placeholder="PHP" />
        </Form.Item>
        <Form.Item>
          <Button type="primary" htmlType="submit" loading={loading} disabled={!selectedRow}>
            {t('platformAccounts.fleet.submitButton')}
          </Button>
        </Form.Item>
      </Form>
      {resultMsg && <Alert type="success" showIcon message={resultMsg} />}
    </Card>
  )
}

// ─── 单点创建面板 ─────────────────────────────────────────────────────────────

function CreateSingleAccountPanel() {
  const { t } = useTranslation('account')
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
      message.success(t('platformAccounts.single.messages.createSuccess', { accountNo: acc.account_no }))
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('platformAccounts.single.messages.createFailed'))
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
        message={t('platformAccounts.single.alertTitle')}
        description={t('platformAccounts.single.alertDescription')}
      />
      <Form form={form} layout="vertical" onFinish={onFinish} initialValues={{ currency: 'PHP' }}>
        <Form.Item label={t('platformAccounts.single.reservedIdLabel')} name="reserved_id" rules={[{ required: true, type: 'number', min: 1, max: 10000 }]}>
          <InputNumber min={1} max={10000} style={{ width: '100%' }} placeholder={t('platformAccounts.single.reservedIdPlaceholder')} />
        </Form.Item>
        <Form.Item label={t('platformAccounts.single.accountTypeLabel')} name="account_type" rules={[{ required: true }]}>
          <Select
            placeholder={t('platformAccounts.single.accountTypePlaceholder')}
            options={PLATFORM_ACCOUNT_TYPES.map(t => ({ value: t.value, label: t.label }))}
          />
        </Form.Item>
        <Form.Item label={t('platformAccounts.single.currencyLabel')} name="currency">
          <Input placeholder="PHP" />
        </Form.Item>
        <Form.Item>
          <Button type="primary" htmlType="submit" loading={loading}>{t('platformAccounts.single.submitButton')}</Button>
        </Form.Item>
      </Form>
      {created && (
        <Descriptions title={t('platformAccounts.single.resultTitle')} bordered column={1} size="small">
          <Descriptions.Item label={t('platformAccounts.single.fields.accountNo')}>{created.account_no}</Descriptions.Item>
          <Descriptions.Item label={t('platformAccounts.single.fields.userId')}>{created.user_id}</Descriptions.Item>
          <Descriptions.Item label={t('platformAccounts.single.fields.accountType')}>{created.account_type}</Descriptions.Item>
          <Descriptions.Item label={t('platformAccounts.single.fields.businessType')}>{created.account_business_type}</Descriptions.Item>
          <Descriptions.Item label={t('platformAccounts.single.fields.category')}>{created.account_category}</Descriptions.Item>
          <Descriptions.Item label={t('platformAccounts.single.fields.currency')}>{created.currency}</Descriptions.Item>
        </Descriptions>
      )}
    </Card>
  )
}

// ─── 页面主容器 ────────────────────────────────────────────────────────────────

export default function PlatformAccounts() {
  const { t } = useTranslation('account')
  return (
    <div>
      <Title level={3}>{t('platformAccounts.pageTitle')}</Title>
      <Paragraph type="secondary">
        {t('platformAccounts.pageDescription')}
      </Paragraph>

      <Tabs
        defaultActiveKey="fleet"
        items={[
          {
            key: 'fleet',
            label: t('platformAccounts.tabs.fleet'),
            children: <CreateChannelFleetPanel />,
          },
          {
            key: 'single',
            label: t('platformAccounts.tabs.single'),
            children: <CreateSingleAccountPanel />,
          },
        ]}
      />
    </div>
  )
}
