import { useCallback, useEffect, useState } from 'react'
import {
  Button,
  Form,
  Input,
  message,
  Modal,
  Popconfirm,
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
  createHotAccount,
  deleteHotAccount,
  listHotAccounts,
  reloadHotAccounts,
  updateHotAccount,
} from '../../api/accounting'
import type { HotAccountConfig } from '../../types/accounting'
import InstanceListPanel from '../../components/InstanceListPanel'

type FormMode = 'create' | 'edit'

interface FormValues {
  account_no: string
  description?: string
  enabled?: boolean
}

export default function HotAccountConfigPage() {
  const { t } = useTranslation('config')
  const [data, setData] = useState<HotAccountConfig[]>([])
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
      const items = await listHotAccounts()
      setData(items ?? [])
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('hot.messages.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { fetchList() }, [fetchList])

  const openCreate = () => {
    setModalMode('create')
    setEditingId(null)
    form.resetFields()
    setModalOpen(true)
  }

  const openEdit = (record: HotAccountConfig) => {
    setModalMode('edit')
    setEditingId(record.id)
    form.setFieldsValue({
      account_no: record.account_no,
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
        await createHotAccount({ account_no: values.account_no, description: values.description })
        message.success(t('hot.messages.createSuccess'))
      } else if (editingId !== null) {
        await updateHotAccount(editingId, {
          enabled: values.enabled ?? true,
          description: values.description,
        })
        message.success(t('hot.messages.updateSuccess'))
      }
      setModalOpen(false)
      fetchList()
      await reloadHotAccounts()
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('hot.messages.operationFailed'))
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = async (id: number) => {
    try {
      await deleteHotAccount(id)
      message.success(t('hot.messages.deleteSuccess'))
      fetchList()
      await reloadHotAccounts()
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('hot.messages.deleteFailed'))
    }
  }

  const handleReload = async () => {
    setReloading(true)
    try {
      const res = await reloadHotAccounts()
      message.success(t('hot.messages.reloadSuccess', { count: res.count }))
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? t('hot.messages.reloadFailed'))
    } finally {
      setReloading(false)
    }
  }

  const columns: ColumnsType<HotAccountConfig> = [
    { title: t('hot.columns.id'), dataIndex: 'id', width: 70 },
    { title: t('hot.columns.accountNo'), dataIndex: 'account_no', width: 220 },
    {
      title: t('hot.columns.status'),
      dataIndex: 'enabled',
      width: 80,
      render: (enabled: boolean) =>
        enabled ? <Tag color="green">{t('hot.status.enabled')}</Tag> : <Tag color="default">{t('hot.status.disabled')}</Tag>,
    },
    { title: t('hot.columns.description'), dataIndex: 'description', ellipsis: true },
    {
      title: t('hot.columns.updatedAt'),
      dataIndex: 'updated_at',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: t('hot.columns.actions'),
      width: 140,
      render: (_: unknown, record: HotAccountConfig) => (
        <Space>
          <Button type="link" size="small" onClick={() => openEdit(record)}>
            {t('common:actions.edit')}
          </Button>
          <Popconfirm
            title={t('hot.deleteConfirm')}
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
            {t('hot.addAccount')}
          </Button>
          <Tooltip title={t('hot.reloadTooltip')}>
            <Button
              icon={<SyncOutlined spin={reloading} />}
              loading={reloading}
              onClick={handleReload}
            >
              {t('hot.reloadButton')}
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

      <InstanceListPanel type="hot" />

      <Modal
        title={modalMode === 'create' ? t('hot.createModalTitle') : t('hot.editModalTitle')}
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
            label={t('hot.form.accountNoLabel')}
            rules={[{ required: true, message: t('hot.form.accountNoRequired') }]}
          >
            <Input placeholder={t('hot.form.accountNoPlaceholder')} disabled={modalMode === 'edit'} />
          </Form.Item>

          {modalMode === 'edit' && (
            <Form.Item name="enabled" label={t('hot.form.enabledLabel')} valuePropName="checked">
              <Switch checkedChildren={t('hot.form.enabledOn')} unCheckedChildren={t('hot.form.enabledOff')} />
            </Form.Item>
          )}

          <Form.Item name="description" label={t('hot.form.descLabel')}>
            <Input.TextArea rows={3} placeholder={t('hot.form.descPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
