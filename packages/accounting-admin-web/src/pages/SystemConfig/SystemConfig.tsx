import { useEffect, useState } from 'react'
import {
  Table, Button, Modal, Form, Input, Select, message, Space, Typography, Tag, Popconfirm, Alert,
} from 'antd'
import { PlusOutlined, ReloadOutlined, EditOutlined, DeleteOutlined } from '@ant-design/icons'
import {
  listSystemConfig,
  upsertSystemConfig,
  deleteSystemConfig,
  reloadSystemConfig,
  type SystemConfigItem,
} from '../../api/accounting'

const { Title, Paragraph, Text } = Typography

const VALUE_TYPES = [
  { value: 'int', label: 'int (整数)' },
  { value: 'string', label: 'string (字符串)' },
  { value: 'bool', label: 'bool (布尔)' },
  { value: 'json', label: 'json (任意结构：数组 / 对象 / 浮点等)' },
]

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
  const [data, setData] = useState<SystemConfigItem[]>([])
  const [loading, setLoading] = useState(false)
  const [modalOpen, setModalOpen] = useState(false)
  const [editing, setEditing] = useState<SystemConfigItem | null>(null)
  const [form] = Form.useForm()

  const load = () => {
    setLoading(true)
    listSystemConfig()
      .then((rows) => setData(Array.isArray(rows) ? rows : []))
      .catch((e) => message.error(e instanceof Error ? e.message : '加载失败'))
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
        message.error('value_json 不是合法 JSON: ' + err)
        return
      }
      await upsertSystemConfig(v.config_key, v.value_json, v.value_type, v.description || '', 'admin-web')
      message.success(editing ? '配置已更新（已推送至所有实例）' : '配置已新增（已推送至所有实例）')
      setModalOpen(false)
      load()
    } catch (e) {
      if (e instanceof Error) message.error(e.message)
    }
  }

  const handleDelete = async (key: string) => {
    try {
      await deleteSystemConfig(key)
      message.success('已删除（已推送至所有实例）')
      load()
    } catch (e) {
      message.error(e instanceof Error ? e.message : '删除失败')
    }
  }

  const handleReloadAll = async () => {
    try {
      await reloadSystemConfig()
      message.success('已强制所有实例 reload 配置')
      load()
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'reload 失败')
    }
  }

  const columns = [
    {
      title: 'Key',
      dataIndex: 'config_key',
      key: 'config_key',
      width: 280,
      render: (v: string) => <Text code>{v}</Text>,
    },
    {
      title: '类型',
      dataIndex: 'value_type',
      key: 'value_type',
      width: 80,
      render: (v: string) => <Tag color={v === 'json' ? 'purple' : v === 'bool' ? 'gold' : 'blue'}>{v || 'string'}</Tag>,
    },
    {
      title: 'Value',
      dataIndex: 'value_json',
      key: 'value_json',
      render: (v: string) => <Text code style={{ wordBreak: 'break-all' }}>{v}</Text>,
    },
    {
      title: '说明',
      dataIndex: 'description',
      key: 'description',
    },
    {
      title: '更新',
      dataIndex: 'updated_at',
      key: 'updated_at',
      width: 170,
      render: (v: string) => v ? new Date(v).toLocaleString() : '-',
    },
    {
      title: '操作',
      key: 'op',
      width: 160,
      render: (_: unknown, item: SystemConfigItem) => (
        <Space>
          <Button icon={<EditOutlined />} size="small" onClick={() => openEdit(item)}>编辑</Button>
          <Popconfirm title={`删除 ${item.config_key}?`} onConfirm={() => handleDelete(item.config_key)}>
            <Button icon={<DeleteOutlined />} size="small" danger>删除</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <Title level={3}>系统配置</Title>
      <Paragraph type="secondary">
        通用 key-value 配置中心，存于 <Text code>account_meta.system_config</Text> 表。修改任意一条会
        立即 fanout 到所有 alive instances；60s 兜底 tick 也会自动同步。Value 必须是合法 JSON
        （字符串如 <Text code>"abc"</Text>，数字如 <Text code>123</Text>，对象如 <Text code>{"{\"a\":1}"}</Text>）。
      </Paragraph>
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="新增 key 后，使用方需要在代码中通过 systemConfigSvc.GetInt/GetString/GetJSON 等读取；硬编码 fallback 默认值仍生效，配置仅作为可在线调整的覆盖。"
      />

      <Space style={{ marginBottom: 12 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>新增配置</Button>
        <Button icon={<ReloadOutlined />} onClick={load}>刷新</Button>
        <Button onClick={handleReloadAll}>强制所有实例 reload</Button>
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
        title={editing ? `编辑配置: ${editing.config_key}` : '新增配置'}
        open={modalOpen}
        onCancel={() => setModalOpen(false)}
        onOk={handleOk}
        okText="保存并推送"
        width={640}
      >
        <Form form={form} layout="vertical">
          <Form.Item
            name="config_key"
            label="Key"
            rules={[{ required: true, message: '必填' }, { pattern: /^[a-zA-Z0-9._-]+$/, message: '只允许字母数字 . _ -' }]}
          >
            <Input placeholder="如 tcc_recovery.stuck_timeout_minutes" disabled={!!editing} />
          </Form.Item>
          <Form.Item name="value_type" label="类型 hint" rules={[{ required: true }]}>
            <Select options={VALUE_TYPES} />
          </Form.Item>
          <Form.Item
            name="value_json"
            label="Value (JSON 格式)"
            rules={[{ required: true, message: '必填' }]}
            extra="字符串要带双引号（如 &quot;abc&quot;），数字 / 数组 / 对象直接写。"
          >
            <Input.TextArea rows={4} placeholder='例如：5  或  "abc"  或  [1,2,3]  或  {"a":1}' />
          </Form.Item>
          <Form.Item name="description" label="说明">
            <Input placeholder="简短描述这个 key 的用途" />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
