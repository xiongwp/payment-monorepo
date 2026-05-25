import { useCallback, useEffect, useState } from 'react'
import {
  Alert, Button, Card, Col, Descriptions, Divider, Form, Input, Modal, Row, Space, Table, Tag, Typography, message,
} from 'antd'
import { useParams } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
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
// labelKey points at detail.kyc.actions.* in the merchant namespace.
const ACTIONS_BY_STATUS: Record<string, Array<{ action: KycAction; labelKey: string; danger?: boolean }>> = {
  pending:         [{ action: 'submit', labelKey: 'submit' }, { action: 'reject', labelKey: 'reject', danger: true }],
  submitted:       [{ action: 'review', labelKey: 'review' }, { action: 'request_more_info', labelKey: 'requestMoreInfo' }, { action: 'reject', labelKey: 'reject', danger: true }],
  reviewing:       [{ action: 'approve', labelKey: 'approve' }, { action: 'request_more_info', labelKey: 'requestMoreInfo' }, { action: 'reject', labelKey: 'reject', danger: true }],
  needs_more_info: [{ action: 'submit', labelKey: 'resubmit' }, { action: 'reject', labelKey: 'reject', danger: true }],
  approved:        [{ action: 'suspend', labelKey: 'suspend', danger: true }, { action: 'review', labelKey: 'reReview' }, { action: 'terminate', labelKey: 'terminate', danger: true }],
  suspended:       [{ action: 'unsuspend', labelKey: 'unsuspend' }, { action: 'review', labelKey: 'reReview' }, { action: 'terminate', labelKey: 'terminate', danger: true }],
  rejected:        [],
  terminated:      [],
}

export default function MerchantDetail() {
  const { t } = useTranslation('merchant')
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
      message.success(t('detail.kyc.modal.doneMessage', { label: actionModal.label }))
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
        title: t('detail.apiKey.rotatedTitle', { kind: kind.toUpperCase() }),
        width: 640,
        content: (
          <div>
            <Alert type="warning" showIcon style={{ marginBottom: 12 }}
              message={t('detail.apiKey.rotatedAlert')} />
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
      message.success(t('detail.documents.reviewedMessage', { status }))
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  const onUpload = async (v: Partial<MerchantKycDocument>) => {
    try {
      await addMerchantDocument(id, v)
      message.success(t('detail.documents.addedMessage'))
      setUploadOpen(false)
      uploadForm.resetFields()
      load()
    } catch (e) {
      message.error(String(e))
    }
  }

  if (!m) return <Card loading={loading}>{t('detail.loading')}</Card>

  const allowed = ACTIONS_BY_STATUS[m.kyc_status] || []

  return (
    <div>
      <Typography.Title level={3}>
        {t('detail.title', { name: m.name })} <Tag color={KYC_COLORS[m.kyc_status]}>{m.kyc_status}</Tag>
      </Typography.Title>

      <Row gutter={16}>
        <Col span={14}>
          <Card title={t('detail.basicInfo.cardTitle')} loading={loading}>
            <Descriptions column={2} size="small">
              <Descriptions.Item label={t('detail.basicInfo.id')}>{m.id}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.email')}>{m.contact_email}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.country')}>{m.country}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.businessType')}>{m.business_type}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.taxId')}>{m.tax_id || '-'}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.mcc')}>{m.mcc || '-'}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.riskTier')}>{m.risk_tier}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.rateRps')}>{m.rate_limit_rps}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.webhookUrl')} span={2}>{m.webhook_url || '-'}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.settleCurrency')}>{m.settle_currency}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.settleMethod')}>{m.settle_method || '-'}</Descriptions.Item>
              <Descriptions.Item label={t('detail.basicInfo.settleAccount')} span={2}>
                {m.settle_account || '-'}
                {m.settle_bank && ` @ ${m.settle_bank}`}
                {m.settle_holder && t('detail.basicInfo.settleHolderSuffix', { holder: m.settle_holder })}
              </Descriptions.Item>
            </Descriptions>
          </Card>
        </Col>

        <Col span={10}>
          <Card title={t('detail.kyc.cardTitle')} loading={loading}>
            <Space wrap>
              {allowed.length === 0 && <Typography.Text type="secondary">{t('detail.kyc.terminal')}</Typography.Text>}
              {allowed.map((a) => {
                const label = t(`detail.kyc.actions.${a.labelKey}`)
                return (
                  <Button
                    key={a.action}
                    type={a.danger ? 'default' : 'primary'}
                    danger={a.danger}
                    onClick={() => setActionModal({
                      action: a.action,
                      label,
                      idempotencyKey:
                        typeof crypto !== 'undefined' && crypto.randomUUID
                          ? crypto.randomUUID()
                          : `kyc-${Date.now()}-${Math.random().toString(36).slice(2)}`,
                    })}
                  >{label}</Button>
                )
              })}
            </Space>
            {m.kyc_reason && (
              <Alert type="info" style={{ marginTop: 12 }} message={t('detail.kyc.reasonLabel', { reason: m.kyc_reason })} />
            )}
          </Card>
          <Card title={t('detail.apiKey.cardTitle')} style={{ marginTop: 12 }}>
            <Space>
              <Button
                loading={rotatePending === 'live'}
                disabled={rotatePending !== null}
                onClick={() => onRotate('live')}
              >{t('detail.apiKey.resetLive')}</Button>
              <Button
                loading={rotatePending === 'test'}
                disabled={rotatePending !== null}
                onClick={() => onRotate('test')}
              >{t('detail.apiKey.resetTest')}</Button>
            </Space>
          </Card>
          <Card title={t('detail.channelCredentials.cardTitle')} style={{ marginTop: 12 }}>
            <Typography.Paragraph type="secondary" style={{ marginBottom: 8 }}>
              {t('detail.channelCredentials.description')}
            </Typography.Paragraph>
            <a href={`/merchants/${id}/secrets`}>{t('detail.channelCredentials.openLink')}</a>
          </Card>
        </Col>
      </Row>

      <Divider />

      <Card
        title={t('detail.documents.cardTitle')} loading={loading}
        extra={<Button size="small" type="primary" onClick={() => setUploadOpen(true)}>{t('detail.documents.uploadButton')}</Button>}
      >
        <Table<MerchantKycDocument>
          rowKey="id"
          dataSource={docs}
          size="small"
          pagination={false}
          columns={[
            { title: t('detail.documents.columns.id'), dataIndex: 'id', width: 140 },
            { title: t('detail.documents.columns.docType'), dataIndex: 'doc_type', width: 160 },
            { title: t('detail.documents.columns.docNumber'), dataIndex: 'doc_number' },
            { title: t('detail.documents.columns.url'), dataIndex: 'file_url', ellipsis: true, render: (v) => <a href={v} target="_blank">{v}</a> },
            { title: t('detail.documents.columns.review'), dataIndex: 'review_status', width: 100, render: (v: string) => <Tag>{v}</Tag> },
            {
              title: t('detail.documents.columns.actions'), width: 180,
              render: (_, d) => (
                <Space size="small">
                  <Button size="small" onClick={() => onReviewDoc(d.id, 'accepted')}>{t('detail.documents.actions.accept')}</Button>
                  <Button size="small" danger onClick={() => onReviewDoc(d.id, 'rejected')}>{t('detail.documents.actions.reject')}</Button>
                </Space>
              ),
            },
          ]}
        />
      </Card>

      <Divider />

      <Card title={t('detail.audits.cardTitle')} loading={loading}>
        <Table<MerchantKycAudit>
          rowKey="id"
          dataSource={audits}
          size="small"
          pagination={{ pageSize: 10 }}
          columns={[
            { title: t('detail.audits.columns.time'), dataIndex: 'created_ms', width: 170, render: (v: number) => new Date(v).toLocaleString() },
            { title: t('detail.audits.columns.from'), dataIndex: 'from_status', width: 140, render: (v: string) => <Tag color={KYC_COLORS[v]}>{v}</Tag> },
            { title: t('detail.audits.columns.to'), dataIndex: 'to_status', width: 140, render: (v: string) => <Tag color={KYC_COLORS[v]}>{v}</Tag> },
            { title: t('detail.audits.columns.actor'), dataIndex: 'actor', width: 140 },
            { title: t('detail.audits.columns.reason'), dataIndex: 'reason' },
          ]}
        />
      </Card>

      <Modal
        title={actionModal ? t('detail.kyc.modal.titleTemplate', { label: actionModal.label }) : ''}
        open={!!actionModal}
        onCancel={() => { if (!actionPending) setActionModal(null) }}
        onOk={() => actionForm.submit()}
        confirmLoading={actionPending}
        cancelButtonProps={{ disabled: actionPending }}
        maskClosable={!actionPending}
      >
        <Form form={actionForm} layout="vertical" onFinish={doAction}>
          <Form.Item name="actor" label={t('detail.kyc.modal.actorLabel')}>
            <Input placeholder={t('detail.kyc.modal.actorPlaceholder')} />
          </Form.Item>
          <Form.Item name="reason" label={t('detail.kyc.modal.reasonLabel')}>
            <Input.TextArea rows={3} placeholder={t('detail.kyc.modal.reasonPlaceholder')} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('detail.documents.uploadModal.title')}
        open={uploadOpen}
        onCancel={() => setUploadOpen(false)}
        onOk={() => uploadForm.submit()}
        width={600}
      >
        <Alert type="info" style={{ marginBottom: 12 }} showIcon
          message={t('detail.documents.uploadModal.alert')} />
        <Form form={uploadForm} layout="vertical" onFinish={onUpload}>
          <Form.Item name="doc_type" label={t('detail.documents.uploadModal.fields.docType')} rules={[{ required: true }]}>
            <Input placeholder={t('detail.documents.uploadModal.fields.docTypePlaceholder')} />
          </Form.Item>
          <Form.Item name="doc_number" label={t('detail.documents.uploadModal.fields.docNumber')}><Input /></Form.Item>
          <Form.Item name="file_url" label={t('detail.documents.uploadModal.fields.fileUrl')} rules={[{ required: true }]}>
            <Input placeholder={t('detail.documents.uploadModal.fields.fileUrlPlaceholder')} />
          </Form.Item>
          <Form.Item name="mime_type" label={t('detail.documents.uploadModal.fields.mimeType')}><Input /></Form.Item>
          <Form.Item name="uploaded_by" label={t('detail.documents.uploadModal.fields.uploadedBy')}><Input /></Form.Item>
        </Form>
      </Modal>
    </div>
  )
}
