import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert, Button, Card, Form, Input, Modal, Popconfirm, Select, Space, Table, Tag, Typography, message,
} from 'antd'
import { Link, useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
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
  const { t } = useTranslation('merchant')
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
      message.success(t('secrets.putModal.savedMessage', { channel: v.channel, field: v.field_name }))
      setPutOpen(false)
      putForm.resetFields()
      load()
    } catch (e) { message.error(String(e)) }
  }

  const onDelete = async (s: MerchantChannelSecret) => {
    try {
      await deleteMerchantSecret(id, s.channel, s.field_name)
      message.success(t('secrets.delete.doneMessage'))
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
        {merchant ? <Link to={`/merchants/${id}`}>{merchant.name}</Link> : id} · {t('secrets.titleSuffix')}
      </Typography.Title>
      <Alert
        type="warning" showIcon style={{ marginBottom: 16 }}
        message={t('secrets.warningAlert')}
      />
      <Card>
        <Space wrap style={{ marginBottom: 16 }}>
          <Select
            placeholder={t('secrets.filters.channelPlaceholder')} allowClear style={{ width: 200 }}
            options={Object.keys(CHANNEL_FIELD_TEMPLATES).map((c) => ({ value: c, label: c }))}
            onChange={setFilter}
          />
          <Button onClick={load}>{t('common:actions.refresh')}</Button>
          <Button type="primary" onClick={() => setPutOpen(true)}>{t('secrets.actions.create')}</Button>
        </Space>
        <Table<MerchantChannelSecret>
          rowKey={(r) => `${r.channel}|${r.field_name}`}
          size="small"
          loading={loading}
          dataSource={secrets}
          pagination={false}
          columns={[
            { title: t('secrets.columns.channel'), dataIndex: 'channel', width: 140, render: (v) => <Tag>{v}</Tag> },
            { title: t('secrets.columns.field'), dataIndex: 'field_name', width: 220, render: (v) => <Typography.Text code>{v}</Typography.Text> },
            { title: t('secrets.columns.masked'), dataIndex: 'masked_hint', ellipsis: true, render: (v) => <Typography.Text type="secondary" code>{v || '-'}</Typography.Text> },
            { title: t('secrets.columns.version'), dataIndex: 'version', width: 60 },
            { title: t('secrets.columns.updatedBy'), dataIndex: 'created_by', width: 120, ellipsis: true },
            { title: t('secrets.columns.updatedAt'), dataIndex: 'updated_ms', width: 160, render: (v) => dayjs(v).format('MM-DD HH:mm:ss') },
            {
              title: '', width: 90,
              render: (_, r) => (
                <Popconfirm
                  title={t('secrets.delete.title')}
                  description={t('secrets.delete.description')}
                  okText={t('secrets.delete.okText')} okButtonProps={{ danger: true }}
                  onConfirm={() => onDelete(r)}
                >
                  <a style={{ color: 'red' }}>{t('secrets.delete.link')}</a>
                </Popconfirm>
              ),
            },
          ]}
        />
      </Card>

      <Modal
        title={t('secrets.putModal.title')}
        open={putOpen}
        onCancel={() => setPutOpen(false)}
        onOk={() => putForm.submit()}
        width={640}
      >
        <Alert
          type="info" showIcon style={{ marginBottom: 12 }}
          message={t('secrets.putModal.alert')}
        />
        <Form form={putForm} layout="vertical" onFinish={onPut}>
          <Form.Item name="channel" label={t('secrets.putModal.fields.channel')} rules={[{ required: true }]}>
            <Select
              options={Object.keys(CHANNEL_FIELD_TEMPLATES).map((c) => ({ value: c, label: c }))}
              onChange={(v) => setSelectedChannel(v)}
              showSearch
            />
          </Form.Item>
          <Form.Item name="field_name" label={t('secrets.putModal.fields.fieldName')} rules={[{ required: true }]}>
            {selectedChannel && missingFields.length > 0 ? (
              <Select
                placeholder={t('secrets.putModal.fields.fieldNamePlaceholder')}
                options={[
                  ...missingFields.map((f) => ({ value: f, label: `${f}${t('secrets.putModal.fields.missingSuffix')}` })),
                  ...CHANNEL_FIELD_TEMPLATES[selectedChannel]
                    .filter((f) => !missingFields.includes(f))
                    .map((f) => ({ value: f, label: `${f}${t('secrets.putModal.fields.overrideSuffix')}` })),
                ]}
                allowClear
              />
            ) : (
              <Input placeholder={t('secrets.putModal.fields.fieldNameInputPlaceholder')} />
            )}
          </Form.Item>
          <Form.Item name="plaintext" label={t('secrets.putModal.fields.plaintext')} rules={[{ required: true }]}>
            <Input.TextArea rows={3} placeholder={t('secrets.putModal.fields.plaintextPlaceholder')} />
          </Form.Item>
          <Form.Item name="actor" label={t('secrets.putModal.fields.actor')} initialValue="admin">
            <Input />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
