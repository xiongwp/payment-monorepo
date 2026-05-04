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
      message.error((err as { message?: string })?.message ?? '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

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
        message.success('创建成功')
      } else if (editingId !== null) {
        await updateHotAccount(editingId, {
          enabled: values.enabled ?? true,
          description: values.description,
        })
        message.success('更新成功')
      }
      setModalOpen(false)
      fetchList()
      await reloadHotAccounts()
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? '操作失败')
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = async (id: number) => {
    try {
      await deleteHotAccount(id)
      message.success('删除成功')
      fetchList()
      await reloadHotAccounts()
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? '删除失败')
    }
  }

  const handleReload = async () => {
    setReloading(true)
    try {
      const res = await reloadHotAccounts()
      message.success(`热重载完成，共 ${res.count} 个账户`)
    } catch (err: unknown) {
      message.error((err as { message?: string })?.message ?? '热重载失败')
    } finally {
      setReloading(false)
    }
  }

  const columns: ColumnsType<HotAccountConfig> = [
    { title: 'ID', dataIndex: 'id', width: 70 },
    { title: '账户号', dataIndex: 'account_no', width: 220 },
    {
      title: '状态',
      dataIndex: 'enabled',
      width: 80,
      render: (enabled: boolean) =>
        enabled ? <Tag color="green">启用</Tag> : <Tag color="default">禁用</Tag>,
    },
    { title: '描述', dataIndex: 'description', ellipsis: true },
    {
      title: '更新时间',
      dataIndex: 'updated_at',
      width: 180,
      render: (v: string) => v ? new Date(v).toLocaleString('zh-CN') : '-',
    },
    {
      title: '操作',
      width: 140,
      render: (_: unknown, record: HotAccountConfig) => (
        <Space>
          <Button type="link" size="small" onClick={() => openEdit(record)}>
            编辑
          </Button>
          <Popconfirm
            title="确认删除该热点账户配置？"
            onConfirm={() => handleDelete(record.id)}
            okText="确认"
            cancelText="取消"
          >
            <Button type="link" size="small" danger>
              删除
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
            新增热点账户
          </Button>
          <Tooltip title="将数据库中的热点账户白名单热重载到运行中的服务">
            <Button
              icon={<SyncOutlined spin={reloading} />}
              loading={reloading}
              onClick={handleReload}
            >
              热重载白名单
            </Button>
          </Tooltip>
        </Space>
        <Button icon={<ReloadOutlined />} onClick={fetchList} loading={loading}>
          刷新
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
        title={modalMode === 'create' ? '新增热点账户' : '编辑热点账户'}
        open={modalOpen}
        onOk={handleSubmit}
        onCancel={() => setModalOpen(false)}
        confirmLoading={submitting}
        okText="确认"
        cancelText="取消"
        destroyOnClose
      >
        <Form form={form} layout="vertical" style={{ marginTop: 16 }}>
          <Form.Item
            name="account_no"
            label="账户号"
            rules={[{ required: true, message: '请输入账户号' }]}
          >
            <Input placeholder="请输入账户号" disabled={modalMode === 'edit'} />
          </Form.Item>

          {modalMode === 'edit' && (
            <Form.Item name="enabled" label="启用状态" valuePropName="checked">
              <Switch checkedChildren="启用" unCheckedChildren="禁用" />
            </Form.Item>
          )}

          <Form.Item name="description" label="描述">
            <Input.TextArea rows={3} placeholder="可选描述" />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
