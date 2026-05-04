import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Form, Input, Modal, Select, Space, Switch, Table, Tag,
  Typography, message,
} from 'antd'
import dayjs from 'dayjs'
import { recentOutcomes, recordOutcome } from '../../api/risk'
import type { Outcome, OutcomeSource } from '../../api/risk'

const SOURCE_LABEL: Record<OutcomeSource, string> = {
  review_human: '人工 review',
  dispute: 'dispute / chargeback',
  merchant_confirm: '商户确认',
}

const SOURCE_COLOR: Record<OutcomeSource, string> = {
  review_human: 'blue',
  dispute: 'red',
  merchant_confirm: 'green',
}

export default function Outcomes() {
  const [loading, setLoading] = useState(false)
  const [rows, setRows] = useState<Outcome[]>([])
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm<{
    decision_id: string; source: OutcomeSource; is_fraud: boolean;
    actor: string; notes: string;
  }>()

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const r = await recentOutcomes(200)
      setRows(r.items || [])
    } catch (e) { message.error(String(e)) }
    finally { setLoading(false) }
  }, [])

  useEffect(() => { load() }, [load])

  const onSubmit = async (v: Parameters<typeof recordOutcome>[0]) => {
    try {
      await recordOutcome(v)
      message.success('已记录')
      setOpen(false)
      form.resetFields()
      load()
    } catch (e) { message.error(String(e)) }
  }

  return (
    <div>
      <Typography.Title level={3}>风控反馈记录（Outcome）</Typography.Title>
      <Card>
        <Alert
          type="info" showIcon style={{ marginBottom: 16 }}
          message="每条 Outcome 关联到一个 decision_id，记录该笔最终是不是欺诈的真实结果。来源：人工 review / dispute / 商户主动确认。ML pipeline 离线导出做训练 label。"
        />
        <Space style={{ marginBottom: 16 }}>
          <Button type="primary" onClick={() => setOpen(true)}>记录 Outcome</Button>
          <Button onClick={load}>刷新</Button>
          <Typography.Text type="secondary">最近 {rows.length} 条</Typography.Text>
        </Space>
        <Table<Outcome>
          rowKey={(r) => `${r.decision_id}-${r.at}`} size="small" loading={loading} dataSource={rows}
          pagination={{ pageSize: 25 }}
          columns={[
            {
              title: '决策 ID', dataIndex: 'decision_id', width: 270, ellipsis: true,
              render: (v) => <Typography.Text code copyable>{v}</Typography.Text>,
            },
            {
              title: '时间', dataIndex: 'at', width: 170,
              render: (v: string) => dayjs(v).format('MM-DD HH:mm:ss'),
            },
            {
              title: '来源', dataIndex: 'source', width: 140,
              render: (v: OutcomeSource) => <Tag color={SOURCE_COLOR[v]}>{SOURCE_LABEL[v] || v}</Tag>,
            },
            {
              title: '判定', dataIndex: 'is_fraud', width: 90, align: 'center',
              render: (v: boolean) => v
                ? <Tag color="red">FRAUD</Tag>
                : <Tag color="green">LEGIT</Tag>,
            },
            { title: '操作人', dataIndex: 'actor', width: 140, ellipsis: true },
            { title: '备注', dataIndex: 'notes', ellipsis: true },
          ]}
        />
      </Card>

      <Modal
        title="记录 Outcome 反馈" open={open}
        onCancel={() => setOpen(false)} onOk={() => form.submit()}
      >
        <Alert
          type="warning" showIcon style={{ marginBottom: 12 }}
          message="一般 dispute 系统在终态时自动写入；此处是给运营手动补录用的（如商户邮件确认是 / 不是欺诈）。"
        />
        <Form form={form} layout="vertical" onFinish={onSubmit} initialValues={{
          source: 'merchant_confirm', is_fraud: false,
        }}>
          <Form.Item name="decision_id" label="决策 ID" rules={[{ required: true }]}>
            <Input placeholder="32 hex（来自 audit / review）" />
          </Form.Item>
          <Form.Item name="source" label="来源" rules={[{ required: true }]}>
            <Select options={Object.entries(SOURCE_LABEL).map(([k, v]) => ({
              value: k, label: v,
            }))} />
          </Form.Item>
          <Form.Item name="is_fraud" label="是否欺诈" valuePropName="checked">
            <Switch checkedChildren="是" unCheckedChildren="否" />
          </Form.Item>
          <Form.Item name="actor" label="操作人">
            <Input placeholder="如 ops:zhang.san" />
          </Form.Item>
          <Form.Item name="notes" label="备注">
            <Input.TextArea rows={3} placeholder="证据 / 关联工单号 / 备注" />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
