import { useEffect, useState } from 'react'
import {
  Button,
  Card,
  Col,
  Divider,
  Form,
  Input,
  Row,
  Space,
  Table,
  Tag,
  Typography,
  message,
} from 'antd'
import { useTranslation } from 'react-i18next'
import dayjs from 'dayjs'
import { kmsDecrypt, kmsEncrypt, listKeys } from '../../api'
import type { KMSKey } from '../../api/types'

export default function KMSPanel() {
  const { t } = useTranslation('kms')
  const [keys, setKeys] = useState<KMSKey[]>([])
  const [activeID, setActiveID] = useState<string>('')
  const [loading, setLoading] = useState(false)

  const [encForm] = Form.useForm()
  const [decForm] = Form.useForm()
  const [encResult, setEncResult] = useState<string>('')
  const [decResult, setDecResult] = useState<string>('')

  const reload = () => {
    setLoading(true)
    listKeys()
      .then((r) => {
        setKeys(r.items ?? [])
        setActiveID(r.active_key_id)
      })
      .catch((e) => message.error(String(e)))
      .finally(() => setLoading(false))
  }
  useEffect(reload, [])

  const onEncrypt = async (v: any) => {
    try {
      const r = await kmsEncrypt({
        plaintext: v.plaintext,
        context: v.context || '',
        key_id: v.key_id || undefined,
      })
      setEncResult(r.ciphertext)
    } catch (e) {
      message.error(String(e))
    }
  }

  const onDecrypt = async (v: any) => {
    try {
      const r = await kmsDecrypt({
        ciphertext: v.ciphertext,
        context: v.context || '',
      })
      setDecResult(r.plaintext)
    } catch (e) {
      message.error(String(e))
    }
  }

  return (
    <div>
      <Typography.Title level={3}>{t('panel.title')}</Typography.Title>
      <Card
        title={t('panel.masterKeys')}
        loading={loading}
        extra={<Button onClick={reload}>{t('common:actions.refresh')}</Button>}
      >
        <Table<KMSKey>
          rowKey="key_id"
          dataSource={keys}
          pagination={false}
          size="small"
          columns={[
            {
              title: t('panel.columns.keyId'),
              dataIndex: 'key_id',
              render: (v) => (
                <Space>
                  <span>{v}</span>
                  {v === activeID && <Tag color="green">{t('panel.columns.active')}</Tag>}
                </Space>
              ),
            },
            { title: t('panel.columns.algorithm'), dataIndex: 'algorithm' },
            {
              title: t('panel.columns.createdAt'),
              dataIndex: 'created_at',
              render: (v) => (v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'),
            },
          ]}
        />
      </Card>

      <Divider />

      <Row gutter={16}>
        <Col span={12}>
          <Card title={t('panel.encrypt.title')}>
            <Form form={encForm} layout="vertical" onFinish={onEncrypt}>
              <Form.Item
                name="plaintext"
                label={t('panel.encrypt.plaintext')}
                rules={[{ required: true }]}
              >
                <Input.TextArea rows={3} />
              </Form.Item>
              <Form.Item
                name="context"
                label={t('panel.encrypt.context')}
                extra={t('panel.encrypt.contextHint')}
              >
                <Input placeholder={t('panel.encrypt.contextPlaceholder')} />
              </Form.Item>
              <Form.Item name="key_id" label={t('panel.encrypt.keyId')}>
                <Input placeholder={activeID} />
              </Form.Item>
              <Form.Item>
                <Button type="primary" htmlType="submit">
                  {t('panel.encrypt.submit')}
                </Button>
              </Form.Item>
            </Form>
            {encResult && (
              <div>
                <strong>{t('panel.encrypt.resultLabel')}</strong>
                <pre style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {encResult}
                </pre>
              </div>
            )}
          </Card>
        </Col>

        <Col span={12}>
          <Card title={t('panel.decrypt.title')}>
            <Form form={decForm} layout="vertical" onFinish={onDecrypt}>
              <Form.Item
                name="ciphertext"
                label={t('panel.decrypt.ciphertext')}
                rules={[{ required: true }]}
              >
                <Input.TextArea rows={3} placeholder={t('panel.decrypt.ciphertextPlaceholder')} />
              </Form.Item>
              <Form.Item name="context" label={t('panel.decrypt.context')}>
                <Input placeholder={t('panel.decrypt.contextPlaceholder')} />
              </Form.Item>
              <Form.Item>
                <Button type="primary" htmlType="submit">
                  {t('panel.decrypt.submit')}
                </Button>
              </Form.Item>
            </Form>
            {decResult && (
              <div>
                <strong>{t('panel.decrypt.resultLabel')}</strong>
                <pre style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {decResult}
                </pre>
              </div>
            )}
          </Card>
        </Col>
      </Row>
    </div>
  )
}
