import { useCallback, useEffect, useState } from 'react'
import {
  Button,
  Form,
  Input,
  message,
  Modal,
  Popconfirm,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
} from 'antd'
import {
  PlusOutlined,
  ReloadOutlined,
  SyncOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import { useTranslation } from 'react-i18next'
import {
  createBufferAccount,
  deleteBufferAccount,
  listBufferAccounts,
  reloadBufferAccounts,
  updateBufferAccount,
} from '../../api/accounting'
import type { BufferAccountConfig, BufferFlushLevel } from '../../types/accounting'
import { BUFFER_FLUSH_LEVEL_LABELS, BUFFER_FLUSH_LEVELS } from '../../types/accounting'
import InstanceListPanel from '../../components/InstanceListPanel'

type FormMode = 'create' | 'edit'

interface FormValues {
  account_no: string
  flush_interval_level: BufferFlushLevel
  description?: string
  enabled?: boolean
}

export default function BufferAccountConfigPage() {
  const { t } = useTranslation('config')
  const [data, setData] = useState<BufferAccountConfig[]>([])
  const [loading, setLoading] = useState(false)
  const [reloading, setReloading] = useState(false)
  const [modalOpen, setModalOpen] = useState(false)
  const [modalMode, setModalMode] = useState<FormMode>('create')
  const [editingId, setEditingId] = useState<number | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [form] = Form.useForm<FormValues>()

  const fetchList = useCallback(async () => {
    setLoading(true)
    try {
      const items = await listBufferAccounts()
      setData(items ?? [])
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('buffer.messages.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { fetchList() }, [fetchList])

  const openCreate = () => {
    setModalMode('create')
    setEditingId(null)
    form.resetFields()
    form.setFieldsValue({ flush_interval_level: 5 })
    setModalOpen(true)
  }

  const openEdit = (record: BufferAccountConfig) => {
    setModalMode('edit')
    setEditingId(record.id)
    form.setFieldsValue({
      account_no: record.account_no,
      flush_interval_level: record.flush_interval_level,
      description: record.description,
      enabled: record.enabled,
    })
    setModalOpen(true)
  }

  const handleSubmit = async () => {
    const values = await form.validateFields()
    setSubmitting(true)
    try {
      if (modalMode === 'create') {
        await createBufferAccount({
          account_no: values.account_no,
          flush_interval_level: values.flush_interval_level,
          description: values.description,
        })
        message.success(t('buffer.messages.createSuccess'))
      } else if (editingId !== null) {
        await updateBufferAccount(editingId, {
          enabled: values.enabled ?? true,
          flush_interval_level: values.flush_interval_level,
          description: values.description,
        })
        message.success(t('buffer.messages.updateSuccess'))
      }
      setModalOpen(false)
      fetchList()
      // 自动热重载：让 accounting-system 内存缓存立即生效，无需手动点击"重新加载"
      await reloadBufferAccounts()
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('buffer.messages.operationFailed'))
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = async (id: number) => {
    try {
      await deleteBufferAccount(id)
      message.success(t('buffer.messages.deleteSuccess'))
      fetchList()
      await reloadBufferAccounts()
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('buffer.messages.deleteFailed'))
    }
  }

  const handleReload = async () => {
    setReloading(true)
    try {
      const res = await reloadBufferAccounts()
      message.success(t('buffer.messages.reloadSuccess', { count: res.count }))
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('buffer.messages.reloadFailed'))
    } finally {
      setReloading(false)
    }
  }

  const columns: ColumnsType<BufferAccountConfig> = [
    { title: t('buffer.columns.id'), dataIndex: 'id', width: 70 },
    { title: t('buffer.columns.accountNo'), dataIndex: 'account_no', width: 220 },
    {
      title: t('buffer.columns.flushInterval'),
      dataIndex: 'flush_interval_level',
      width: 110,
      render: (level: BufferFlushLevel) => (
        <Tag color="blue">{BUFFER_FLUSH_LEVEL_LABELS[level] ?? level}</Tag>
      ),
    },
    {
      title: t('buffer.columns.status'),
      dataIndex: 'enabled',
      width: 80,
      render: (enabled: boolean) =>
        enabled ? <Tag color="green">{t('buffer.status.enabled')}</Tag> : <Tag color="default">{t('buffer.status.disabled')}</Tag>,
    },
    { title: t('buffer.columns.description'), dataIndex: 'description', ellipsis: true },
    {
      title: t('buffer.columns.updatedAt'),
      dataIndex: 'updated_at',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: t('buffer.columns.actions'),
      width: 140,
      render: (_: unknown, record: BufferAccountConfig) => (
        <Space>
          <Button type="link" size="small" onClick={() => openEdit(record)}>
            {t('common:actions.edit')}
          </Button>
          <Popconfirm
            title={t('buffer.deleteConfirm')}
            onConfirm={() => handleDelete(record.id)}
            okText={t('common:actions.confirm')}
            cancelText={t('common:actions.cancel')}
          >
            <Button type="link" size="small" danger>
              {t('common:actions.delete')}
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 16 }}>
        <Space>
          <Button icon={<PlusOutlined />} type="primary" onClick={openCreate}>
            {t('buffer.addAccount')}
          </Button>
          <Tooltip title={t('buffer.reloadTooltip')}>
            <Button
              icon={<SyncOutlined spin={reloading} />}
              loading={reloading}
              onClick={handleReload}
            >
              {t('buffer.reloadButton')}
            </Button>
          </Tooltip>
        </Space>
        <Button icon={<ReloadOutlined />} onClick={fetchList} loading={loading}>
          {t('common:actions.refresh')}
        </Button>
      </div>

      <Table
        rowKey="id"
        columns={columns}
        dataSource={data}
        loading={loading}
        pagination={{ pageSize: 20, showSizeChanger: false }}
        size="middle"
      />

      <InstanceListPanel type="buffer" />

      <Modal
        title={modalMode === 'create' ? t('buffer.createModalTitle') : t('buffer.editModalTitle')}
        open={modalOpen}
        onOk={handleSubmit}
        onCancel={() => setModalOpen(false)}
        confirmLoading={submitting}
        okText={t('common:actions.confirm')}
        cancelText={t('common:actions.cancel')}
        destroyOnClose
      >
        <Form form={form} layout="vertical" style={{ marginTop: 16 }}>
          <Form.Item
            name="account_no"
            label={t('buffer.form.accountNoLabel')}
            rules={[{ required: true, message: t('buffer.form.accountNoRequired') }]}
          >
            <Input placeholder={t('buffer.form.accountNoPlaceholder')} disabled={modalMode === 'edit'} />
          </Form.Item>

          <Form.Item
            name="flush_interval_level"
            label={t('buffer.form.flushLabel')}
            rules={[{ required: true, message: t('buffer.form.flushRequired') }]}
          >
            <Select placeholder={t('buffer.form.flushPlaceholder')}>
              {BUFFER_FLUSH_LEVELS.map((level) => (
                <Select.Option key={level} value={level}>
                  {BUFFER_FLUSH_LEVEL_LABELS[level]}
                </Select.Option>
              ))}
            </Select>
          </Form.Item>

          {modalMode === 'edit' && (
            <Form.Item name="enabled" label={t('buffer.form.enabledLabel')} valuePropName="checked">
              <Switch checkedChildren={t('buffer.form.enabledOn')} unCheckedChildren={t('buffer.form.enabledOff')} />
            </Form.Item>
          )}

          <Form.Item name="description" label={t('buffer.form.descLabel')}>
            <Input.TextArea rows={3} placeholder={t('buffer.form.descPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
