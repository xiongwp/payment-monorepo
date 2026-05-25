import { useEffect, useState } from 'react'
import { Table, Tag, Button, Space, Typography, message, Alert, Input, Modal, Form, Select, InputNumber } from 'antd'
import { ReloadOutlined, PlusOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useTranslation, Trans } from 'react-i18next'
import type { BusinessTypeInfo } from '../../types/accounting'
import {
  listBusinessTypes,
  registerBusinessType,
  ALL_ACCOUNT_TYPES,
  CATEGORY_BY_ACCOUNT_TYPE,
  CATEGORY_LABEL,
  ACCOUNT_TYPE_LABEL,
} from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

// 从 platformAccountSpec 的 category 派生的常量（同系统 Go 代码）
const CATEGORY_COLOR: Record<string, string> = {
  ASSET: 'blue',
  LIABILITY: 'green',
  EQUITY: 'purple',
  REVENUE: 'orange',
  EXPENSE: 'red',
}

export default function BusinessTypes() {
  const { t } = useTranslation('business')
  const [rows, setRows] = useState<BusinessTypeInfo[]>([])
  const [loading, setLoading] = useState(false)
  const [keyword, setKeyword] = useState('')
  const [modalOpen, setModalOpen] = useState(false)
  const [creating, setCreating] = useState(false)
  const [form] = Form.useForm()

  const load = () => {
    setLoading(true)
    listBusinessTypes()
      .then(setRows)
      .catch((e) => message.error(e instanceof Error ? e.message : t('messages.loadFailed')))
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  const filtered = rows.filter((r) => {
    if (!keyword.trim()) return true
    const k = keyword.trim().toLowerCase()
    return (
      String(r.business_type).includes(k) ||
      r.business_type_code.toLowerCase().includes(k) ||
      (r.description ?? '').toLowerCase().includes(k)
    )
  })

  const columns: ColumnsType<BusinessTypeInfo> = [
    { title: t('columns.businessType'), dataIndex: 'business_type', key: 'business_type', width: 130, sorter: (a, b) => a.business_type - b.business_type, defaultSortOrder: 'ascend' },
    { title: t('columns.code'), dataIndex: 'business_type_code', key: 'business_type_code', width: 240 },
    {
      title: t('columns.accountType'), dataIndex: 'account_type', key: 'account_type', width: 220,
      // 显示"名称 (数字)"，便于运营识别；过滤仍按数字
      render: (v: number) => `${ACCOUNT_TYPE_LABEL[v] ?? t('unknown')} (${v})`,
      filters: Array.from(new Set(rows.map(r => r.account_type))).map(v => ({
        text: `${ACCOUNT_TYPE_LABEL[v] ?? t('unknown')} (${v})`, value: v,
      })),
      onFilter: (v, r) => r.account_type === v,
    },
    {
      // category 不存表,由 account_type 派生(与后端 CategoryForAccountType 同步)
      title: t('columns.categoryDerived'), key: 'category', width: 160,
      render: (_, r) => {
        const catNum = CATEGORY_BY_ACCOUNT_TYPE[r.account_type]
        const label = CATEGORY_LABEL[catNum] ?? t('unknown')
        return <Tag color={CATEGORY_COLOR[label] || 'default'}>{label}</Tag>
      },
    },
    { title: t('columns.description'), dataIndex: 'description', key: 'description', render: (v: string | null) => v || '-' },
    {
      title: t('columns.status'), dataIndex: 'enabled', key: 'enabled', width: 80,
      render: (v: number) => v === 1 ? <Tag color="green">{t('status.enabled')}</Tag> : <Tag color="red">{t('status.disabled')}</Tag>,
      filters: [{ text: t('status.enabled'), value: 1 }, { text: t('status.disabled'), value: 0 }],
      onFilter: (v, r) => r.enabled === v,
    },
  ]

  return (
    <div>
      <Title level={3}>{t('title')}</Title>
      <Paragraph type="secondary">
        <Trans i18nKey="description.line1" ns="business" components={{ code: <Text code /> }} />
        <br />
        <Trans i18nKey="description.line2" ns="business" components={{ b: <b /> }} />
        <br />
        {t('description.line3')}
      </Paragraph>

      <Alert
        style={{ marginBottom: 16 }}
        type="info"
        showIcon
        message={t('alert.message')}
        description={t('alert.description')}
      />

      <Space style={{ marginBottom: 12 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setModalOpen(true)}>{t('addButton')}</Button>
        <Input.Search
          allowClear
          placeholder={t('searchPlaceholder')}
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          style={{ width: 360 }}
        />
        <Button icon={<ReloadOutlined />} onClick={load}>{t('common:actions.refresh')}</Button>
        <span style={{ color: '#888' }}>{t('totalCount', { count: filtered.length })}</span>
      </Space>

      <Modal
        title={t('createModal.title')}
        open={modalOpen}
        confirmLoading={creating}
        onCancel={() => { setModalOpen(false); form.resetFields() }}
        onOk={async () => {
          try {
            const values = await form.validateFields()
            setCreating(true)
            const info = await registerBusinessType({
              account_type: values.account_type,
              business_type_code: String(values.business_type_code).trim(),
              description: values.description,
              business_type: values.business_type || 0,
            })
            message.success(t('messages.registered', { businessType: info.business_type, code: info.business_type_code }))
            setModalOpen(false)
            form.resetFields()
            load()
          } catch (e) {
            if (e instanceof Error) message.error(e.message)
          } finally {
            setCreating(false)
          }
        }}
        width={560}
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message={t('createModal.alertMessage')}
          description={
            <Trans i18nKey="createModal.alertDescription" ns="business" components={{ code: <Text code /> }} />
          }
        />
        <Form form={form} layout="vertical" initialValues={{ business_type: 0 }}>
          <Form.Item label={t('createModal.form.accountTypeLabel')} name="account_type" rules={[{ required: true }]}>
            <Select
              placeholder={t('createModal.form.accountTypePlaceholder')}
              options={ALL_ACCOUNT_TYPES.map(at => ({ value: at.value, label: at.label }))}
            />
          </Form.Item>
          <Form.Item
            label={t('createModal.form.codeLabel')}
            name="business_type_code"
            rules={[{ required: true, message: t('createModal.form.codeRequired') }]}
          >
            <Input placeholder={t('createModal.form.codePlaceholder')} />
          </Form.Item>
          <Form.Item label={t('createModal.form.descLabel')} name="description" rules={[{ required: true }]}>
            <Input.TextArea rows={2} placeholder={t('createModal.form.descPlaceholder')} />
          </Form.Item>
          <Form.Item
            label={t('createModal.form.businessTypeLabel')}
            name="business_type"
            tooltip={t('createModal.form.businessTypeTooltip')}
          >
            <InputNumber min={0} max={999} style={{ width: '100%' }} placeholder={t('createModal.form.businessTypePlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>

      <Table
        rowKey="id"
        size="small"
        loading={loading}
        columns={columns}
        dataSource={filtered}
        pagination={{ pageSize: 30, showSizeChanger: true }}
      />
    </div>
  )
}
