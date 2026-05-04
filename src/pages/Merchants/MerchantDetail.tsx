import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Col, Descriptions, Divider, Form, Input, Modal, Row, Space, Table, Tag, Typography, message,
} from 'antd'
import { useParams } from 'react-router-dom'
import {
  getMerchant, listMerchantAudits, listMerchantDocuments,
  kycTransition, rotateMerchantKey, reviewMerchantDocument,
  addMerchantDocument,
} from '../../api'
import type { Merchant, MerchantKycAudit, MerchantKycDocument, KycAction } from '../../api'

const KYC_COLORS: Record<string, string> = {
  pending: 'default', submitted: 'blue', reviewing: 'geekblue',
  needs_more_info: 'gold', approved: 'green', rejected: 'red',
  suspended: 'orange', terminated: 'red',
}

// ACTIONS_BY_STATUS encodes the FSM allowed-outbound set per state. UI shows
// only the buttons that the backend will accept; keeps ops from seeing a
// "FailedPrecondition" toast on every try.
const ACTIONS_BY_STATUS: Record<string, Array<{ action: KycAction; label: string; danger?: boolean }>> = {
  pending:         [{ action: 'submit', label: '提交审核' }, { action: 'reject', label: '驳回', danger: true }],
  submitted:       [{ action: 'review', label: '开始审核' }, { action: 'request_more_info', label: '需补材料' }, { action: 'reject', label: '驳回', danger: true }],
  reviewing:       [{ action: 'approve', label: '通过' }, { action: 'request_more_info', label: '需补材料' }, { action: 'reject', label: '驳回', danger: true }],
  needs_more_info: [{ action: 'submit', label: '重新提交' }, { action: 'reject', label: '驳回', danger: true }],
  approved:        [{ action: 'suspend', label: '暂停', danger: true }, { action: 'review', label: '重新核查' }, { action: 'terminate', label: '终止', danger: true }],
  suspended:       [{ action: 'unsuspend', label: '恢复' }, { action: 'review', label: '重新核查' }, { action: 'terminate', label: '终止', danger: true }],
  rejected:        [],
  terminated:      [],
}

export default function MerchantDetail() {
  const { id = '' } = useParams()
  const [m, setM] = useState<Merchant | null>(null)
  const [audits, setAudits] = useState<MerchantKycAudit[]>([])
  const [docs, setDocs] = useState<MerchantKycDocument[]>([])
  const [loading, setLoading] = useState(false)
  // actionModal 打开时同时生成一把 idempotency_key，整个 modal 生命期内
  // 复用同一个 key——operator 多点几次"提交"也只触发一次后端真实操作
  // （BFF idempotencyCache 5min 兜底）。modal 关闭即下次重开时重置。
  const [actionModal, setActionModal] = useState<
    { action: KycAction; label: string; idempotencyKey: string } | null
  >(null)
  const [actionPending, setActionPending] = useState(false)
  const [rotatePending, setRotatePending] = useState<'live' | 'test' | null>(null)
  const [actionForm] = Form.useForm()
  const [uploadOpen, setUploadOpen] = useState(false)
  const [uploadForm] = Form.useForm()

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [merchant, auditList, docList] = await Promise.all([
        getMerchant(id), listMerchantAudits(id), listMerchantDocuments(id),
      ])
      setM(merchant)
      setAudits(auditList || [])
      setDocs(docList || [])
    } catch (e) {
      message.error(String(e))
    } finally {
      setLoading(false)
    }
  }, [id])

  useEffect(() => { load() }, [load])

  const doAction = async (v: { actor?: string; reason?: string }) => {
    if (!actionModal || actionPending) return
    setActionPending(true)
    try {
      const updated = await kycTransition(id, actionModal.action, {
        ...v,
        idempotencyKey: actionModal.idempotencyKey, // freeze on modal open
      })
      setM(updated)
      message.success(`已${actionModal.label}`)
      setActionModal(null)
      actionForm.resetFields()
      load()
    } catch (e) {
      message.error(String(e))
    } finally {
      setActionPending(false)
    }
  }

  const onRotate = async (kind: 'live' | 'test') => {
    if (rotatePending) return // 已有 rotate 进行中，忽略二次点击
    setRotatePending(kind)
    // 整轮 rotate 期间 freeze 同一把 idempotency_key（BFF 端 5min cache 兜底
    // 防止 axios retry / network blip 造成的双签发）。
    const idemKey = (typeof crypto !== 'undefined' && crypto.randomUUID)
      ? crypto.randomUUID()
      : `rk-${Date.now()}-${Math.random().toString(36).slice(2)}`
    try {
      const r = await rotateMerchantKey(id, kind, idemKey)
      Modal.info({
        title: `${kind.toUpperCase()} key 已轮换`,
        width: 640,
        content: (
          <div>
            <Alert type="warning" showIcon style={{ marginBottom: 12 }}
              message="只显示一次，请立即保存。" />
            <Input.TextArea value={r.plaintext} readOnly autoSize
              style={{ fontFamily: 'monospace' }} />
          </div>
        ),
      })
    } catch (e) {
      message.error(String(e))
    } finally {
      setRotatePending(null)
    }
  }

  const onReviewDoc = async (docID: string, status: string) => {
    try {
      await reviewMerchantDocument(docID, { status })
      message.success(`文档已标记为 ${status}`)
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  const onUpload = async (v: Partial<MerchantKycDocument>) => {
    try {
      await addMerchantDocument(id, v)
      message.success('已新增文档')
      setUploadOpen(false)
      uploadForm.resetFields()
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  if (!m) return <Card loading={loading}>loading...</Card>

  const allowed = ACTIONS_BY_STATUS[m.kyc_status] || []

  return (
    <div>
      <Typography.Title level={3}>
        商户详情 · {m.name} <Tag color={KYC_COLORS[m.kyc_status]}>{m.kyc_status}</Tag>
      </Typography.Title>

      <Row gutter={16}>
        <Col span={14}>
          <Card title="基本信息" loading={loading}>
            <Descriptions column={2} size="small">
              <Descriptions.Item label="ID">{m.id}</Descriptions.Item>
              <Descriptions.Item label="邮箱">{m.contact_email}</Descriptions.Item>
              <Descriptions.Item label="国家">{m.country}</Descriptions.Item>
              <Descriptions.Item label="主体">{m.business_type}</Descriptions.Item>
              <Descriptions.Item label="TIN/SEC">{m.tax_id || '-'}</Descriptions.Item>
              <Descriptions.Item label="MCC">{m.mcc || '-'}</Descriptions.Item>
              <Descriptions.Item label="风控分层">{m.risk_tier}</Descriptions.Item>
              <Descriptions.Item label="Rate RPS">{m.rate_limit_rps}</Descriptions.Item>
              <Descriptions.Item label="Webhook URL" span={2}>{m.webhook_url || '-'}</Descriptions.Item>
              <Descriptions.Item label="结算币种">{m.settle_currency}</Descriptions.Item>
              <Descriptions.Item label="结算方式">{m.settle_method || '-'}</Descriptions.Item>
              <Descriptions.Item label="结算账户" span={2}>
                {m.settle_account || '-'}
                {m.settle_bank && ` @ ${m.settle_bank}`}
                {m.settle_holder && `（${m.settle_holder}）`}
              </Descriptions.Item>
            </Descriptions>
          </Card>
        </Col>

        <Col span={10}>
          <Card title="KYC 操作" loading={loading}>
            <Space wrap>
              {allowed.length === 0 && <Typography.Text type="secondary">终态，无可用操作</Typography.Text>}
              {allowed.map((a) => (
                <Button
                  key={a.action}
                  type={a.danger ? 'default' : 'primary'}
                  danger={a.danger}
                  onClick={() => setActionModal({
                    ...a,
                    idempotencyKey:
                      typeof crypto !== 'undefined' && crypto.randomUUID
                        ? crypto.randomUUID()
                        : `kyc-${Date.now()}-${Math.random().toString(36).slice(2)}`,
                  })}
                >{a.label}</Button>
              ))}
            </Space>
            {m.kyc_reason && (
              <Alert type="info" style={{ marginTop: 12 }} message={`当前原因：${m.kyc_reason}`} />
            )}
          </Card>
          <Card title="API Key 轮换" style={{ marginTop: 12 }}>
            <Space>
              <Button
                loading={rotatePending === 'live'}
                disabled={rotatePending !== null}
                onClick={() => onRotate('live')}
              >重置 Live Key</Button>
              <Button
                loading={rotatePending === 'test'}
                disabled={rotatePending !== null}
                onClick={() => onRotate('test')}
              >重置 Test Key</Button>
            </Space>
          </Card>
          <Card title="渠道凭据" style={{ marginTop: 12 }}>
            <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
              每家 PH 渠道的 partner_id / signing_key / webhook_secret 加密存放，支持多商户隔离。
            </Typography.Paragraph>
            <a href={`/merchants/${id}/secrets`}>打开渠道凭据管理 →</a>
          </Card>
        </Col>
      </Row>

      <Divider />

      <Card
        title="KYC 文档" loading={loading}
        extra={<Button size="small" type="primary" onClick={() => setUploadOpen(true)}>上传文档</Button>}
      >
        <Table<MerchantKycDocument>
          rowKey="id"
          dataSource={docs}
          size="small"
          pagination={false}
          columns={[
            { title: 'ID', dataIndex: 'id', width: 140 },
            { title: '类型', dataIndex: 'doc_type', width: 160 },
            { title: '编号', dataIndex: 'doc_number' },
            { title: 'URL', dataIndex: 'file_url', ellipsis: true, render: (v) => <a href={v} target="_blank">{v}</a> },
            { title: '审核', dataIndex: 'review_status', width: 100, render: (v: string) => <Tag>{v}</Tag> },
            {
              title: '操作', width: 180,
              render: (_, d) => (
                <Space size="small">
                  <Button size="small" onClick={() => onReviewDoc(d.id, 'accepted')}>通过</Button>
                  <Button size="small" danger onClick={() => onReviewDoc(d.id, 'rejected')}>拒绝</Button>
                </Space>
              ),
            },
          ]}
        />
      </Card>

      <Divider />

      <Card title="KYC 审计流转日志" loading={loading}>
        <Table<MerchantKycAudit>
          rowKey="id"
          dataSource={audits}
          size="small"
          pagination={{ pageSize: 10 }}
          columns={[
            { title: '时间', dataIndex: 'created_ms', width: 170, render: (v: number) => new Date(v).toLocaleString() },
            { title: 'From', dataIndex: 'from_status', width: 140, render: (v: string) => <Tag color={KYC_COLORS[v]}>{v}</Tag> },
            { title: 'To', dataIndex: 'to_status', width: 140, render: (v: string) => <Tag color={KYC_COLORS[v]}>{v}</Tag> },
            { title: 'Actor', dataIndex: 'actor', width: 140 },
            { title: '原因', dataIndex: 'reason' },
          ]}
        />
      </Card>

      <Modal
        title={actionModal ? `KYC · ${actionModal.label}` : ''}
        open={!!actionModal}
        onCancel={() => { if (!actionPending) setActionModal(null) }}
        onOk={() => actionForm.submit()}
        confirmLoading={actionPending}
        cancelButtonProps={{ disabled: actionPending }}
        maskClosable={!actionPending}
      >
        <Form form={actionForm} layout="vertical" onFinish={doAction}>
          <Form.Item name="actor" label="操作人（admin user）">
            <Input placeholder="你的 admin ID" />
          </Form.Item>
          <Form.Item name="reason" label="原因">
            <Input.TextArea rows={3} placeholder="选填。驳回/补材料/暂停/终止建议填" />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title="新增 KYC 文档"
        open={uploadOpen}
        onCancel={() => setUploadOpen(false)}
        onOk={() => uploadForm.submit()}
        width={600}
      >
        <Alert type="info" style={{ marginBottom: 12 }} showIcon
          message="文档正文需先上传到对象存储（S3/R2/GCS），这里只落元信息 + 访问 URL。" />
        <Form form={uploadForm} layout="vertical" onFinish={onUpload}>
          <Form.Item name="doc_type" label="类型" rules={[{ required: true }]}>
            <Input placeholder="gov_id / sec_registration / dti / bir_2303 / bank_statement / utility_bill" />
          </Form.Item>
          <Form.Item name="doc_number" label="编号"><Input /></Form.Item>
          <Form.Item name="file_url" label="文件 URL" rules={[{ required: true }]}>
            <Input placeholder="https://s3..." />
          </Form.Item>
          <Form.Item name="mime_type" label="MIME"><Input /></Form.Item>
          <Form.Item name="uploaded_by" label="上传者"><Input /></Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
