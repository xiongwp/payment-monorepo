import { useEffect, useState } from 'react'
import { Table, Tag, Button, Space, Typography, message, Alert, Input, Modal, Form, Select, InputNumber } from 'antd'
import { ReloadOutlined, PlusOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
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
      .catch((e) => message.error(e instanceof Error ? e.message : '加载失败'))
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
    { title: 'Business Type', dataIndex: 'business_type', key: 'business_type', width: 130, sorter: (a, b) => a.business_type - b.business_type, defaultSortOrder: 'ascend' },
    { title: 'Code', dataIndex: 'business_type_code', key: 'business_type_code', width: 240 },
    {
      title: 'Account Type', dataIndex: 'account_type', key: 'account_type', width: 220,
      // 显示"名称 (数字)"，便于运营识别；过滤仍按数字
      render: (v: number) => `${ACCOUNT_TYPE_LABEL[v] ?? '?'} (${v})`,
      filters: Array.from(new Set(rows.map(r => r.account_type))).map(v => ({
        text: `${ACCOUNT_TYPE_LABEL[v] ?? '?'} (${v})`, value: v,
      })),
      onFilter: (v, r) => r.account_type === v,
    },
    {
      // category 不存表,由 account_type 派生(与后端 CategoryForAccountType 同步)
      title: 'Category (derived)', key: 'category', width: 160,
      render: (_, r) => {
        const catNum = CATEGORY_BY_ACCOUNT_TYPE[r.account_type]
        const label = CATEGORY_LABEL[catNum] ?? '?'
        return <Tag color={CATEGORY_COLOR[label] || 'default'}>{label}</Tag>
      },
    },
    { title: '描述', dataIndex: 'description', key: 'description', render: (v: string | null) => v || '-' },
    {
      title: '状态', dataIndex: 'enabled', key: 'enabled', width: 80,
      render: (v: number) => v === 1 ? <Tag color="green">启用</Tag> : <Tag color="red">禁用</Tag>,
      filters: [{ text: '启用', value: 1 }, { text: '禁用', value: 0 }],
      onFilter: (v, r) => r.enabled === v,
    },
  ]

  return (
    <div>
      <Title level={3}>业务类型 (Business Type) 管理</Title>
      <Paragraph type="secondary">
        存于 <Text code>account_meta.account_business_type_info</Text>。
        (user_id, business_type) 是账户唯一键；同一 account_type 下多个渠道用不同 business_type 区分。
        <br />
        <b>新增渠道</b>请走"系统账户"页面的渠道注册流程；本页用于查询 / 审计。
        <br />
        约定：1-9 系统预置，10-100 保留，101-999 自定义渠道。
      </Paragraph>

      <Alert
        style={{ marginBottom: 16 }}
        type="info"
        showIcon
        message="只读视图"
        description="业务类型 (business_type) 一旦创建就不可修改 category / account_type (影响借贷方向)。 如需停用某渠道，请使用 admin API PUT enabled=0。"
      />

      <Space style={{ marginBottom: 12 }}>
        <Button type="primary" icon={<PlusOutlined />} onClick={() => setModalOpen(true)}>新增 Business Type</Button>
        <Input.Search
          allowClear
          placeholder="搜 business_type / code / channel / 描述"
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          style={{ width: 360 }}
        />
        <Button icon={<ReloadOutlined />} onClick={load}>刷新</Button>
        <span style={{ color: '#888' }}>共 {filtered.length} 条</span>
      </Space>

      <Modal
        title="新增 Business Type（仅登记，不建账户）"
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
            message.success(`已登记：business_type=${info.business_type} · ${info.business_type_code}。下一步到"系统账户"页创建 100 分片账户。`)
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
          message="只登记一行 registry"
          description={
            <>
              本操作只写入 <Text code>account_meta.account_business_type_info</Text>，不创建账户。
              登记完成后请到"系统账户"页，选中此 business_type 触发 Fleet 创建 100 个分片账户。
            </>
          }
        />
        <Form form={form} layout="vertical" initialValues={{ business_type: 0 }}>
          <Form.Item label="账户类型 (account_type)" name="account_type" rules={[{ required: true }]}>
            <Select
              placeholder="选择账户类型（1-9）"
              options={ALL_ACCOUNT_TYPES.map(t => ({ value: t.value, label: t.label }))}
            />
          </Form.Item>
          <Form.Item
            label="业务码 (business_type_code)"
            name="business_type_code"
            rules={[{ required: true, message: '大写下划线命名，如 ALIPAY_RECEIVABLE' }]}
          >
            <Input placeholder="ALIPAY_RECEIVABLE" />
          </Form.Item>
          <Form.Item label="用途描述" name="description" rules={[{ required: true }]}>
            <Input.TextArea rows={2} placeholder="支付宝渠道应收款账户" />
          </Form.Item>
          <Form.Item
            label="Business Type 数字码"
            name="business_type"
            tooltip="0 = 自动从 [101, 999] 分配一个未占用号；也可手动指定"
          >
            <InputNumber min={0} max={999} style={{ width: '100%' }} placeholder="0 自动分配" />
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
