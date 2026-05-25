import { useEffect, useState } from 'react'
import {
  Table, Button, Modal, Form, Input, Select, message, Space, Typography, Tag, Popconfirm, Alert,
} from 'antd'
import { PlusOutlined, ReloadOutlined, EditOutlined, DeleteOutlined } from '@ant-design/icons'
import { useTranslation, Trans } from 'react-i18next'
import {
  listSystemConfig,
  upsertSystemConfig,
  deleteSystemConfig,
  reloadSystemConfig,
  type SystemConfigItem,
} from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

// validateJSONByType 不严格校验类型契约，只校验 value_json 是合法 JSON。
// （后端 Upsert 也会校验一次，这里前置防止 round-trip 失败）
function validateJSON(s: string): string | null {
  try {
    JSON.parse(s)
    return null
  } catch (e) {
    return e instanceof Error ? e.message : 'invalid JSON'
  }
}

export default function SystemConfig() {
  const { t } = useTranslation('config')
  const [data, setData] = useState<SystemConfigItem[]>([])
  const [loading, setLoading] = useState(false)
  const [modalOpen, setModalOpen] = useState(false)
  const [editing, setEditing] = useState<SystemConfigItem | null>(null)
  const [form] = Form.useForm()

  const VALUE_TYPES = [
    { value: 'int', label: t('system.valueTypes.int') },
    { value: 'string', label: t('system.valueTypes.string') },
    { value: 'bool', label: t('system.valueTypes.bool') },
    { value: 'json', label: t('system.valueTypes.json') },
  ]

  const load = () => {
    setLoading(true)
    listSystemConfig()
      .then((rows) => setData(Array.isArray(rows) ? rows : []))
      .catch((e) => message.error(e instanceof Error ? e.message : t('system.messages.loadFailed')))
      .finally(() => setLoading(false))
  }
  useEffect(load, [])

  const openCreate = () => {
    setEditing(null)
    form.resetFields()
    form.setFieldsValue({ value_type: 'int', value_json: '0', description: '' })
    setModalOpen(true)
  }
  const openEdit = (item: SystemConfigItem) => {
    setEditing(item)
    form.setFieldsValue({
      config_key: item.config_key,
      value_json: item.value_json,
      value_type: item.value_type,
      description: item.description,
    })
    setModalOpen(true)
  }

  const handleOk = async () => {
    try {
      const v = await form.validateFields()
      const err = validateJSON(v.value_json)
      if (err) {
        message.error(t('system.messages.invalidJson', { err }))
        return
      }
      await upsertSystemConfig(v.config_key, v.value_json, v.value_type, v.description || '', 'admin-web')
      message.success(editing ? t('system.messages.updated') : t('system.messages.created'))
      setModalOpen(false)
      load()
    } catch (e) {
      if (e instanceof Error) message.error(e.message)
    }
  }

  const handleDelete = async (key: string) => {
    try {
      await deleteSystemConfig(key)
      message.success(t('system.messages.deleted'))
      load()
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('system.messages.deleteFailed'))
    }
  }

  const handleReloadAll = async () => {
    try {
      await reloadSystemConfig()
      message.success(t('system.messages.reloadAllSuccess'))
      load()
    } catch (e) {
      message.error(e instanceof Error ? e.message : t('system.messages.reloadAllFailed'))
    }
  }

  const columns = [
    {
      title: t('system.columns.key'),
      dataIndex: 'config_key',
      key: 'config_key',
      width: 280,
      render: (v: string) => <Text code>{v}</Text>,
    },
    {
      title: t('system.columns.type'),
      dataIndex: 'value_type',
      key: 'value_type',
      width: 80,
      render: (v: string) => <Tag color={v === 'json' ? 'purple' : v === 'bool' ? 'gold' : 'blue'}>{v || 'string'}</Tag>,
    },
    {
      title: t('system.columns.value'),
      dataIndex: 'value_json',
      key: 'value_json',
      render: (v: string) => <Text code style={{ wordBreak: 'break-all' }}>{v}</Text>,
    },
    {
      title: t('system.columns.description'),
      dataIndex: 'description',
      key: 'description',
    },
    {
      title: t('system.columns.updated'),
      dataIndex: 'updated_at',
      key: 'updated_at',
      width: 170,
      render: (v: string) => v ? new Date(v).toLocaleString() : '-',
    },
    {
      title: t('common:table.actions'),
      key: 'op',
      width: 160,
      render: (_: unknown, item: SystemConfigItem) => (
        <Space>
          <Button icon={<EditOutlined />} size="small" onClick={() => openEdit(item)}>{t('common:actions.edit')}</Button>
          <Popconfirm title={t('system.deleteConfirm', { key: item.config_key })} onConfirm={() => handleDelete(item.config_key)}>
            <Button icon={<DeleteOutlined />} size="small" danger>{t('common:actions.delete')}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Title level={3}>{t('system.title')}</Title>
      <Paragraph type="secondary">
        <Trans
          i18nKey="system.description"
          ns="config"
          components={{ code: <Text code /> }}
        />
      </Paragraph>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={t('system.alert')}
      />

      <Space style={{ marginBottom: 12 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>{t('system.addConfig')}</Button>
        <Button icon={<ReloadOutlined />} onClick={load}>{t('common:actions.refresh')}</Button>
        <Button onClick={handleReloadAll}>{t('system.forceReloadAll')}</Button>
      </Space>

      <Table
        rowKey="config_key"
        loading={loading}
        columns={columns}
        dataSource={data}
        size="small"
        pagination={{ pageSize: 30 }}
      />

      <Modal
        title={editing ? t('system.editModalTitle', { key: editing.config_key }) : t('system.createModalTitle')}
        open={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleOk}
        okText={t('system.okText')}
        width={640}
      >
        <Form form={form} layout="vertical">
          <Form.Item
            name="config_key"
            label={t('system.form.keyLabel')}
            rules={[{ required: true, message: t('system.form.keyRequired') }, { pattern: /^[a-zA-Z0-9._-]+$/, message: t('system.form.keyPattern') }]}
          >
            <Input placeholder={t('system.form.keyPlaceholder')} disabled={!!editing} />
          </Form.Item>
          <Form.Item name="value_type" label={t('system.form.typeLabel')} rules={[{ required: true }]}>
            <Select options={VALUE_TYPES} />
          </Form.Item>
          <Form.Item
            name="value_json"
            label={t('system.form.valueLabel')}
            rules={[{ required: true, message: t('system.form.valueRequired') }]}
            extra={t('system.form.valueExtra')}
          >
            <Input.TextArea rows={4} placeholder={t('system.form.valuePlaceholder')} />
          </Form.Item>
          <Form.Item name="description" label={t('system.form.descLabel')}>
            <Input placeholder={t('system.form.descPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
