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
import dayjs from 'dayjs'
import { kmsDecrypt, kmsEncrypt, listKeys } from '../../api'
import type { KMSKey } from '../../api/types'

export default function KMSPanel() {
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
      <Typography.Title level={3}>密钥管理 (kms-manage)</Typography.Title>
      <Card
        title="Master Keys"
        loading={loading}
        extra={<Button onClick={reload}>刷新</Button>}
      >
        <Table<KMSKey>
          rowKey="key_id"
          dataSource={keys}
          pagination={false}
          size="small"
          columns={[
            {
              title: 'Key ID',
              dataIndex: 'key_id',
              render: (v) => (
                <Space>
                  <span>{v}</span>
                  {v === activeID && <Tag color="green">ACTIVE</Tag>}
                </Space>
              ),
            },
            { title: '算法', dataIndex: 'algorithm' },
            {
              title: '创建时间',
              dataIndex: 'created_at',
              render: (v) => (v ? dayjs(v * 1000).format('YYYY-MM-DD HH:mm:ss') : '—'),
            },
          ]}
        />
      </Card>

      <Divider />

      <Row gutter={16}>
        <Col span={12}>
          <Card title="Encrypt">
            <Form form={encForm} layout="vertical" onFinish={onEncrypt}>
              <Form.Item
                name="plaintext"
                label="明文"
                rules={[{ required: true }]}
              >
                <Input.TextArea rows={3} />
              </Form.Item>
              <Form.Item
                name="context"
                label="AAD Context"
                extra='推荐 "svc:<service>:<field>"，如 svc:paycore:auth_tokens'
              >
                <Input placeholder="svc:paycore:auth_tokens" />
              </Form.Item>
              <Form.Item name="key_id" label="Key ID (留空=active)">
                <Input placeholder={activeID} />
              </Form.Item>
              <Form.Item>
                <Button type="primary" htmlType="submit">
                  加密
                </Button>
              </Form.Item>
            </Form>
            {encResult && (
              <div>
                <strong>密文：</strong>
                <pre style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {encResult}
                </pre>
              </div>
            )}
          </Card>
        </Col>

        <Col span={12}>
          <Card title="Decrypt">
            <Form form={decForm} layout="vertical" onFinish={onDecrypt}>
              <Form.Item
                name="ciphertext"
                label="密文"
                rules={[{ required: true }]}
              >
                <Input.TextArea rows={3} placeholder="kms:v1:main:..." />
              </Form.Item>
              <Form.Item name="context" label="AAD Context">
                <Input placeholder="加密时使用的同一 context" />
              </Form.Item>
              <Form.Item>
                <Button type="primary" htmlType="submit">
                  解密
                </Button>
              </Form.Item>
            </Form>
            {decResult && (
              <div>
                <strong>明文：</strong>
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
