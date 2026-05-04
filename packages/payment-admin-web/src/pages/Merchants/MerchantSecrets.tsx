import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, Button, Card, Form, Input, Modal, Popconfirm, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import { Link, useParams } from 'react-router-dom'
import dayjs from 'dayjs'
import {
  listMerchantSecrets, putMerchantSecret, deleteMerchantSecret, getMerchant,
} from '../../api'
import type { Merchant, MerchantChannelSecret } from '../../api'

// Channel → field templates (extracted from payment-channel adapter Configs).
// Updating these keys as new channels land is a tiny maintenance cost vs.
// letting admins type arbitrary keys and break spellings across merchants.
const CHANNEL_FIELD_TEMPLATES: Record<string, string[]> = {
  gcash:     ['client_id', 'client_secret', 'signing_key', 'webhook_secret', 'merchant_id', 'store_id'],
  maya:      ['public_key', 'secret_key'],
  grabpay:   ['partner_id', 'partner_secret', 'merchant_id'],
  paymongo:  ['secret_key', 'webhook_secret'],
  xendit:    ['secret_key', 'verification_token'],
  dragonpay: ['merchant_id', 'merchant_key'],
  shopeepay: ['partner_id', 'partner_key', 'merchant_ext_id', 'store_ext_id'],
  billease:  ['client_id', 'client_secret'],
  coinsph:   ['merchant_id', 'secret_key'],
  instapay:  ['client_id', 'client_secret', 'partner_id'],
  pesonet:   ['client_id', 'client_secret', 'partner_id'],
  bdo:       ['client_id', 'client_secret', 'partner_id', 'partner_secret'],
  bpi:       ['client_id', 'client_secret', 'partner_id'],
  metrobank: ['client_id', 'client_secret', 'partner_id', 'partner_secret'],
  landbank:  ['merchant_id', 'secret_key'],
}

export default function MerchantSecrets() {
  const { id = '' } = useParams()
  const [merchant, setMerchant] = useState<Merchant | null>(null)
  const [secrets, setSecrets] = useState<MerchantChannelSecret[]>([])
  const [loading, setLoading] = useState(false)
  const [filter, setFilter] = useState<string | undefined>(undefined)
  const [putOpen, setPutOpen] = useState(false)
  const [putForm] = Form.useForm()
  const [selectedChannel, setSelectedChannel] = useState<string | undefined>(undefined)

  const load = useCallback(async () => {
    if (!id) return
    setLoading(true)
    try {
      const [m, r] = await Promise.all([
        getMerchant(id),
        listMerchantSecrets(id, filter),
      ])
      setMerchant(m)
      setSecrets(r.secrets || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [id, filter])

  useEffect(() => { load() }, [load])

  const onPut = async (v: { channel: string; field_name: string; plaintext: string; actor?: string }) => {
    try {
      await putMerchantSecret(id, v)
      message.success(`已保存 ${v.channel}/${v.field_name} (加密)`)
      setPutOpen(false)
      putForm.resetFields()
      load()
    } catch (e) { message.error(String(e)) }
  }

  const onDelete = async (s: MerchantChannelSecret) => {
    try {
      await deleteMerchantSecret(id, s.channel, s.field_name)
      message.success('已删除')
      load()
    } catch (e) { message.error(String(e)) }
  }

  const grouped = useMemo(() => {
    const g: Record<string, MerchantChannelSecret[]> = {}
    for (const s of secrets) {
      (g[s.channel] = g[s.channel] || []).push(s)
    }
    return g
  }, [secrets])

  const missingFields = useMemo(() => {
    if (!selectedChannel) return []
    const tpl = CHANNEL_FIELD_TEMPLATES[selectedChannel] || []
    const present = new Set((grouped[selectedChannel] || []).map((s) => s.field_name))
    return tpl.filter((f) => !present.has(f))
  }, [grouped, selectedChannel])

  return (
    <div>
      <Typography.Title level={3}>
        {merchant ? <Link to={`/merchants/${id}`}>{merchant.name}</Link> : id} · 渠道凭据
      </Typography.Title>
      <Alert
        type="warning" showIcon style={{ marginBottom: 16 }}
        message="所有明文凭据经 kms-manage 加密后存储；admin 界面永远只显示 masked hint（如 sk_l***abc）。请在 yaml 中配置 kms.endpoint 才能真正调用 KMS。"
      />
      <Card>
        <Space wrap style={{ marginBottom: 16 }}>
          <Select
            placeholder="按渠道过滤" allowClear style={{ width: 200 }}
            options={Object.keys(CHANNEL_FIELD_TEMPLATES).map((c) => ({ value: c, label: c }))}
            onChange={setFilter}
          />
          <Button onClick={load}>刷新</Button>
          <Button type="primary" onClick={() => setPutOpen(true)}>新建/覆盖凭据</Button>
        </Space>
        <Table<MerchantChannelSecret>
          rowKey={(r) => `${r.channel}|${r.field_name}`}
          size="small"
          loading={loading}
          dataSource={secrets}
          pagination={false}
          columns={[
            { title: '渠道', dataIndex: 'channel', width: 140, render: (v) => <Tag>{v}</Tag> },
            { title: '字段', dataIndex: 'field_name', width: 220, render: (v) => <Typography.Text code>{v}</Typography.Text> },
            { title: 'Masked', dataIndex: 'masked_hint', ellipsis: true, render: (v) => <Typography.Text type="secondary" code>{v || '-'}</Typography.Text> },
            { title: 'ver', dataIndex: 'version', width: 60 },
            { title: '更新人', dataIndex: 'created_by', width: 120, ellipsis: true },
            { title: '更新时间', dataIndex: 'updated_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
            {
              title: '', width: 90,
              render: (_, r) => (
                <Popconfirm
                  title="删除此凭据？"
                  description="删除后该字段将从数据库中消失；有需要可重新 Put 同名字段。"
                  okText="确认删除" okButtonProps={{ danger: true }}
                  onConfirm={() => onDelete(r)}
                >
                  <a style={{ color: 'red' }}>删除</a>
                </Popconfirm>
              ),
            },
          ]}
        />
      </Card>

      <Modal
        title="存入商户渠道凭据"
        open={putOpen}
        onCancel={() => setPutOpen(false)}
        onOk={() => putForm.submit()}
        width={640}
      >
        <Alert
          type="info" showIcon style={{ marginBottom: 12 }}
          message="明文仅提交一次；kms-manage 加密后永不再能从 UI 取回。遗失需重新 Put。"
        />
        <Form form={putForm} layout="vertical" onFinish={onPut}>
          <Form.Item name="channel" label="渠道" rules={[{ required: true }]}>
            <Select
              options={Object.keys(CHANNEL_FIELD_TEMPLATES).map((c) => ({ value: c, label: c }))}
              onChange={(v) => setSelectedChannel(v)}
              showSearch
            />
          </Form.Item>
          <Form.Item name="field_name" label="字段名" rules={[{ required: true }]}>
            {selectedChannel && missingFields.length > 0 ? (
              <Select
                placeholder="必填 · 未填写的字段"
                options={[
                  ...missingFields.map((f) => ({ value: f, label: `${f} (未填)` })),
                  ...CHANNEL_FIELD_TEMPLATES[selectedChannel]
                    .filter((f) => !missingFields.includes(f))
                    .map((f) => ({ value: f, label: `${f} (覆盖)` })),
                ]}
                allowClear
              />
            ) : (
              <Input placeholder="如 client_secret / webhook_secret" />
            )}
          </Form.Item>
          <Form.Item name="plaintext" label="明文" rules={[{ required: true }]}>
            <Input.TextArea rows={3} placeholder="明文 API key / secret / signing material" />
          </Form.Item>
          <Form.Item name="actor" label="操作人" initialValue="admin">
            <Input />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
